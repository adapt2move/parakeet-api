"""AssemblyAI-compatible /v2 API: upload, submit, poll, captions, delete and audio URL validation."""

import io
import re
import uuid

import pytest

from .support import API_KEY, UUID_PATTERN, error_message, sample_result

TRANSCRIPT_KEYS = {
    "id",
    "status",
    "error",
    "audio_url",
    "text",
    "words",
    "confidence",
    "audio_duration",
    "language_code",
    "language_confidence",
    "utterances",
}
LANGUAGES = "bg hr cs da nl en et fi fr de el hu it lv lt mt pl pt ro ru sk sl es sv uk".split()
INVALID = "Invalid or unsupported request fields"
UNTRUSTED = "Upload audio first, or configure a trusted AUDIO_URL_HOSTS origin"


def media_type(response):
    return response.headers["content-type"].split(";")[0].strip()


def test_upload_returns_a_public_upload_url(api):
    response = api.client.post("/v2/upload", content=b"audio", headers={"content-type": "audio/wav"})
    assert response.status_code == 200
    body = response.json()
    assert set(body) == {"upload_url"}
    match = re.fullmatch(re.escape(api.base) + r"/uploads/(" + UUID_PATTERN + ")", body["upload_url"])
    assert match, body
    assert (api.audio / match.group(1)).read_bytes() == b"audio"


def test_transcript_cycle(api, result):
    url, uid = api.upload()
    job = api.submit(url, language_code="en")
    assert job == {
        "id": job["id"],
        "status": "queued",
        "error": None,
        "audio_url": url,
        "text": None,
        "words": None,
        "confidence": None,
        "audio_duration": None,
        "language_code": "en",
        "language_confidence": None,
        "utterances": None,
    }
    assert re.fullmatch(UUID_PATTERN, job["id"])
    assert api.client.get(f"/v2/transcript/{job['id']}").json() == job

    api.finish(api.claim(), result)
    transcript = api.client.get(f"/v2/transcript/{job['id']}")
    assert transcript.status_code == 200
    assert transcript.json() == {
        **job,
        "status": "completed",
        "text": "Hello world.",
        "words": [{**word, "speaker": None, "channel": None} for word in result["words"]],
        "confidence": 0.75,
        "audio_duration": 3,
    }

    vtt = api.client.get(f"/v2/transcript/{job['id']}/vtt")
    assert vtt.status_code == 200 and media_type(vtt) == "text/vtt"
    assert vtt.text == "WEBVTT\n\n1\n00:00:00.120 --> 00:00:01.700\nHello world.\n"
    srt = api.client.get(f"/v2/transcript/{job['id']}/srt")
    assert srt.status_code == 200 and media_type(srt) == "application/x-subrip"
    assert srt.text == "1\n00:00:00,120 --> 00:00:01,700\nHello world.\n"

    deleted = api.client.delete(f"/v2/transcript/{job['id']}")
    assert deleted.status_code == 200
    assert deleted.json() == {**transcript.json(), "text": None, "words": None}
    for response in (
        api.client.get(f"/v2/transcript/{job['id']}"),
        api.client.delete(f"/v2/transcript/{job['id']}"),
        api.client.get(f"/v2/transcript/{job['id']}/vtt"),
    ):
        assert response.status_code == 404
        assert error_message(response) == "Transcript missing or expired"
    assert uid not in api.audio_files()


@pytest.mark.parametrize(
    ("duration_ms", "seconds"), [(1, 1), (999, 1), (1000, 1), (2000, 2), (2001, 3), (10_800_000, 10_800)]
)
def test_audio_duration_rounds_up_to_whole_seconds(api, duration_ms, seconds):
    job, _ = api.queue()
    result = {"text": "", "words": [], "audio_duration_ms": duration_ms, "chunks": 1, "seam_fallbacks": 0}
    api.finish(api.claim(), result)
    transcript = api.client.get(f"/v2/transcript/{job['id']}").json()
    assert transcript["audio_duration"] == seconds
    assert transcript["confidence"] is None and transcript["words"] == []


def test_confidence_is_the_mean_with_python_float_semantics(api):
    confidences = [0.1, 0.2, 0.4, 1e-07, 0.3333333333333333]
    words = [
        {"text": f"w{i}", "start": i * 10, "end": i * 10 + 5, "confidence": c}
        for i, c in enumerate(confidences)
    ]
    result = {
        "text": " ".join(w["text"] for w in words),
        "words": words,
        "audio_duration_ms": 100,
        "chunks": 1,
        "seam_fallbacks": 0,
    }
    job, _ = api.queue()
    api.finish(api.claim(), result)
    transcript = api.client.get(f"/v2/transcript/{job['id']}").json()
    expected = sum(confidences) / len(confidences)
    assert transcript["confidence"] == expected
    assert [w["confidence"] for w in transcript["words"]] == confidences


