"""Internal worker contract: claims, fenced leases, completion validation, retries and audio cleanup."""

import json
import re
import uuid
from concurrent.futures import ThreadPoolExecutor

import pytest

from .support import INVALID, UUID_PATTERN, error_message, sample_result, timed_result

FENCED = "Lease expired or job cancelled"


def test_claim_payload_audio_and_heartbeat(api):
    empty = api.worker.post("/internal/jobs/claim")
    assert empty.status_code == 204 and empty.content == b""

    job, _ = api.queue(b"audio", language_code="de")
    claim = api.claim()
    options = {"language_code": "de"}
    assert claim == {
        "id": job["id"],
        "token": claim["token"],
        "lease_seconds": 15,
        "bytes": 5,
        "options": options,
    }
    assert re.fullmatch(UUID_PATTERN, claim["token"])
    assert api.transcript(job["id"])["status"] == "processing"

    audio = api.fetch_audio(claim)
    assert audio.status_code == 200 and audio.content == b"audio"
    assert audio.headers["content-type"] == "application/octet-stream"
    heartbeat = api.heartbeat(claim)
    assert heartbeat.status_code == 200 and heartbeat.json() == {"ok": True}
    api.finish(claim)
    assert api.transcript(job["id"])["status"] == "completed"

    api.queue(b"more audio")
    claim = api.claim()
    assert claim["options"] == {} and claim["bytes"] == 10
    api.finish(claim)


def test_claims_are_ordered_and_exclusive(api):
    jobs = [api.queue()[0]["id"] for _ in range(4)]
    first = api.claim()
    assert first["id"] == jobs[0]
    with ThreadPoolExecutor(24) as pool:
        claims = [first] + [c for c in pool.map(lambda _: api.claim(), range(48)) if c]
    assert sorted(c["id"] for c in claims) == sorted(jobs)
    assert len({c["token"] for c in claims}) == 4
    for claim in claims:
        api.finish(claim)


def test_lease_token_is_required_and_fenced(api, result):
    job, _ = api.queue()
    claim = api.claim()
    unknown = {"id": str(uuid.uuid4()), "token": claim["token"]}
    for call in (api.heartbeat, api.fetch_audio, lambda c, t=None: api.complete(c, {"result": result}, t)):
        response = call(claim, "")
        assert response.status_code == 409 and error_message(response) == "Missing lease token"
        for response in (call(claim, str(uuid.uuid4())), call(unknown)):
            assert response.status_code == 409 and error_message(response) == FENCED
    # The real lease still works after all the rejected attempts.
    assert api.heartbeat(claim).status_code == 200
    api.finish(claim)
    assert api.transcript(job["id"])["status"] == "completed"


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
        assert response.status_code == 409 and error_message(response) == FENCED
    assert api.client.get(f"/v2/transcript/{job['id']}").status_code == 404


def test_retry_issues_a_new_lease_and_a_result_always_completes(api, result):
    job, uid = api.queue()
    first = api.claim()
    response = api.complete(first, {"error": "Transient", "retry": True})
    assert response.status_code == 200 and response.json() == {"ok": True}
    transcript = api.transcript(job["id"])
    assert transcript["status"] == "queued" and transcript["error"] is None
    assert uid in api.audio_files()
    assert api.heartbeat(first).status_code == 409

    second = api.claim()
    assert second["id"] == first["id"] and second["token"] != first["token"]
    assert api.complete(first, {"result": result}).status_code == 409
    assert api.fetch_audio(second).content == b"audio"
    assert api.complete(second, {"result": result, "retry": True}).status_code == 200
    assert api.transcript(job["id"])["status"] == "completed"
    assert uid not in api.audio_files()


def test_retry_exhaustion_fails_the_job(start):
    server = start(MAX_ATTEMPTS=2)
    job, uid = server.queue()
    for attempt in range(2):
        claim = server.claim()
        assert claim["id"] == job["id"]
        assert server.complete(claim, {"error": f"Transient {attempt}", "retry": True}).status_code == 200
    assert server.claim() is None
    transcript = server.transcript(job["id"])
    assert transcript["status"] == "error" and transcript["error"] == "Transient 1"
    assert uid not in server.audio_files()


def test_error_completion_fails_the_job_and_removes_audio(api):
    job, uid = api.queue()
    claim = api.claim()
    response = api.complete(claim, {"error": "e" * 200})
    assert response.status_code == 200 and response.json() == {"ok": True}
    assert uid not in api.audio_files()
    transcript = api.transcript(job["id"])
    assert transcript == {**transcript, "status": "error", "error": "e" * 200, "text": None, "words": None}
    assert transcript["confidence"] is None and transcript["audio_duration"] is None
    assert api.heartbeat(claim).status_code == 409
    captions = api.client.get(f"/v2/transcript/{job['id']}/srt")
    assert captions.status_code == 409 and error_message(captions) == "Transcript is not complete"


def word(index, **fields):
    return lambda result: result["words"][index].update(fields)


def top(**fields):
    return lambda result: result.update(fields)


