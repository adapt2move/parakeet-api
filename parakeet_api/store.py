"""SQLite queue with fenced leases, bounded uploads and short transactions."""

import json
import sqlite3
import threading
import time
import uuid
from contextlib import contextmanager, suppress

from fastapi import HTTPException


class Store:
    def __init__(self, settings):
        self.s = settings
        self.s.data.mkdir(parents=True, exist_ok=True)
        self.blobs = self.s.data / "audio"
        self.blobs.mkdir(exist_ok=True)
        self.path = self.s.data / "jobs.sqlite3"
        if self.s.db_mode not in ("memory", "file"):
            raise ValueError("DB_MODE must be memory or file")
        self.lock = threading.RLock()
        self.memory = None
        if self.s.db_mode == "memory":
            if self.path.exists():
                raise ValueError(
                    "Use a fresh DATA_DIR for memory mode, or DB_MODE=file for existing SQLite data"
                )
            # This directory belongs exclusively to this API instance. A memory
            # queue cannot recover uploads left by its previous process.
            for path in self.blobs.iterdir():
                path.unlink()
            self.memory = sqlite3.connect(":memory:", check_same_thread=False)
        with self.connect() as db:
            db.execute("PRAGMA journal_mode=" + ("MEMORY" if self.memory is not None else "WAL"))
            db.execute("PRAGMA secure_delete=ON")
            version = db.execute("PRAGMA user_version").fetchone()[0]
            if version not in (0, 1):
                raise RuntimeError("Unsupported database schema")
            db.executescript("""
                CREATE TABLE IF NOT EXISTS uploads (
                    id TEXT PRIMARY KEY, bytes INTEGER NOT NULL,
                    ready INTEGER NOT NULL DEFAULT 0, created REAL NOT NULL);
                CREATE TABLE IF NOT EXISTS jobs (
                    id TEXT PRIMARY KEY, upload_id TEXT NOT NULL UNIQUE REFERENCES uploads(id),
                    status TEXT NOT NULL, options TEXT NOT NULL, result TEXT, error TEXT,
                    created REAL NOT NULL, updated REAL NOT NULL,
                    token TEXT, lease REAL, attempts INTEGER NOT NULL DEFAULT 0);
                CREATE INDEX IF NOT EXISTS queue ON jobs(status, created);
                PRAGMA user_version=1;
            """)

    @contextmanager
    def connect(self):
        # One long-lived connection keeps :memory: alive. Serialize transactions
        # across the HTTP event loop and cleanup thread without shared-cache mode.
        with self.lock:
            db = self.memory if self.memory is not None else sqlite3.connect(self.path, timeout=10)
            db.row_factory = sqlite3.Row
            db.execute("PRAGMA foreign_keys=ON")
            db.execute("PRAGMA secure_delete=ON")
            db.execute("PRAGMA temp_store=MEMORY")
            try:
                with db:
                    yield db
            finally:
                if self.memory is None:
                    db.close()

    def close(self):
        with self.lock:
            if self.memory is not None:
                self.memory.close()

    def reserve(self):
        with self.connect() as db:
            db.execute("BEGIN IMMEDIATE")
            used = db.execute("SELECT coalesce(sum(bytes),0) FROM uploads").fetchone()[0]
            if used + self.s.max_bytes > self.s.storage_bytes:
                raise HTTPException(429, "Audio storage full", headers={"Retry-After": "30"})
            uid = str(uuid.uuid4())
            db.execute(
                "INSERT INTO uploads(id,bytes,created) VALUES(?,?,?)", (uid, self.s.max_bytes, time.time())
            )
            return uid

    def upload_ready(self, uid, size):
        with self.connect() as db:
            db.execute("UPDATE uploads SET ready=1,bytes=?,created=? WHERE id=?", (size, time.time(), uid))

    def discard_upload(self, uid):
        with self.connect() as db:
            db.execute(
                "DELETE FROM uploads WHERE id=? AND NOT EXISTS(SELECT 1 FROM jobs WHERE upload_id=?)",
                (uid, uid),
            )
            remaining = db.execute("SELECT 1 FROM uploads WHERE id=?", (uid,)).fetchone()
        if remaining is None:
            (self.blobs / uid).unlink(missing_ok=True)

    def submit(self, uid, options):
        with self.connect() as db:
            db.execute("BEGIN IMMEDIATE")
            existing = db.execute("SELECT * FROM jobs WHERE upload_id=?", (uid,)).fetchone()
            if existing:
                if json.loads(existing["options"]) != options:
                    raise HTTPException(409, "Upload already submitted with different options")
                return dict(existing)
            upload = db.execute("SELECT * FROM uploads WHERE id=? AND ready=1", (uid,)).fetchone()
            if not upload:
                raise HTTPException(400, "Upload missing or expired")
            count = db.execute(
                "SELECT count(*) FROM jobs WHERE status IN ('queued','processing')"
            ).fetchone()[0]
            if count >= self.s.queue_size:
                raise HTTPException(429, "Queue full", headers={"Retry-After": "10"})
            jid, now = str(uuid.uuid4()), time.time()
            db.execute(
                "INSERT INTO jobs(id,upload_id,status,options,created,updated) VALUES(?,?,'queued',?,?,?)",
                (jid, uid, json.dumps(options), now, now),
            )
        return self.get(jid)

    def get(self, jid):
        with self.connect() as db:
            row = db.execute("SELECT * FROM jobs WHERE id=?", (jid,)).fetchone()
        if not row:
            raise HTTPException(404, "Transcript missing or expired")
        return dict(row)

    def claim(self):
        now = time.time()
        claimed = None
        with self.connect() as db:
            db.execute("BEGIN IMMEDIATE")
            db.execute(
                """UPDATE jobs SET status=CASE WHEN attempts>=? THEN 'error' ELSE 'queued' END,
                       error=CASE WHEN attempts>=? THEN 'Worker lease expired' ELSE NULL END,
                       token=NULL,lease=NULL,updated=? WHERE status='processing' AND lease<=?""",
                (self.s.attempts, self.s.attempts, now, now),
            )
            finished = self.release_audio(db)
            row = db.execute(
                """SELECT jobs.*, uploads.bytes FROM jobs JOIN uploads ON uploads.id=jobs.upload_id
                       WHERE status='queued' ORDER BY jobs.created LIMIT 1"""
            ).fetchone()
            if row:
                token = str(uuid.uuid4())
                db.execute(
                    "UPDATE jobs SET status='processing',token=?,lease=?,attempts=attempts+1,updated=? WHERE id=?",
                    (token, now + self.s.lease, now, row["id"]),
                )
                claimed = {
                    "id": row["id"],
                    "token": token,
                    "lease_seconds": self.s.lease,
                    "bytes": row["bytes"],
                    "options": json.loads(row["options"]),
                }
        self.unlink(finished)
        return claimed

    def release_audio(self, db):
        # Finished jobs never read audio again. Their upload rows stay, so submit remains idempotent.
        ids = [
            row[0]
            for row in db.execute(
                """SELECT uploads.id FROM uploads JOIN jobs ON jobs.upload_id=uploads.id
                       WHERE jobs.status IN ('completed','error') AND uploads.bytes>0"""
            )
        ]
        db.executemany("UPDATE uploads SET bytes=0 WHERE id=?", [(uid,) for uid in ids])
        return ids

    def unlink(self, ids):
        # Only after commit: a crash in between leaves a file that cleanup removes later.
        for uid in ids:
            (self.blobs / uid).unlink(missing_ok=True)

    def fenced(self, db, jid, token):
        row = db.execute(
            "SELECT * FROM jobs WHERE id=? AND status='processing' AND token=? AND lease>?",
            (jid, token, time.time()),
        ).fetchone()
        if not row:
            raise HTTPException(409, "Lease expired or job cancelled")
        return row

    def heartbeat(self, jid, token):
        with self.connect() as db:
            db.execute("BEGIN IMMEDIATE")
            self.fenced(db, jid, token)
            db.execute("UPDATE jobs SET lease=? WHERE id=?", (time.time() + self.s.lease, jid))

    def audio(self, jid, token):
        with self.connect() as db:
            row = self.fenced(db, jid, token)
        path = self.blobs / row["upload_id"]
        if not path.is_file():
            raise HTTPException(409, "Audio no longer available")
        return path

    def finish(self, jid, token, result=None, error=None, retry=False):
        with self.connect() as db:
            db.execute("BEGIN IMMEDIATE")
            row = self.fenced(db, jid, token)
            if result is not None:
                status = "completed"
            else:
                status = "queued" if retry and row["attempts"] < self.s.attempts else "error"
            db.execute(
                "UPDATE jobs SET status=?,result=?,error=?,updated=?,token=NULL,lease=NULL WHERE id=?",
                (
                    status,
                    json.dumps(result) if result is not None else None,
                    None if status == "queued" else error,
                    time.time(),
                    jid,
                ),
            )
            finished = self.release_audio(db)
        self.unlink(finished)

    def delete(self, jid):
        with self.connect() as db:
            db.execute("BEGIN IMMEDIATE")
            row = db.execute("SELECT upload_id FROM jobs WHERE id=?", (jid,)).fetchone()
            if not row:
                raise HTTPException(404, "Transcript missing or expired")
            db.execute("DELETE FROM jobs WHERE id=?", (jid,))
            db.execute("DELETE FROM uploads WHERE id=?", (row[0],))
        (self.blobs / row[0]).unlink(missing_ok=True)

    def cleanup(self):
        now = time.time()
        with self.connect() as db:
            db.execute("BEGIN IMMEDIATE")
            db.execute(
                """UPDATE jobs SET status='error',error='Job age limit exceeded',updated=?,token=NULL,lease=NULL
                       WHERE status IN ('queued','processing') AND created<?""",
                (now, now - self.s.max_job_age),
            )
            finished = self.release_audio(db)
            expired = db.execute(
                "SELECT id,upload_id FROM jobs WHERE status IN ('completed','error') AND updated<?",
                (now - self.s.retention,),
            ).fetchall()
            for row in expired:
                db.execute("DELETE FROM jobs WHERE id=?", (row[0],))
                db.execute("DELETE FROM uploads WHERE id=?", (row[1],))
            unused = db.execute(
                """SELECT id FROM uploads WHERE created<? AND
                                NOT EXISTS(SELECT 1 FROM jobs WHERE upload_id=uploads.id)""",
                (now - max(self.s.retention, 3700),),
            ).fetchall()
            for row in unused:
                db.execute("DELETE FROM uploads WHERE id=?", (row[0],))
        self.unlink(finished + [r[1] for r in expired] + [r[0] for r in unused])
        with self.connect() as db:
            db.execute("PRAGMA wal_checkpoint(TRUNCATE)")
            # Finished uploads have bytes=0; their audio file is no longer needed.
            known = {row[0] for row in db.execute("SELECT id FROM uploads WHERE bytes>0")}
        # Recover files left between committing a deletion and unlinking audio.
        for path in self.blobs.iterdir():
            with suppress(FileNotFoundError):
                if path.name not in known and path.stat().st_mtime < now - 3700:
                    path.unlink()

    def counts(self):
        with self.connect() as db:
            return dict(db.execute("SELECT status,count(*) FROM jobs GROUP BY status").fetchall())