RICH = {
    "text": "Hello world. a<b & c> long tail hour",
    "words": [
        {"text": "Hello", "start": 0, "end": 400, "confidence": 0.9},
        {"text": "world.", "start": 500, "end": 1000, "confidence": 0.9},
        # A gap above 1.5 s starts a new caption.
        {"text": "a<b", "start": 2600, "end": 3000, "confidence": 0.9},
        {"text": "&", "start": 3100, "end": 3500, "confidence": 0.9},
        {"text": "c>", "start": 3600, "end": 4000, "confidence": 0.9},
        {"text": "long", "start": 4100, "end": 8000, "confidence": 0.9},
        # A caption never spans more than 6 s.
        {"text": "tail", "start": 8100, "end": 9100, "confidence": 0.9},
        {"text": "hour", "start": 3_723_004, "end": 3_723_500, "confidence": 0.9},
    ],
    "audio_duration_ms": 3_724_000,
    "chunks": 2,
    "seam_fallbacks": 0,
}
COUNTING = "one two three four five six seven eight nine ten".split()
COUNTED = {
    "text": " ".join(COUNTING),
    "words": [
        {"text": w, "start": i * 100, "end": i * 100 + 90, "confidence": 0.5} for i, w in enumerate(COUNTING)
    ],
    "audio_duration_ms": 1000,
    "chunks": 1,
    "seam_fallbacks": 0,
}


def completed(api, result):
    job, _ = api.queue()
    api.finish(api.claim(), result)
    return job["id"]


def test_captions_split_escape_and_format_hours(api):
    jid = completed(api, RICH)
    cues = [
        ("00:00:00{}000", "00:00:01{}000", "Hello world."),
        ("00:00:02{}600", "00:00:08{}000", "a&lt;b &amp; c&gt; long"),
        ("00:00:08{}100", "00:00:09{}100", "tail"),
        ("01:02:03{}004", "01:02:03{}500", "hour"),
    ]

    def render(separator):
        return "\n".join(
            f"{i + 1}\n{start.format(separator)} --> {end.format(separator)}\n{text}\n"
            for i, (start, end, text) in enumerate(cues)
        )

    assert api.client.get(f"/v2/transcript/{jid}/srt").text == render(",")
    assert api.client.get(f"/v2/transcript/{jid}/vtt").text == "WEBVTT\n\n" + render(".")


def test_chars_per_caption(api):
    jid = completed(api, COUNTED)
    default = api.client.get(f"/v2/transcript/{jid}/srt")
    assert (
        default.text == "1\n00:00:00,000 --> 00:00:00,990\none two three four five six seven eight nine ten\n"
    )
    narrow = api.client.get(f"/v2/transcript/{jid}/srt", params={"chars_per_caption": 20})
    assert narrow.status_code == 200
    assert narrow.text == (
        "1\n00:00:00,000 --> 00:00:00,390\none two three four\n\n"
        "2\n00:00:00,400 --> 00:00:00,790\nfive six seven eight\n\n"
        "3\n00:00:00,800 --> 00:00:00,990\nnine ten\n"
    )
    vtt = api.client.get(f"/v2/transcript/{jid}/vtt", params={"chars_per_caption": 20})
    assert vtt.text.startswith("WEBVTT\n\n1\n00:00:00.000 --> 00:00:00.390\none two three four\n\n2\n")
    wide = api.client.get(f"/v2/transcript/{jid}/vtt", params={"chars_per_caption": 200})
    assert wide.status_code == 200 and wide.text.count(" --> ") == 1

    for value in (19, 201, 0, -5):
        response = api.client.get(f"/v2/transcript/{jid}/srt", params={"chars_per_caption": value})
        assert response.status_code == 400
        assert error_message(response) == "chars_per_caption must be 20..200"
    for value in ("abc", "20.5"):
        response = api.client.get(f"/v2/transcript/{jid}/vtt", params={"chars_per_caption": value})
        assert response.status_code == 422
        assert error_message(response) == INVALID

    unknown = api.client.get(f"/v2/transcript/{jid}/txt")
    assert unknown.status_code == 404 and error_message(unknown) == "Unknown endpoint"


def test_captions_need_a_completed_transcript(api):
    job, _ = api.queue()
    for extension in ("srt", "vtt"):
        response = api.client.get(f"/v2/transcript/{job['id']}/{extension}")
        assert response.status_code == 409
        assert error_message(response) == "Transcript is not complete"
    missing = api.client.get(f"/v2/transcript/{uuid.uuid4()}/srt")
    assert missing.status_code == 404 and error_message(missing) == "Transcript missing or expired"
    assert api.client.delete(f"/v2/transcript/{job['id']}").status_code == 200