def one_word(text):
    return top(
        text=text, words=[{"text": text, "start": 0, "end": 10, "confidence": 0.5}], audio_duration_ms=10
    )


INVALID_RESULTS = {
    "unordered starts": word(1, start=10),
    "unordered ends": word(1, start=200, end=500),
    "end after duration": word(1, end=2101),
    "zero-length word": word(0, start=610),
    "negative start": word(0, start=-1),
    "fractional start": word(0, start=120.5),
    "confidence above one": word(0, confidence=1.5),
    "negative confidence": word(0, confidence=-0.1),
    "text mismatch": top(text="Hello  world."),
    "blank word text": one_word(""),
    "word text too long": one_word("a" * 4097),
    "unknown word field": word(0, foo=1),
    "speaker label": word(0, speaker="A"),
    "channel": word(0, channel=1),
    "unknown result field": top(language="en"),
    "missing chunks": lambda result: result.pop("chunks"),
    "missing words": lambda result: result.pop("words"),
    "zero chunks": top(chunks=0),
    "negative seam fallbacks": top(seam_fallbacks=-1),
    "zero duration": top(audio_duration_ms=0),
    "duration above three hours": top(audio_duration_ms=10_800_001),
    # No coercion: numbers must be JSON numbers, integers where integers are expected.
    "numeric string start": word(0, start="120"),
    "numeric string confidence": word(0, confidence="0.8"),
    "numeric string duration": top(audio_duration_ms="2100"),
    "integral float start": word(0, start=120.0),
    "float chunks": top(chunks=1.0),
    "boolean chunks": top(chunks=True),
    "boolean confidence": word(0, confidence=True),
    "numeric text": word(0, text=5),
    "words object": top(words={}),
}


def encoded(change=None, **body):
    result = sample_result()
    if change:
        change(result)
    return json.dumps({"result": result, **body}).encode()


INVALID_BODIES = [encoded(change) for change in INVALID_RESULTS.values()] + [
    json.dumps({"error": "x" * 201}).encode(),
    json.dumps({"error": 5}).encode(),
    encoded(worker="w1"),
    encoded(retry="true"),
    encoded(retry=1),
    b"{not json",
    b"[]",
    b"",
    encoded() + b" {}",
]


def test_complete_rejects_invalid_bodies(start, result):
    # A dedicated server: a body accepted by mistake must not leak a job into other tests.
    server = start()
    job, uid = server.queue()
    claim = server.claim()
    for body in ({}, {"result": result, "error": "Both"}, {"result": None, "error": None}, {"retry": True}):
        response = server.complete(claim, body)
        assert (
            response.status_code == 400
            and error_message(response) == "Provide exactly one of result or error"
        )
    for body in INVALID_BODIES:
        response = server.complete(claim, body)
        assert response.status_code == 422 and error_message(response) == INVALID, body[:200]
    # Rejections leave the lease intact.
    assert server.transcript(job["id"])["status"] == "processing"
    assert uid in server.audio_files()
    server.finish(claim)


def test_complete_enforces_result_size_limits(api):
    job, _ = api.queue()
    claim = api.claim()
    too_many = timed_result(["a"] * 100_001, step=1, length=1, duration_ms=200_000)
    too_long = timed_result(["a" * 4096] * 489, step=10, length=5)
    for result in (too_many, too_long):
        response = api.complete(claim, {"result": result})
        assert response.status_code == 422 and error_message(response) == INVALID
    api.finish(claim, timed_result(["a"] * 100_000, step=1, length=1, duration_ms=200_000))
    assert len(api.transcript(job["id"])["words"]) == 100_000


ACCEPTED = {
    "no words": timed_result([], duration_ms=1),
    "three hours": {**timed_result(["end"]), "audio_duration_ms": 10_800_000},
    "overlapping equal timings": {
        **timed_result(["a", "b"], step=0, length=10, duration_ms=10),
        "chunks": 3,
        "seam_fallbacks": 2,
    },
}
ACCEPTED["three hours"]["words"][0].update(start=10_799_999, end=10_800_000)
ACCEPTED["overlapping equal timings"]["words"][0].update(confidence=0, speaker=None, channel=None)
ACCEPTED["overlapping equal timings"]["words"][1].update(confidence=1)


@pytest.mark.parametrize("name", ACCEPTED)
def test_complete_accepts_boundary_results(api, name):
    job, _ = api.queue()
    api.finish(api.claim(), ACCEPTED[name])
    transcript = api.transcript(job["id"])
    assert transcript["status"] == "completed" and transcript["text"] == ACCEPTED[name]["text"]
    # Stored words always carry the AssemblyAI speaker and channel fields.
    assert transcript["words"] == [{**w, "speaker": None, "channel": None} for w in ACCEPTED[name]["words"]]


def test_resubmit_is_idempotent_and_rejects_different_options(api):
    url, uid = api.upload()
    job = api.submit(url)
    for fields in ({}, {"language_code": None}, {"language_code": ""}):
        assert api.submit(url, **fields)["id"] == job["id"]
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
    assert gone.status_code == 400 and error_message(gone) == "Upload missing or expired"
