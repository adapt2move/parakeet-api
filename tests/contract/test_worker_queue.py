"""Internal worker contract: claims, fenced leases, completion validation, retries and audio cleanup."""

import re
import uuid
from concurrent.futures import ThreadPoolExecutor

import httpx
import pytest

from .support import UUID_PATTERN, WORKER_KEY, error_message, sample_result

INVALID = "Invalid or unsupported request fields"
FENCED = "Lease expired or job cancelled"


def test_empty_queue_claim_returns_no_content(api):
    response = api.worker.post("/internal/jobs/claim")
    assert response.status_code == 204
    assert response.content == b""


def test_claim_payload_audio_and_heartbeat(api):
    job, uid = api.queue(b"audio", language_code="de")
    claim = api.claim()
    assert set(claim) == {"id", "token", "lease_seconds", "bytes", "options"}
    assert claim["id"] == job["id"]
    assert re.fullmatch(UUID_PATTERN, claim["token"])
    assert claim["lease_seconds"] == 15
    assert claim["bytes"] == 5
    assert claim["options"] == {"language_code": "de"}
    assert api.client.get(f"/v2/transcript/{job['id']}").json()["status"] == "processing"

    audio = api.fetch_audio(claim)
    assert audio.status_code == 200
    assert audio.content == b"audio"
    assert audio.headers["content-type"] == "application/octet-stream"
    heartbeat = api.heartbeat(claim)
    assert heartbeat.status_code == 200 and heartbeat.json() == {"ok": True}
    api.finish(claim)
    assert api.client.get(f"/v2/transcript/{job['id']}").json()["status"] == "completed"

    api.queue(b"more audio")
    claim = api.claim()
    assert claim["options"] == {} and claim["bytes"] == 10
    api.finish(claim)


def test_claims_follow_submission_order(api):
    jobs = [api.queue()[0]["id"] for _ in range(3)]
    claims = [api.claim() for _ in jobs]
    assert [c["id"] for c in claims] == jobs
    for claim in claims:
        api.finish(claim)


def test_claims_are_exclusive_under_concurrency(api):
    jobs = {api.queue()[0]["id"] for _ in range(4)}

    def claim(_):
        with httpx.Client(base_url=api.base, headers={"authorization": WORKER_KEY}, trust_env=False) as http:
            response = http.post("/internal/jobs/claim")
        assert response.status_code in (200, 204), response.text
        return response.json() if response.status_code == 200 else None

    with ThreadPoolExecutor(24) as pool:
        claims = [c for c in pool.map(claim, range(48)) if c]
    assert len(claims) == 4
    assert {c["id"] for c in claims} == jobs
    assert len({c["token"] for c in claims}) == 4
    for claim in claims:
        api.finish(claim)


def test_lease_token_is_required_and_fenced(api, result):
    job, _ = api.queue()
    claim = api.claim()
    body = {"result": result}
    calls = (
        lambda token: api.heartbeat(claim, token),
        lambda token: api.fetch_audio(claim, token),
        lambda token: api.complete(claim, body, token),
    )
    for call in calls:
        response = call("")
        assert response.status_code == 409
        assert error_message(response) == "Missing lease token"
        response = call(str(uuid.uuid4()))
        assert response.status_code == 409
        assert error_message(response) == FENCED

    assert api.claim() is None
    no_header = api.worker.post(f"/internal/jobs/{claim['id']}/heartbeat")
    assert no_header.status_code == 409 and error_message(no_header) == "Missing lease token"
    unknown = {"id": str(uuid.uuid4()), "token": claim["token"]}
    assert api.heartbeat(unknown).status_code == 409
    assert api.fetch_audio(unknown).status_code == 409
    assert api.complete(unknown, body).status_code == 409
    # The real lease still works after all the rejected attempts.
    assert api.heartbeat(claim).status_code == 200
    api.finish(claim)
    assert api.client.get(f"/v2/transcript/{job['id']}").json()["status"] == "completed"


def test_delete_fences_the_worker_and_removes_audio(api, result):
    job, uid = api.queue()
    claim = api.claim()
    assert uid in api.audio_files()
    assert api.client.delete(f"/v2/transcript/{job['id']}").status_code == 200
    assert uid not in api.audio_files()
    for response in (
        api.heartbeat(claim),
        api.fetch_audio(claim),
        api.complete(claim, {"result": result}),
        api.complete(claim, {"error": "Transient", "retry": True}),
    ):
        assert response.status_code == 409
        assert error_message(response) == FENCED
    assert api.client.get(f"/v2/transcript/{job['id']}").status_code == 404
    assert api.claim() is None


