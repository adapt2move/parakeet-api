"""Health, authentication roles, error body shapes, metrics and log hygiene."""

import json
import uuid
from urllib.parse import urlsplit

import pytest

from .support import API_KEY, WORKER_KEY, RawRequest, error_message, multipart

JOB = "00000000-0000-4000-8000-000000000000"
CLIENT_ROUTES = [
    ("POST", "/v2/upload"),
    ("POST", "/v2/transcript"),
    ("GET", f"/v2/transcript/{JOB}"),
    ("DELETE", f"/v2/transcript/{JOB}"),
    ("GET", f"/v2/transcript/{JOB}/srt"),
    ("POST", "/v1/audio/transcriptions"),
    ("GET", "/metrics"),
    ("GET", "/not-a-route"),
]
WORKER_ROUTES = [
    ("POST", "/internal/jobs/claim"),
    ("POST", f"/internal/jobs/{JOB}/heartbeat"),
    ("GET", f"/internal/jobs/{JOB}/audio"),
    ("POST", f"/internal/jobs/{JOB}/complete"),
]
WRONG_CLIENT = [
    None,
    "",
    "wrong",
    WORKER_KEY,
    f"Bearer {WORKER_KEY}",
    API_KEY[:-1],
    API_KEY + "x",
    f"Basic {API_KEY}",
]
WRONG_WORKER = [None, "", "wrong", API_KEY, f"Bearer {API_KEY}", WORKER_KEY[:-1], f"Bearer  {WORKER_KEY}"]


def headers(authorization):
    return {} if authorization is None else {"authorization": authorization}


@pytest.mark.parametrize("path", ["/health/live", "/health/ready"])
def test_health_endpoints_need_no_auth(api, path):
    for authorization in (None, "wrong", API_KEY, WORKER_KEY):
        response = api.anonymous.get(path, headers=headers(authorization))
        assert response.status_code == 200
        assert response.json() == {"status": "ok"}


@pytest.mark.parametrize(("method", "path"), CLIENT_ROUTES)
def test_client_routes_require_the_client_key(api, method, path):
    for authorization in WRONG_CLIENT:
        response = api.anonymous.request(method, path, headers=headers(authorization))
        assert response.status_code == 401, authorization
        # Authentication failures use the plain shape even under /v1.
        assert response.json() == {"error": "Unauthorized"}
    for authorization in (API_KEY, f"Bearer {API_KEY}"):
        response = api.anonymous.request(method, path, headers=headers(authorization))
        assert response.status_code != 401


@pytest.mark.parametrize(("method", "path"), WORKER_ROUTES)
def test_internal_routes_require_the_worker_key(api, method, path):
    for authorization in WRONG_WORKER:
        response = api.anonymous.request(method, path, headers=headers(authorization))
        assert response.status_code == 401, authorization
        assert response.json() == {"error": "Unauthorized"}
    for authorization in (WORKER_KEY, f"Bearer {WORKER_KEY}"):
        response = api.anonymous.request(method, path, headers=headers(authorization))
        assert response.status_code != 401


def test_authentication_precedes_body_limits(api):
    for authorization in (None, WORKER_KEY):
        request = RawRequest(
            api, "POST", "/v2/upload", {"Content-Length": str(10**9)}, authorization=authorization
        )
        try:
            status, _, body = request.response()
        finally:
            request.close()
        assert status == 401
        assert json.loads(body) == {"error": "Unauthorized"}


def test_worker_key_with_bearer_prefix_drives_a_job(api):
    job, _ = api.queue()
    bearer = api.http({"authorization": f"Bearer {WORKER_KEY}"})
    try:
        claim = bearer.post("/internal/jobs/claim").json()
        assert claim["id"] == job["id"]
        lease = {"x-lease-token": claim["token"]}
        assert bearer.post(f"/internal/jobs/{claim['id']}/heartbeat", headers=lease).status_code == 200
        assert bearer.get(f"/internal/jobs/{claim['id']}/audio", headers=lease).content == b"audio"
        assert bearer.post(
            f"/internal/jobs/{claim['id']}/complete", headers=lease, json={"error": "x"}
        ).json() == {"ok": True}
    finally:
        bearer.close()


def test_error_shapes_differ_between_v1_and_other_paths(api):
    missing = api.client.get(f"/v2/transcript/{uuid.uuid4()}")
    assert missing.status_code == 404
    assert missing.json() == {"error": "Transcript missing or expired"}

    invalid = api.client.post("/v2/transcript", json={"audio_url": "x", "extra": 1})
    assert invalid.json() == {"error": "Invalid or unsupported request fields"}

    lease = api.worker.post(f"/internal/jobs/{JOB}/heartbeat")
    assert lease.status_code == 409 and lease.json() == {"error": "Missing lease token"}

    body, content_type = multipart([("file", b"audio", "a.wav"), ("model", "tiny", None)])
    v1 = api.client.post("/v1/audio/transcriptions", content=body, headers=content_type)
    assert v1.status_code == 400
    assert v1.json() == {
        "error": {"message": "Unknown model; use parakeet", "type": "invalid_request_error", "code": "400"}
    }
    unsupported = api.client.post(
        "/v1/audio/transcriptions", files={"file": ("a.wav", b"audio")}, data={"prompt": "x"}
    )
    assert unsupported.status_code == 422
    assert error_message(unsupported, v1=True).startswith("Unsupported transcription fields")


