"""Health, authentication roles, routing, error shapes, metrics and log hygiene."""

import json

import pytest

from .support import API_KEY, WORKER_KEY, error_message, multipart, raw_response, timed_result

JOB = "00000000-0000-4000-8000-000000000000"
CLIENT_ROUTES = [
    ("POST", "/v2/upload"),
    ("POST", "/v2/transcript"),
    ("GET", f"/v2/transcript/{JOB}"),
    ("DELETE", f"/v2/transcript/{JOB}"),
    ("GET", f"/v2/transcript/{JOB}/srt"),
    ("POST", "/v1/audio/transcriptions"),
    ("GET", "/metrics"),
    ("GET", "/health/live/"),
]
WORKER_ROUTES = [
    ("POST", "/internal/jobs/claim"),
    ("POST", f"/internal/jobs/{JOB}/heartbeat"),
    ("GET", f"/internal/jobs/{JOB}/audio"),
    ("POST", f"/internal/jobs/{JOB}/complete"),
]
WRONG_CLIENT = [None, "", "wrong", WORKER_KEY, f"Bearer {WORKER_KEY}", API_KEY[:-1], f"Basic {API_KEY}"]
WRONG_WORKER = [None, "", "wrong", API_KEY, f"Bearer {API_KEY}", WORKER_KEY[:-1], f"Bearer  {WORKER_KEY}"]


def auth(authorization):
    return {} if authorization is None else {"authorization": authorization}


def test_health_endpoints_need_no_auth(api):
    for path in ("/health/live", "/health/ready"):
        for authorization in (None, "wrong", API_KEY, WORKER_KEY):
            response = api.anonymous.get(path, headers=auth(authorization))
            assert response.status_code == 200 and response.json() == {"status": "ok"}


@pytest.mark.parametrize(
    ("method", "path", "key", "wrong"),
    [(m, p, API_KEY, WRONG_CLIENT) for m, p in CLIENT_ROUTES]
    + [(m, p, WORKER_KEY, WRONG_WORKER) for m, p in WORKER_ROUTES],
)
def test_routes_require_their_key(api, method, path, key, wrong):
    for authorization in wrong:
        response = api.anonymous.request(method, path, headers=auth(authorization))
        assert response.status_code == 401, authorization
        assert error_message(response, v1=None) == "Unauthorized"
    for authorization in (key, f"Bearer {key}"):
        assert api.anonymous.request(method, path, headers=auth(authorization)).status_code != 401


def test_authentication_precedes_body_handling(api):
    for authorization in (None, WORKER_KEY):
        status, body = raw_response(
            api, "POST", "/v2/upload", {"Content-Length": str(10**9)}, authorization=authorization
        )
        assert status == 401
        assert error_message(json.loads(body)) == "Unauthorized"


UNMATCHED = [
    ("GET", "/", 404),
    ("GET", "/v2/nothing", 404),
    ("GET", f"/uploads/{JOB}", 404),
    ("GET", "/v1/models", 404),
    ("GET", f"/v2/transcript/{JOB}/txt", 404),
    # Trailing slashes are different paths, not redirects.
    ("POST", "/v2/upload/", 404),
    ("POST", "/v2/transcript/", 404),
    ("GET", f"/v2/transcript/{JOB}/", 404),
    ("POST", "/v1/audio/transcriptions/", 404),
    ("GET", "/health/live/", 404),
    ("PUT", "/v2/upload", 405),
    ("GET", "/v2/transcript", 405),
    ("POST", f"/v2/transcript/{JOB}", 405),
    ("GET", "/v1/audio/transcriptions", 405),
    ("GET", "/internal/jobs/claim/", 404),
    ("GET", "/internal/jobs/claim", 405),
]


@pytest.mark.parametrize(("method", "path", "status"), UNMATCHED)
def test_unmatched_requests_get_json_errors(api, method, path, status):
    http = api.worker if path.startswith("/internal/") else api.client
    response = http.request(method, path)
    assert response.status_code == status
    assert "location" not in response.headers
    assert error_message(response, v1=path.startswith("/v1/"))


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
    assert server.complete(server.claim(), {"error": "Audio could not be decoded"}).status_code == 200
    assert server.metrics() == {"queued": 1, "completed": 1, "error": 1}
    assert server.complete(server.claim(), {"error": "Transient", "retry": True}).status_code == 200
    assert server.metrics() == {"queued": 1, "completed": 1, "error": 1}
    assert server.client.delete(f"/v2/transcript/{jobs[2]['id']}").status_code == 200
    assert server.client.get("/metrics").text.splitlines() == [
        'parakeet_jobs{status="completed"} 1',
        'parakeet_jobs{status="error"} 1',
    ]


def test_logs_are_json_without_secrets_urls_or_content(start):
    server = start()
    marker = "zebra-transcript-marker"
    result = timed_result([marker])
    url, _ = server.upload(b"secret-audio-bytes")
    job = server.submit(url)
    claim = server.claim()
    server.finish(claim, result)
    assert server.client.get(f"/v2/transcript/{job['id']}/vtt").status_code == 200
    server.client.post("/v2/upload", content=b"")
    server.client.post("/v2/transcript", json={"audio_url": "https://leaky-host.invalid/secret-path.wav"})
    server.client.post("/v2/transcript", json={"audio_url": "https://audio.invalid/secret-path.wav"})
    body, content_type = multipart(
        [("file", b"secret-audio-bytes", "secret-filename.wav"), ("model", "x", None)]
    )
    server.client.post("/v1/audio/transcriptions", content=body, headers=content_type)
    server.anonymous.get("/metrics", headers={"authorization": "guessed-secret-key"})

    logs = server.logs()
    lines = [line for line in logs.splitlines() if line.strip()]
    assert lines and all(isinstance(json.loads(line), dict) for line in lines)
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
        str(server.data),
    ):
        assert forbidden not in logs, forbidden
