"""OpenAI-compatible synchronous /v1/audio/transcriptions over real HTTP."""

from concurrent.futures import ThreadPoolExecutor

import httpx
import pytest
from openai import OpenAI

from .support import API_KEY, RawRequest, error_message, multipart, sample_result, timed_result, wait_until

PATH = "/v1/audio/transcriptions"
SRT = "1\n00:00:00,120 --> 00:00:01,700\nHello world.\n"
VTT = "WEBVTT\n\n1\n00:00:00.120 --> 00:00:01.700\nHello world.\n"
VERBOSE = {"task": "transcribe", "duration": 2.1, "text": "Hello world."}
WORDS = [{"word": "Hello", "start": 0.12, "end": 0.61}, {"word": "world.", "start": 0.9, "end": 1.7}]
SEGMENTS = [{"id": 0, "start": 0.12, "end": 1.7, "text": "Hello world."}]
UNSUPPORTED = "Unsupported transcription fields; streaming and prompts are not supported"
FORMAT = "Invalid response format or timestamp granularity"
AUDIO = ("file", b"audio", "a.wav")


def field(name, value):
    return (name, value, None)


@pytest.fixture
def sdk(api, no_proxy):
    with httpx.Client(trust_env=False, timeout=60) as http:
        yield OpenAI(api_key=API_KEY, base_url=f"{api.base}/v1", max_retries=0, http_client=http)


def served(api, call, result=None, options=None):
    """Run a blocking /v1 call while acting as the worker; return the call's value."""
    files = api.audio_files()
    with ThreadPoolExecutor(1) as pool:
        pending = pool.submit(call)
        claim = api.wait_claim()
        assert claim["options"] == (options or {})
        assert api.fetch_audio(claim).content == b"audio"
        api.finish(claim, result or sample_result())
        value = pending.result(timeout=30)
    # The synchronous request owns its job: nothing stays queued, stored or on disk.
    assert wait_until(lambda: api.client.get(f"/v2/transcript/{claim['id']}").status_code == 404, 5)
    assert wait_until(lambda: api.audio_files() == files, 5)
    return value


CASES = {
    "json": ({}, "application/json", {"text": "Hello world."}),
    "text": ({"response_format": "text"}, "text/plain", "Hello world."),
    "srt": ({"response_format": "srt"}, "text/plain", SRT),
    "vtt": ({"response_format": "vtt"}, "text/vtt", VTT),
    "verbose": ({"response_format": "verbose_json"}, "application/json", {**VERBOSE, "segments": SEGMENTS}),
    "verbose words": (
        {"response_format": "verbose_json", "timestamp_granularities": ["word"]},
        "application/json",
        {**VERBOSE, "words": WORDS},
    ),
}


@pytest.mark.parametrize("case", CASES)
def test_openai_sdk_response_formats(api, sdk, case):
    arguments, media_type, expected = CASES[case]
    create = sdk.audio.transcriptions.with_raw_response.create
    raw = served(api, lambda: create(model="parakeet", file=("test.wav", b"audio"), **arguments))
    response = raw.http_response
    assert response.status_code == 200 and response.headers["content-type"].split(";")[0] == media_type
    if isinstance(expected, str):
        assert response.text == expected and raw.parse() == expected
    else:
        assert response.json() == expected and raw.parse().text == "Hello world."


def test_openai_sdk_word_and_segment_timestamps(api, sdk):
    output = served(
        api,
        lambda: sdk.audio.transcriptions.create(
            model="whisper-1",
            file=("test.wav", b"audio"),
            language="de",
            temperature=0,
            response_format="verbose_json",
            timestamp_granularities=["word", "segment"],
        ),
        options={"language_code": "de"},
    )
    assert [(w.word, w.start, w.end) for w in output.words] == [("Hello", 0.12, 0.61), ("world.", 0.9, 1.7)]
    assert [(s.id, s.start, s.end, s.text) for s in output.segments] == [(0, 0.12, 1.7, "Hello world.")]
    assert output.duration == 2.1


def test_segments_follow_caption_rules(api):
    # Ten 9-character words: the ninth would push the first segment past 80 characters.
    words = [f"word{i}abcd" for i in range(10)] + ["fourteen"]
    result = timed_result(words, duration_ms=9000)
    # A gap above 1.5 s starts a new segment.
    result["words"][-1].update(start=5000, end=5333)
    body, headers = multipart([AUDIO, field("response_format", "verbose_json")])
    response = served(api, lambda: api.client.post(PATH, content=body, headers=headers), result)
    assert response.status_code == 200
    assert response.json() == {
        "task": "transcribe",
        "duration": 9.0,
        "text": result["text"],
        "segments": [
            {"id": 0, "start": 0.0, "end": 0.79, "text": " ".join(words[:8])},
            {"id": 1, "start": 0.8, "end": 0.99, "text": " ".join(words[8:10])},
            {"id": 2, "start": 5.0, "end": 5.333, "text": "fourteen"},
        ],
    }