def test_retry_issues_a_new_lease_and_fences_the_old_token(api):
    job, uid = api.queue()
    first = api.claim()
    response = api.complete(first, {"error": "Transient", "retry": True})
    assert response.status_code == 200 and response.json() == {"ok": True}
    transcript = api.client.get(f"/v2/transcript/{job['id']}").json()
    assert transcript["status"] == "queued" and transcript["error"] is None
    assert uid in api.audio_files()
    assert api.heartbeat(first).status_code == 409

    second = api.claim()
    assert second["id"] == first["id"] and second["token"] != first["token"]
    assert api.complete(first, {"result": sample_result()}).status_code == 409
    assert api.fetch_audio(second).content == b"audio"
    api.finish(second)
    assert uid not in api.audio_files()


def test_complete_requires_exactly_one_of_result_or_error(api, result):
    api.queue()
    claim = api.claim()
    for body in ({}, {"result": result, "error": "Both"}, {"result": None, "error": None}, {"retry": True}):
        response = api.complete(claim, body)
        assert response.status_code == 400, body
        assert error_message(response) == "Provide exactly one of result or error"
    api.finish(claim)


def mutate(change):
    body = {"result": sample_result()}
    change(body)
    return body


def set_word(index, **fields):
    return lambda body: body["result"]["words"][index].update(fields)


def set_result(**fields):
    return lambda body: body["result"].update(fields)


INVALID_BODIES = {
    "unordered starts": set_word(1, start=10),
    "unordered ends": lambda body: body["result"].update(
        words=[
            {"text": "Hello", "start": 100, "end": 900, "confidence": 0.8},
            {"text": "world.", "start": 200, "end": 500, "confidence": 0.7},
        ]
    ),
    "end after duration": set_word(1, end=2101),
    "zero-length word": set_word(0, start=610),
    "negative start": set_word(0, start=-1),
    "zero end": lambda body: body["result"].update(
        words=[{"text": "Hello", "start": 0, "end": 0, "confidence": 0.8}], text="Hello"
    ),
    "fractional start": set_word(0, start=120.5),
    "confidence above one": set_word(0, confidence=1.5),
    "negative confidence": set_word(0, confidence=-0.1),
    "text mismatch": set_result(text="Hello  world."),
    "blank word text": lambda body: body["result"].update(
        words=[{"text": "", "start": 0, "end": 10, "confidence": 0.8}], text=""
    ),
    "unknown word field": set_word(0, foo=1),
    "speaker label": set_word(0, speaker="A"),
    "unknown result field": set_result(language="en"),
    "missing chunks": lambda body: body["result"].pop("chunks"),
    "missing words": lambda body: body["result"].pop("words"),
    "zero chunks": set_result(chunks=0),
    "negative seam fallbacks": set_result(seam_fallbacks=-1),
    "zero duration": set_result(audio_duration_ms=0),
    "duration above three hours": set_result(audio_duration_ms=10_800_001),
    "unknown top-level field": lambda body: body.update(worker="w1"),
    "error too long": lambda body: body.update(result=None, error="x" * 201),
}


def test_complete_rejects_invalid_or_unordered_results(api):
    job, uid = api.queue()
    claim = api.claim()
    for name, change in INVALID_BODIES.items():
        response = api.complete(claim, mutate(change))
        assert response.status_code == 422, name
        assert error_message(response) == INVALID, name
    for content in (b"{not json", b"[]"):
        response = api.worker.post(
            f"/internal/jobs/{claim['id']}/complete",
            headers={**api.lease(claim), "content-type": "application/json"},
            content=content,
        )
        assert response.status_code == 422
        assert error_message(response) == INVALID
    # Rejections leave the lease intact.
    assert api.client.get(f"/v2/transcript/{job['id']}").json()["status"] == "processing"
    assert uid in api.audio_files()
    api.finish(claim)


ACCEPTED = {
    "no words": {"text": "", "words": [], "audio_duration_ms": 1, "chunks": 1, "seam_fallbacks": 0},
    "overlapping equal timings": {
        "text": "a b",
        "words": [
            {"text": "a", "start": 0, "end": 10, "confidence": 0, "speaker": None, "channel": None},
            {"text": "b", "start": 0, "end": 10, "confidence": 1},
        ],
        "audio_duration_ms": 10,
        "chunks": 3,
        "seam_fallbacks": 2,
    },
    "three hours": {
        "text": "end",
        "words": [{"text": "end", "start": 10_799_999, "end": 10_800_000, "confidence": 0.5}],
        "audio_duration_ms": 10_800_000,
        "chunks": 1,
        "seam_fallbacks": 0,
    },
    "exactly 200 character error": None,
}