def test_unknown_routes_are_not_found(api):
    for method, path in (
        ("GET", "/"),
        ("GET", "/v1/models"),
        ("GET", "/v2/nothing"),
        ("GET", f"/uploads/{JOB}"),
    ):
        assert api.client.request(method, path).status_code == 404, path
    assert api.client.request("GET", "/v2/transcript").status_code in (404, 405)
    assert api.worker.request("GET", "/internal/jobs/claim").status_code in (404, 405)


def test_unknown_routes_use_the_error_shapes(api):
    for path in ("/v2/nothing", "/metrics/extra", f"/uploads/{JOB}"):
        response = api.client.get(path)
        assert response.status_code == 404
        assert error_message(response)
    v1 = api.client.get("/v1/models")
    assert v1.status_code == 404
    assert error_message(v1, v1=True)
    wrong_method = api.client.put("/v2/upload", content=b"x")
    assert wrong_method.status_code == 405
    assert error_message(wrong_method)
    wrong_v1_method = api.client.get("/v1/audio/transcriptions")
    assert wrong_v1_method.status_code == 405
    assert error_message(wrong_v1_method, v1=True)


def test_trailing_slashes_redirect_to_the_route(api):
    for method, path, target in (
        ("POST", "/v2/upload/", "/v2/upload"),
        ("GET", "/v2/upload/", "/v2/upload"),
        ("POST", "/v2/transcript//", "/v2/transcript"),
        ("GET", f"/v2/transcript/{JOB}/?x=1&y", f"/v2/transcript/{JOB}?x=1&y"),
        ("DELETE", f"/v2/transcript/{JOB}/", f"/v2/transcript/{JOB}"),
        ("GET", f"/v2/transcript/{JOB}/srt/", f"/v2/transcript/{JOB}/srt"),
        ("POST", "/v1/audio/transcriptions/", "/v1/audio/transcriptions"),
        ("GET", "/metrics/", "/metrics"),
        ("GET", "/health/live/", "/health/live"),
    ):
        response = api.client.request(method, path, content=b"x")
        assert response.status_code == 307, path
        assert response.content == b""
        # Only the redirect target matters, not whether the URL is absolute.
        location = urlsplit(response.headers["location"])
        assert location.path + ("?" + location.query if location.query else "") == target
    assert api.worker.post("/internal/jobs/claim/").status_code == 307
    assert api.anonymous.get("/health/live/").status_code == 401
    assert api.client.get("/v2/nothing/").status_code == 404
    followed = api.client.post("/v2/upload/", content=b"audio", follow_redirects=True)
    assert followed.status_code == 200 and followed.json()["upload_url"]


def test_metrics_count_jobs_by_status(start):
    server = start()
    empty = server.client.get("/metrics")
    assert empty.status_code == 200
    assert empty.headers["content-type"].split(";")[0] == "text/plain"
    assert empty.text == ""

    jobs = [server.queue()[0] for _ in range(3)]
    assert server.metrics() == {"queued": 3}
    first = server.claim()
    assert server.metrics() == {"queued": 2, "processing": 1}
    server.finish(first)
    assert server.metrics() == {"queued": 2, "completed": 1}
    second = server.claim()
    assert server.complete(second, {"error": "Audio could not be decoded"}).status_code == 200
    assert server.metrics() == {"queued": 1, "completed": 1, "error": 1}
    third = server.claim()
    assert server.complete(third, {"error": "Transient", "retry": True}).status_code == 200
    assert server.metrics() == {"queued": 1, "completed": 1, "error": 1}
    assert server.client.delete(f"/v2/transcript/{jobs[2]['id']}").status_code == 200
    assert server.metrics() == {"completed": 1, "error": 1}
    lines = sorted(server.client.get("/metrics").text.splitlines())
    assert lines == ['parakeet_jobs{status="completed"} 1', 'parakeet_jobs{status="error"} 1']


def test_logs_do_not_contain_secrets_urls_or_content(start):
    server = start()
    marker = "zebra-transcript-marker"
    result = {
        "text": marker,
        "words": [{"text": marker, "start": 0, "end": 100, "confidence": 0.5}],
        "audio_duration_ms": 100,
        "chunks": 1,
        "seam_fallbacks": 0,
    }
    url, uid = server.upload(b"secret-audio-bytes")
    job = server.submit(url)
    claim = server.claim()
    server.finish(claim, result)
    assert server.client.get(f"/v2/transcript/{job['id']}/vtt").status_code == 200
    server.client.post("/v2/transcript", json={"audio_url": "https://leaky-host.invalid/secret-path.wav"})
    server.client.post("/v2/transcript", json={"audio_url": "https://audio.invalid/secret-path.wav"})
    body, content_type = multipart(
        [("file", b"secret-audio-bytes", "secret-filename.wav"), ("model", "x", None)]
    )
    server.client.post("/v1/audio/transcriptions", content=body, headers=content_type)
    server.anonymous.get("/metrics", headers={"authorization": "guessed-secret-key"})

    logs = server.logs()
    for forbidden in (
        API_KEY,
        WORKER_KEY,
        claim["token"],
        marker,
        "secret-audio-bytes",
        "secret-filename",
        "secret-path",
        "leaky-host",
        "guessed-secret-key",
        "/uploads/",
    ):
        assert forbidden not in logs, forbidden


def test_logs_are_json_lines(start):
    server = start()
    server.queue()
    server.finish(server.claim())
    server.client.post("/v2/upload", content=b"")
    lines = [line for line in server.logs().splitlines() if line.strip()]
    assert lines
    for line in lines:
        assert isinstance(json.loads(line), dict), line
