"""SQLite queue with fenced leases, bounded uploads and short transactions."""

import json
import sqlite3
import time
import uuid
from contextlib import contextmanager

from fastapi import HTTPException


class Store:
    def __init__(self, settings):
        self.s = settings
        self.s.data.mkdir(parents=True, exist_ok=True)
        self.blobs = self.s.data / "audio"
        self.blobs.mkdir(exist_ok=True)
        self.path = self.s.data / "jobs.sqlite3"
        with self.connect() as db:
            db.execute("PRAGMA journal_mode=WAL")
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
        db = sqlite3.connect(self.path, timeout=10)
        db.row_factory = sqlite3.Row
        db.execute("PRAGMA foreign_keys=ON")
        db.execute("PRAGMA secure_delete=ON")
        try:
            with db:
                yield db
        finally:
            db.close()

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
        with self.connect() as db:
            db.execute("BEGIN IMMEDIATE")
            db.execute(
                """UPDATE jobs SET status=CASE WHEN attempts>=? THEN 'error' ELSE 'queued' END,
                       error=CASE WHEN attempts>=? THEN 'Worker lease expired' ELSE NULL END,
                       token=NULL,lease=NULL,updated=? WHERE status='processing' AND lease<=?""",
                (self.s.attempts, self.s.attempts, now, now),
            )
            row = db.execute("SELECT * FROM jobs WHERE status='queued' ORDER BY created LIMIT 1").fetchone()
            if not row:
                return None
            token = str(uuid.uuid4())
            db.execute(
                "UPDATE jobs SET status='processing',token=?,lease=?,attempts=attempts+1,updated=? WHERE id=?",
                (token, now + self.s.lease, now, row["id"]),
            )
            return {
                "id": row["id"],
                "token": token,
                "lease_seconds": self.s.lease,
                "options": json.loads(row["options"]),
            }

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
            return self.blobs / row["upload_id"]

    def finish(self, jid, token, result=None, error=None, retry=False):
        with self.connect() as db:
            db.execute("BEGIN IMMEDIATE")
            row = self.fenced(db, jid, token)
            status = (
                "queued" if retry and row["attempts"] < self.s.attempts else "error" if error else "completed"
            )
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
        # Keep upload until job expiry: retries of submit stay idempotent.

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
        for uid in [r[1] for r in expired] + [r[0] for r in unused]:
            (self.blobs / uid).unlink(missing_ok=True)
        with self.connect() as db:
            db.execute("PRAGMA wal_checkpoint(TRUNCATE)")
            known = {row[0] for row in db.execute("SELECT id FROM uploads")}
        # Recover files left between committing a deletion and unlinking audio.
        for path in self.blobs.iterdir():
            if path.name not in known and path.stat().st_mtime < now - 3700:
                path.unlink(missing_ok=True)

    def counts(self):
        with self.connect() as db:
            return dict(db.execute("SELECT status,count(*) FROM jobs GROUP BY status").fetchall())