def test_delete_queued_transcript(api):
    job, uid = api.queue(language_code="fr")
    deleted = api.client.delete(f"/v2/transcript/{job['id']}")
    assert deleted.status_code == 200
    # AssemblyAI reports deleted transcripts as completed with their content cleared.
    assert deleted.json() == {**job, "status": "completed"}
    assert uid not in api.audio_files()
    assert api.client.get(f"/v2/transcript/{job['id']}").status_code == 404
    assert api.claim() is None


def test_unknown_transcript_ids(api):
    for jid in (str(uuid.uuid4()), "not-a-uuid"):
        for response in (api.client.get(f"/v2/transcript/{jid}"), api.client.delete(f"/v2/transcript/{jid}")):
            assert response.status_code == 404
            assert error_message(response) == "Transcript missing or expired"


@pytest.mark.parametrize("language", LANGUAGES)
def test_supported_languages(api, language):
    job, _ = api.queue(language_code=language)
    assert job["language_code"] == language
    assert api.client.delete(f"/v2/transcript/{job['id']}").status_code == 200


def test_submit_validation(api):
    url, _ = api.upload()
    files = api.audio_files()
    for body in (
        {"audio_url": url, "speaker_labels": True},
        {"audio_url": url, "webhook_url": "https://audio.invalid/hook"},
        {},
        {"language_code": "en"},
        {"audio_url": 5},
        {"audio_url": None},
        {"audio_url": url, "language_code": 5},
        {"audio_url": "https://audio.invalid/" + "a" * 8192},
        [url],
    ):
        response = api.client.post("/v2/transcript", json=body)
        assert response.status_code == 422, body
        assert error_message(response) == INVALID
        assert url not in response.text
    for content in (b"{", b"", b"null"):
        response = api.client.post(
            "/v2/transcript", content=content, headers={"content-type": "application/json"}
        )
        assert response.status_code == 422, content
        assert error_message(response) == INVALID
    for language in ("xx", "EN", "en-US", "auto"):
        response = api.client.post("/v2/transcript", json={"audio_url": url, "language_code": language})
        assert response.status_code == 400
        assert error_message(response) == "Invalid transcription options or audio"
    assert api.audio_files() == files
    job = api.submit(url, language_code=None)
    assert job["language_code"] is None
    assert api.client.delete(f"/v2/transcript/{job['id']}").status_code == 200


URL_REJECTIONS = [
    ("http://audio.invalid/a.wav", UNTRUSTED),
    ("ftp://audio.invalid/a.wav", UNTRUSTED),
    ("file:///etc/passwd", UNTRUSTED),
    ("//audio.invalid/a.wav", UNTRUSTED),
    ("audio.invalid/a.wav", UNTRUSTED),
    ("https://evil.invalid/a.wav", UNTRUSTED),
    ("https://audio.invalid.evil.invalid/a.wav", UNTRUSTED),
    ("https://169.254.169.254/latest/meta-data", UNTRUSTED),
    ("https://audio.invalid:8443/a.wav", UNTRUSTED),
    ("https://audio.invalid:80/a.wav", UNTRUSTED),
    ("https://user@audio.invalid/a.wav", UNTRUSTED),
    ("https://user:secret@audio.invalid/a.wav", UNTRUSTED),
    ("https://:secret@audio.invalid/a.wav", UNTRUSTED),
    ("https://audio.invalid@evil.invalid/a.wav", UNTRUSTED),
    ("https://evil.invalid\\@audio.invalid/a.wav", UNTRUSTED),
    ("https://audio.invalid/a.wav#fragment", UNTRUSTED),
    ("https://audio.invalid/\x00", "Invalid audio URL"),
    ("https://audio.invalid/a\x7f", "Invalid audio URL"),
    ("https://audio.invalid/a\n.wav", "Invalid audio URL"),
    ("https://audio.invalid/a\r.wav", "Invalid audio URL"),
    ("https://audio\t.invalid/a.wav", "Invalid audio URL"),
    ("https://audio.invalid/a b.wav", "Invalid audio URL"),
    (" https://audio.invalid/a.wav", "Invalid audio URL"),
    ("https://audío.invalid/a.wav", "Invalid audio URL"),
    ("https://audio.invalid/ ", "Invalid audio URL"),
]


