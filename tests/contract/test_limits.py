"""Request size, upload slot, transfer rate, storage and queue limits."""

import json
import time
from concurrent.futures import ThreadPoolExecutor

import pytest

from .support import WORKER_KEY, RawRequest, error_message, multipart, wait_until

UPLOAD_LIMIT = 4096 + 65536
V1 = "/v1/audio/transcriptions"


def declared(server, method, path, length, authorization=None, **extra):
    """Send only headers that declare a body length; the server must answer without reading it."""
    kwargs = {} if authorization is None else {"authorization": authorization}
    request = RawRequest(server, method, path, {"Content-Length": str(length), **extra}, **kwargs)
    try:
        return request.response()
    finally:
        request.close()


@pytest.mark.parametrize(
    ("method", "path", "limit", "authorization"),
    [
        ("POST", "/v2/upload", UPLOAD_LIMIT, None),
        ("POST", V1, UPLOAD_LIMIT, None),
        ("POST", "/v2/transcript", 65536, None),
        ("GET", "/metrics", 65536, None),
        ("POST", "/internal/jobs/claim", 65536, WORKER_KEY),
        ("POST", "/internal/jobs/00000000-0000-4000-8000-000000000000/heartbeat", 65536, WORKER_KEY),
        (
            "POST",
            "/internal/jobs/00000000-0000-4000-8000-000000000000/complete",
            16 * 1024 * 1024,
            WORKER_KEY,
        ),
    ],
)
def test_declared_content_length_limits(api, method, path, limit, authorization):
    files = api.audio_files()
    status, headers, body = declared(api, method, path, limit + 1, authorization)
    assert status == 413
    # Middleware rejections use the plain error shape on every path, including /v1.
    assert json.loads(body) == {"error": "Request too large"}
    assert api.audio_files() == files


def test_bodies_within_the_limits_are_accepted(api, result):
    url, _ = api.upload(b"x" * 4096)
    job = api.submit(url)
    claim = api.claim()
    # A large completion body above the general 64 KiB cap reaches validation.
    padded = {**result, "text": "Hello world." + " " * 70000}
    response = api.complete(claim, {"result": padded})
    assert response.status_code == 422
    api.finish(claim)
    assert api.client.get(f"/v2/transcript/{job['id']}").json()["status"] == "completed"


def test_streamed_bodies_are_limited(api):
    files = api.audio_files()

    def chunks(total, size=8192):
        for offset in range(0, total, size):
            yield b"x" * min(size, total - offset)

    upload = api.client.post("/v2/upload", content=chunks(4097))
    assert upload.status_code == 413
    assert error_message(upload) == "Audio too large"
    oversized = api.client.post(
        "/v2/transcript", content=chunks(70000), headers={"content-type": "application/json"}
    )
    assert oversized.status_code == 413
    streamed = api.client.post("/v2/upload", content=chunks(4096))
    assert streamed.status_code == 200
    assert len(api.audio_files() - files) == 1


def test_upload_size_limits(api):
    files = api.audio_files()
    too_large = api.client.post("/v2/upload", content=b"x" * 4097)
    assert too_large.status_code == 413
    assert error_message(too_large) == "Audio too large"
    assert api.audio_files() == files
    _, uid = api.upload(b"x" * 4096)
    assert (api.audio / uid).stat().st_size == 4096


def test_empty_upload_is_rejected(api):
    files = api.audio_files()
    response = api.client.post("/v2/upload", content=b"")
    assert response.status_code == 400
    assert error_message(response) == "Audio is empty"
    body, headers = multipart([("file", b"", "a.wav")])
    response = api.client.post(V1, content=body, headers=headers)
    assert response.status_code == 400
    assert error_message(response, v1=True) == "Audio is empty"
    assert api.audio_files() == files


@pytest.fixture(scope="module")
def slow(launcher):
    """One upload slot and a short idle allowance, so slot and pacing rules are quick to observe."""
    instance = launcher.start(
        MAX_CONCURRENT_UPLOADS=1, UPLOAD_IDLE_SECONDS=1, MIN_UPLOAD_BYTES_PER_SECOND=1000
    )
    yield instance
    instance.stop()


def upload_after_release(server, data=b"audio"):
    # The slot is released right after the response; allow the server a moment to get there.
    response = wait_until(
        lambda: (r := server.client.post("/v2/upload", content=data)).status_code != 429 and r, 3
    )
    assert response and response.status_code == 200, response and response.text
    return response.json()["upload_url"].rsplit("/", 1)[1]


def test_upload_slots_full_returns_429(slow):
    files = slow.audio_files()
    holder = RawRequest(slow, "POST", "/v2/upload", {"Content-Length": "10"})
    try:
        assert holder.send(b"12345")
        time.sleep(0.2)
        for response in (
            slow.client.post("/v2/upload", content=b"audio"),
            slow.client.post(V1, files={"file": ("a.wav", b"audio")}),
        ):
            assert response.status_code == 429
            assert response.headers["retry-after"] == "5"
            assert response.json() == {"error": "Upload slots full"}
        # Only uploads compete for slots.
        assert slow.client.get("/metrics").status_code == 200
        assert (
            slow.client.post("/v2/transcript", json={"audio_url": "http://audio.invalid/"}).status_code == 400
        )
        assert holder.send(b"67890")
        status, _, body = holder.response()
        assert status == 200, body
    finally:
        holder.close()
    upload_after_release(slow)
    assert len(slow.audio_files() - files) == 2