@pytest.mark.parametrize("name", ACCEPTED)
def test_complete_accepts_boundary_results(api, name):
    job, _ = api.queue()
    claim = api.claim()
    if ACCEPTED[name] is None:
        response = api.complete(claim, {"error": "e" * 200})
        assert response.status_code == 200, response.text
        assert api.client.get(f"/v2/transcript/{job['id']}").json()["error"] == "e" * 200
        return
    api.finish(claim, ACCEPTED[name])
    transcript = api.client.get(f"/v2/transcript/{job['id']}").json()
    assert transcript["status"] == "completed"
    assert transcript["text"] == ACCEPTED[name]["text"]
    # Stored words always carry the AssemblyAI speaker and channel fields.
    assert transcript["words"] == [
        {**word, "speaker": None, "channel": None} for word in ACCEPTED[name]["words"]
    ]


def test_error_completion_fails_the_job_and_removes_audio(api):
    job, uid = api.queue()
    claim = api.claim()
    response = api.complete(claim, {"error": "Audio could not be decoded"})
    assert response.status_code == 200 and response.json() == {"ok": True}
    assert uid not in api.audio_files()
    transcript = api.client.get(f"/v2/transcript/{job['id']}").json()
    assert transcript["status"] == "error"
    assert transcript["error"] == "Audio could not be decoded"
    assert transcript["text"] is None and transcript["words"] is None
    assert transcript["confidence"] is None and transcript["audio_duration"] is None
    assert api.heartbeat(claim).status_code == 409
    assert api.claim() is None
    captions = api.client.get(f"/v2/transcript/{job['id']}/srt")
    assert captions.status_code == 409 and error_message(captions) == "Transcript is not complete"


def test_result_with_retry_still_completes(api, result):
    job, uid = api.queue()
    claim = api.claim()
    response = api.complete(claim, {"result": result, "retry": True})
    assert response.status_code == 200, response.text
    assert api.client.get(f"/v2/transcript/{job['id']}").json()["status"] == "completed"
    assert uid not in api.audio_files()
    assert api.claim() is None


def test_retry_exhaustion_fails_the_job(start):
    server = start(MAX_ATTEMPTS=2)
    job, uid = server.queue()
    for attempt in range(2):
        claim = server.claim()
        assert claim["id"] == job["id"]
        response = server.complete(claim, {"error": f"Transient {attempt}", "retry": True})
        assert response.status_code == 200, response.text
    assert server.claim() is None
    transcript = server.client.get(f"/v2/transcript/{job['id']}").json()
    assert transcript["status"] == "error"
    assert transcript["error"] == "Transient 1"
    assert uid not in server.audio_files()


@pytest.mark.parametrize("outcome", ["complete", "error", "delete queued", "delete processing"])
def test_audio_is_removed_when_the_job_ends(api, outcome):
    job, uid = api.queue()
    assert uid in api.audio_files()
    if outcome == "delete queued":
        assert api.client.delete(f"/v2/transcript/{job['id']}").status_code == 200
        assert uid not in api.audio_files()
        return
    claim = api.claim()
    assert uid in api.audio_files()
    if outcome == "complete":
        api.finish(claim)
    elif outcome == "error":
        assert api.complete(claim, {"error": "Audio could not be decoded"}).status_code == 200
    else:
        assert api.client.delete(f"/v2/transcript/{job['id']}").status_code == 200
    assert uid not in api.audio_files()


def test_resubmit_is_idempotent_and_rejects_different_options(api):
    url, uid = api.upload()
    job = api.submit(url)
    assert api.submit(url)["id"] == job["id"]
    assert api.submit(url, language_code=None)["id"] == job["id"]
    assert api.submit(url, language_code="")["id"] == job["id"]
    conflict = api.client.post("/v2/transcript", json={"audio_url": url, "language_code": "de"})
    assert conflict.status_code == 409
    assert error_message(conflict) == "Upload already submitted with different options"

    api.finish(api.claim())
    # Finished jobs released their audio but stay idempotent.
    again = api.submit(url)
    assert again["id"] == job["id"] and again["status"] == "completed"
    assert uid not in api.audio_files()

    assert api.client.delete(f"/v2/transcript/{job['id']}").status_code == 200
    gone = api.client.post("/v2/transcript", json={"audio_url": url})
    assert gone.status_code == 400
    assert error_message(gone) == "Upload missing or expired"
