import asyncio
import json
import sys
import threading
import types

import httpx
import pytest

RESULT = {
    "text": "Hello",
    "words": [{"text": "Hello", "start": 0, "end": 500, "confidence": 0.9}],
    "audio_duration_ms": 1000,
    "chunks": 1,
    "seam_fallbacks": 0,
}


@pytest.fixture
def worker(monkeypatch):
    # The worker module loads native inference; the retry logic does not need it.
    monkeypatch.setitem(sys.modules, "parakeet_api.engine", types.SimpleNamespace(Engine=None))
    monkeypatch.delitem(sys.modules, "parakeet_api.worker", raising=False)
    import parakeet_api.worker as module

    monkeypatch.setattr(module, "BACKOFF_SECONDS", 0)
    return module


class Engine:
    def __init__(self, error=None):
        self.calls, self.error = 0, error

    def transcribe(self, directory, cancel, progress):
        self.calls += 1
        if self.error:
            raise self.error
        return RESULT, 0


def run(worker, engine, completions):
    """Execute one job against a fake API answering /complete with the given statuses or errors."""
    posted = []

    def handler(request):
        if request.url.path.endswith("/audio"):
            return httpx.Response(200, content=b"audio")
        posted.append(json.loads(request.content))
        answer = completions.pop(0)
        if isinstance(answer, Exception):
            raise answer
        return httpx.Response(answer, json={})

    async def main():
        transport = httpx.MockTransport(handler)
        async with httpx.AsyncClient(base_url="http://api", transport=transport) as client:
            job = {"id": "job", "token": "token", "lease_seconds": 90, "bytes": 5, "options": {}}
            await worker.execute(client, engine, job, threading.Event())

    asyncio.run(main())
    return posted


def test_transient_api_failures_resend_the_finished_result(worker):
    engine = Engine()
    posted = run(worker, engine, [503, httpx.ConnectError("down"), 200])
    assert engine.calls == 1
    assert [body.keys() for body in posted] == [{"result"}] * 3
    assert worker.state["completed"] == 1


def test_rejected_result_is_a_permanent_error(worker):
    engine = Engine()
    posted = run(worker, engine, [422, 200])
    assert engine.calls == 1
    assert posted[1] == {"error": "Transcription rejected by API", "retry": False}


def test_lost_lease_reports_nothing(worker):
    posted = run(worker, Engine(), [409])
    assert len(posted) == 1 and "result" in posted[0]


@pytest.mark.parametrize(
    "error,expected",
    [
        (RuntimeError("native"), {"error": "Worker processing failed", "retry": True}),
        (ValueError("Audio could not be decoded"), {"error": "Audio could not be decoded", "retry": False}),
    ],
)
def test_inference_failures_choose_retry_by_cause(worker, error, expected):
    assert run(worker, Engine(error), [200]) == [expected]