def trickle(server, path, headers, body, interval):
    """Send the first byte at once and the rest one byte per interval; None stalls after the first byte."""
    request = RawRequest(server, "POST", path, {"Content-Length": str(len(body)), **headers})
    try:
        started = time.monotonic()
        request.send(body[:1])
        if interval is None:
            request.responded(10)
        else:
            for offset in range(1, len(body)):
                if request.responded(interval) or not request.send(body[offset : offset + 1]):
                    break
        status, _, response = request.response()
        return status, json.loads(response), time.monotonic() - started
    finally:
        request.close()


def test_trickling_upload_times_out(slow):
    files = slow.audio_files()
    status, body, elapsed = trickle(slow, "/v2/upload", {}, b"x" * 1000, 0.05)
    assert status == 408
    assert body == {"error": "Upload too slow"}
    assert elapsed < 5
    assert slow.audio_files() == files
    uid = upload_after_release(slow)
    assert slow.audio_files() - files == {uid}


def test_stalled_upload_times_out(slow):
    files = slow.audio_files()
    status, body, elapsed = trickle(slow, "/v2/upload", {}, b"x" * 1000, None)
    assert status == 408
    assert body == {"error": "Upload stalled"}
    assert 0.5 < elapsed < 5
    assert slow.audio_files() == files
    uid = upload_after_release(slow)
    assert slow.audio_files() - files == {uid}


def test_trickling_v1_upload_times_out(slow):
    files = slow.audio_files()
    body, headers = multipart([("file", b"x" * 900, "a.wav")])
    status, error, _ = trickle(slow, V1, headers, body, 0.05)
    assert status == 408
    assert error["error"]
    assert slow.audio_files() == files
    upload_after_release(slow)


def test_queued_v1_requests_do_not_hold_upload_slots(slow):
    with ThreadPoolExecutor(3) as pool:
        pending = []
        for expected in range(1, 4):
            pending.append(pool.submit(slow.client.post, V1, files={"file": ("a.wav", b"audio")}))
            assert wait_until(lambda: slow.metrics().get("queued") == expected, 10), slow.metrics()
        for _ in range(3):
            slow.finish(slow.claim())
        responses = [p.result(timeout=30) for p in pending]
    assert [r.status_code for r in responses] == [200, 200, 200]
    assert all(r.json() == {"text": "Hello world."} for r in responses)


def test_storage_reservation_returns_429(start):
    server = start(MAX_UPLOAD_BYTES=1024, MAX_STORAGE_BYTES=2048)
    first, first_uid = server.upload(b"x" * 1024)
    server.upload(b"y" * 1024)
    for response in (
        server.client.post("/v2/upload", content=b"audio"),
        server.client.post(V1, files={"file": ("a.wav", b"audio")}),
    ):
        assert response.status_code == 429
        assert response.headers["retry-after"] == "30"
    assert error_message(server.client.post("/v2/upload", content=b"audio")) == "Audio storage full"
    v1 = server.client.post(V1, files={"file": ("a.wav", b"audio")})
    assert error_message(v1, v1=True) == "Audio storage full"
    assert len(server.audio_files()) == 2

    # Finished jobs give their reservation back.
    job = server.submit(first)
    server.finish(server.claim())
    assert first_uid not in server.audio_files()
    server.upload(b"z" * 1024)
    assert server.client.post("/v2/upload", content=b"audio").status_code == 429
    assert server.client.delete(f"/v2/transcript/{job['id']}").status_code == 200


def test_queue_full_returns_429(start):
    server = start(MAX_PENDING_JOBS=1)
    job, _ = server.queue()
    assert server.submit(job["audio_url"])["id"] == job["id"]
    waiting, _ = server.upload()
    full = server.client.post("/v2/transcript", json={"audio_url": waiting})
    assert full.status_code == 429
    assert full.headers["retry-after"] == "10"
    assert error_message(full) == "Queue full"

    files = server.audio_files()
    v1 = server.client.post(V1, files={"file": ("a.wav", b"audio")})
    assert v1.status_code == 429 and v1.headers["retry-after"] == "10"
    assert error_message(v1, v1=True) == "Queue full"
    assert server.audio_files() == files

    claim = server.claim()
    # Processing jobs still count as pending.
    assert server.client.post("/v2/transcript", json={"audio_url": waiting}).status_code == 429
    server.finish(claim)
    second = server.submit(waiting)
    assert second["status"] == "queued"
    server.finish(server.claim())
    assert server.metrics() == {"completed": 2}


def test_sync_wait_times_out_with_504(start):
    server = start(SYNC_TIMEOUT_SECONDS=1)
    started = time.monotonic()
    response = server.client.post(V1, files={"file": ("a.wav", b"audio")})
    assert response.status_code == 504
    assert error_message(response, v1=True) == "Transcription wait timed out; use the asynchronous /v2 API"
    assert 0.9 < time.monotonic() - started < 10
    assert wait_until(lambda: server.metrics() == {}, 5)
    assert server.audio_files() == set()
    assert server.claim() is None