@pytest.mark.parametrize(("url", "message"), URL_REJECTIONS)
def test_audio_url_rejections(api, url, message):
    files = api.audio_files()
    response = api.client.post("/v2/transcript", json={"audio_url": url})
    assert response.status_code == 400
    assert error_message(response) == message
    assert "evil" not in response.text and "secret" not in response.text
    assert api.audio_files() == files


def test_own_upload_urls_must_be_canonical(api):
    url, uid = api.upload()
    files = api.audio_files()
    prefix = f"{api.base}/uploads/"
    for candidate in (
        prefix + uid.upper(),
        prefix + uid.replace("-", ""),
        prefix + "{" + uid + "}",
        prefix + "urn:uuid:" + uid,
        prefix + uid + "/extra",
        prefix + uid + "?download=1",
        prefix + uid + "#x",
        prefix + "not-a-uuid",
        prefix,
    ):
        response = api.client.post("/v2/transcript", json={"audio_url": candidate})
        assert response.status_code == 400, candidate
        assert error_message(response) == "Invalid upload URL"
    missing = api.client.post("/v2/transcript", json={"audio_url": prefix + str(uuid.uuid4())})
    assert missing.status_code == 400 and error_message(missing) == "Upload missing or expired"
    # Another origin is not this server's upload URL, even with the same path.
    foreign = api.client.post(
        "/v2/transcript", json={"audio_url": f"http://localhost:{api.port}/uploads/{uid}"}
    )
    assert foreign.status_code == 400 and error_message(foreign) == UNTRUSTED
    assert api.audio_files() == files
    job = api.submit(url)
    assert api.client.delete(f"/v2/transcript/{job['id']}").status_code == 200


@pytest.mark.parametrize("url", ["https://audio.invalid/a.wav", "https://AUDIO.invalid:443/a.wav?x=1"])
def test_trusted_audio_url_failures_are_generic(api, url):
    files = api.audio_files()
    response = api.client.post("/v2/transcript", json={"audio_url": url})
    assert response.status_code == 400
    assert error_message(response) == "Audio download failed"
    assert api.audio_files() == files


def test_assemblyai_sdk_upload_submit_poll_and_delete(api, monkeypatch, no_proxy):
    import assemblyai as aai

    settings = aai.Settings(api_key=API_KEY, base_url=api.base, polling_interval=0.05)
    monkeypatch.setattr(aai, "settings", settings)
    monkeypatch.setattr(aai.Client, "_default", None)
    sdk = aai.Client(settings=settings)
    try:
        transcriber = aai.Transcriber(client=sdk)
        transcript = transcriber.submit(
            io.BytesIO(b"sdk audio"), config=aai.TranscriptionConfig(language_code="en")
        )
        assert transcript.status == aai.TranscriptStatus.queued
        claim = api.claim()
        assert claim["options"] == {"language_code": "en"} and claim["bytes"] == 9
        assert api.fetch_audio(claim).content == b"sdk audio"
        api.finish(claim, sample_result())

        transcript = transcript.wait_for_completion()
        assert transcript.status == aai.TranscriptStatus.completed
        assert transcript.text == "Hello world."
        assert transcript.words[0].start == 120 and transcript.words[1].end == 1700
        assert transcript.confidence == 0.75
        assert transcript.audio_duration == 3
        assert (
            transcript.export_subtitles_vtt() == "WEBVTT\n\n1\n00:00:00.120 --> 00:00:01.700\nHello world.\n"
        )
        assert transcript.export_subtitles_srt(chars_per_caption=20) == (
            "1\n00:00:00,120 --> 00:00:01,700\nHello world.\n"
        )

        polled = aai.Transcript.get_by_id(transcript.id)
        assert polled.text == "Hello world."
        deleted = aai.Transcript.delete_by_id(transcript.id)
        assert deleted.text is None
        assert api.client.get(f"/v2/transcript/{transcript.id}").status_code == 404
    finally:
        sdk.http_client.close()
        if aai.Client._default is not None:
            aai.Client._default.http_client.close()


def test_assemblyai_sdk_reports_worker_errors(api, no_proxy):
    import assemblyai as aai

    settings = aai.Settings(api_key=API_KEY, base_url=api.base, polling_interval=0.05)
    sdk = aai.Client(settings=settings)
    try:
        transcript = aai.Transcriber(client=sdk).submit(io.BytesIO(b"noise"))
        claim = api.claim()
        assert api.complete(claim, {"error": "Audio could not be decoded"}).status_code == 200
        transcript = transcript.wait_for_completion()
        assert transcript.status == aai.TranscriptStatus.error
        assert transcript.error == "Audio could not be decoded"
        assert set(api.client.get(f"/v2/transcript/{transcript.id}").json()) == TRANSCRIPT_KEYS
    finally:
        sdk.http_client.close()