def test_ten_fields_may_follow_the_file(api):
    fields = [("model", "parakeet-tdt-0.6b-v3"), ("language", "it"), ("temperature", "0.0")]
    fields += [("timestamp_granularities[]", g) for g in ("word", "segment") * 3] + [
        ("response_format", "text")
    ]
    body, headers = multipart([AUDIO, *(field(name, value) for name, value in fields)])
    response = served(
        api, lambda: api.client.post(PATH, content=body, headers=headers), options={"language_code": "it"}
    )
    assert response.status_code == 200 and response.text == "Hello world."


REJECTIONS = {
    "prompt": ([AUDIO, field("prompt", "Hello")], 422, UNSUPPORTED),
    "stream": ([field("stream", "true"), AUDIO], 422, UNSUPPORTED),
    "unknown model": ([field("model", "gpt-4o-transcribe"), AUDIO], 400, "Unknown model; use parakeet"),
    "unknown model after file": ([AUDIO, field("model", "large-v3")], 400, "Unknown model; use parakeet"),
    "temperature": ([AUDIO, field("temperature", "0.2")], 422, "Only greedy decoding is supported"),
    "response format": ([field("response_format", "diarized_json"), AUDIO], 400, FORMAT),
    "granularity": ([AUDIO, field("timestamp_granularities[]", "char")], 400, FORMAT),
    "language": ([AUDIO, field("language", "xx")], 400, "Invalid transcription options or audio"),
    "missing file": ([field("model", "parakeet")], 400, "file must be an audio file"),
    "file as plain field": ([field("file", "audio")], 400, "file must be an audio file"),
    "empty file": ([("file", b"", "a.wav")], 400, "Audio is empty"),
    "file too large": ([("file", b"x" * 4097, "a.wav")], 413, "Audio too large"),
    "duplicate model": ([field("model", "parakeet"), field("model", "parakeet"), AUDIO], 400, None),
    "second file": ([AUDIO, ("extra", b"audio", "b.wav")], 400, None),
    "oversized field before file": ([field("model", "x" * 66000), AUDIO], 400, None),
    "oversized field after file": ([AUDIO, field("model", "x" * 66000)], 400, None),
    "eleven fields": ([AUDIO, *[field("timestamp_granularities[]", "word")] * 11], 400, None),
}
NOT_MULTIPART = {
    "missing boundary": (b"--x\r\n", "multipart/form-data"),
    "urlencoded": (b"model=parakeet&file=audio", "application/x-www-form-urlencoded"),
    "json": (b'{"model": "parakeet", "file": "audio"}', "application/json"),
    "raw audio": (b"audio", "audio/wav"),
    "no content type": (b"audio", None),
    "truncated body": (multipart([AUDIO])[0][:-10], multipart([AUDIO])[1]["content-type"]),
}


@pytest.mark.parametrize("case", [*REJECTIONS, *NOT_MULTIPART])
def test_invalid_requests_are_rejected_without_a_job(api, case):
    if case in REJECTIONS:
        parts, status, message = REJECTIONS[case]
        body, headers = multipart(parts)
    else:
        (body, content_type), status, message = NOT_MULTIPART[case], 400, None
        headers = {"content-type": content_type} if content_type else {}
    files = api.audio_files()
    response = api.client.post(PATH, content=body, headers=headers)
    assert response.status_code == status, response.text
    assert error_message(response, v1=True) == message or message is None
    assert api.audio_files() == files
    assert api.claim() is None


def test_worker_retry_keeps_waiting_and_errors_are_422(api):
    files = api.audio_files()
    with ThreadPoolExecutor(1) as pool:
        pending = pool.submit(api.client.post, PATH, files={"file": ("a.wav", b"audio")})
        first = api.wait_claim()
        assert api.complete(first, {"error": "Transient", "retry": True}).status_code == 200
        second = api.wait_claim()
        assert second["id"] == first["id"]
        assert api.complete(second, {"error": "Audio could not be decoded"}).status_code == 200
        response = pending.result(timeout=30)
    assert response.status_code == 422
    assert error_message(response, v1=True) == "Audio could not be decoded"
    assert wait_until(lambda: api.client.get(f"/v2/transcript/{first['id']}").status_code == 404, 5)
    assert api.audio_files() == files


def test_client_disconnect_cancels_the_job(api):
    files = api.audio_files()
    body, headers = multipart([AUDIO])
    request = RawRequest(api, "POST", PATH, {**headers, "Content-Length": str(len(body))})
    request.send(body)
    try:
        assert wait_until(lambda: api.metrics().get("queued") == 1, 10)
        assert len(api.audio_files() - files) == 1
    finally:
        request.close()
    # The waiting request notices the disconnect, deletes its job and releases the audio.
    assert wait_until(lambda: not api.metrics().get("queued"), 10)
    assert wait_until(lambda: api.audio_files() == files, 10)
    assert api.claim() is None
