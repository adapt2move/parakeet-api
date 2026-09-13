import time
from concurrent.futures import ThreadPoolExecutor
from dataclasses import replace

import pytest
from fastapi import HTTPException

from parakeet_api.store import Store


def submit(store):
    uid = store.reserve()
    (store.blobs / uid).write_bytes(b"audio")
    store.upload_ready(uid, 5)
    return store.submit(uid, {})


def test_claim_is_exclusive_and_recovers_after_restart(settings, result):
    store = Store(settings)
    job = submit(store)
    with ThreadPoolExecutor(8) as pool:
        claims = list(pool.map(lambda _: store.claim(), range(8)))
    assert sum(c is not None for c in claims) == 1
    first = next(c for c in claims if c)
    with store.connect() as db:
        db.execute("UPDATE jobs SET lease=0 WHERE id=?", (job["id"],))
    restarted = Store(settings)
    second = restarted.claim()
    assert second["id"] == first["id"] and second["token"] != first["token"]
    with pytest.raises(HTTPException) as stale:
        store.finish(first["id"], first["token"], result)
    assert stale.value.status_code == 409
    restarted.finish(second["id"], second["token"], result)
    assert store.get(job["id"])["status"] == "completed"


def test_retry_exhaustion(settings):
    store = Store(replace(settings, attempts=2))
    job = submit(store)
    for _ in range(2):
        claim = store.claim()
        store.finish(claim["id"], claim["token"], error="Transient", retry=True)
    assert store.claim() is None
    assert store.get(job["id"])["status"] == "error"


def test_queue_and_storage_are_bounded(settings):
    store = Store(replace(settings, queue_size=1))
    job = submit(store)
    assert store.submit(job["upload_id"], {})["id"] == job["id"]
    uid = store.reserve()
    store.upload_ready(uid, 5)
    with pytest.raises(HTTPException) as full:
        store.submit(uid, {})
    assert full.value.status_code == 429
    for _ in range(3):
        store.reserve()
    with pytest.raises(HTTPException):
        store.reserve()


def test_cancel_fences_worker_and_cleans_audio(settings):
    store = Store(settings)
    job = submit(store)
    claim = store.claim()
    store.delete(job["id"])
    with pytest.raises(HTTPException):
        store.heartbeat(job["id"], claim["token"])
    assert not list(store.blobs.iterdir())


def test_retention_and_job_age(settings, result):
    store = Store(settings)
    job = submit(store)
    claim = store.claim()
    store.finish(job["id"], claim["token"], result)
    with store.connect() as db:
        db.execute("UPDATE jobs SET updated=0")
    store.cleanup()
    assert not list(store.blobs.iterdir())
    job = submit(store)
    with store.connect() as db:
        db.execute("UPDATE jobs SET created=?", (time.time() - settings.max_job_age - 1,))
    store.cleanup()
    assert store.get(job["id"])["status"] == "error"


def test_cleanup_removes_crash_orphans_but_preserves_recent_files(settings):
    import os

    store = Store(settings)
    stale = store.blobs / "crash-orphan"
    stale.write_bytes(b"audio")
    os.utime(stale, (0, 0))
    recent = store.blobs / "recent-upload"
    recent.write_bytes(b"audio")
    store.cleanup()
    assert not stale.exists() and recent.exists()
