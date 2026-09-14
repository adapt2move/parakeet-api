"""AssemblyAI-compatible /v2 API: upload, submit, poll, captions, delete and audio URL validation."""

import io
import json
import re
import uuid

import pytest

from .support import API_KEY, INVALID, UUID_PATTERN, error_message, timed_result

LANGUAGES = "bg hr cs da nl en et fi fr de el hu it lv lt mt pl pt ro ru sk sl es sv uk".split()
UNTRUSTED = "Upload audio first, or configure a trusted AUDIO_URL_HOSTS origin"
BAD_URL = "Invalid audio URL"
EMPTY = dict.fromkeys(
    ("error", "text", "words", "confidence", "audio_duration", "language_confidence", "utterances")
)
SRT = "1\n00:00:00,120 --> 00:00:01,700\nHello world.\n"
VTT = "WEBVTT\n\n1\n00:00:00.120 --> 00:00:01.700\nHello world.\n"


def completed(api, result):
    job, _ = api.queue()
    api.finish(api.claim(), result)
    return job["id"]


def test_transcript_cycle(api, result):
    response = api.client.post("/v2/upload", content=b"audio", headers={"content-type": "audio/wav"})
    assert response.status_code == 200 and set(response.json()) == {"upload_url"}
    url = response.json()["upload_url"]
    uid = re.fullmatch(re.escape(api.base) + f"/uploads/({UUID_PATTERN})", url).group(1)
    assert (api.audio / uid).read_bytes() == b"audio"

    job = api.submit(url, language_code="en")
    assert job == {**EMPTY, "id": job["id"], "status": "queued", "audio_url": url, "language_code": "en"}
    assert re.fullmatch(UUID_PATTERN, job["id"]) and api.transcript(job["id"]) == job

    api.finish(api.claim(), result)
    transcript = api.transcript(job["id"])
    words = [{**word, "speaker": None, "channel": None} for word in result["words"]]
    assert transcript == {
        **job,
        "status": "completed",
        "text": "Hello world.",
        "words": words,
        "confidence": pytest.approx(0.75),
        "audio_duration": 3,
    }
    for extension, media_type, text in (("vtt", "text/vtt", VTT), ("srt", "application/x-subrip", SRT)):
        captions = api.client.get(f"/v2/transcript/{job['id']}/{extension}")
        assert captions.status_code == 200 and captions.text == text
        assert captions.headers["content-type"].split(";")[0] == media_type

    deleted = api.client.delete(f"/v2/transcript/{job['id']}")
    assert deleted.status_code == 200 and deleted.json() == {**transcript, "text": None, "words": None}
    assert uid not in api.audio_files()
    for response in (
        api.client.get(f"/v2/transcript/{job['id']}"),
        api.client.delete(f"/v2/transcript/{job['id']}"),
        api.client.get(f"/v2/transcript/{job['id']}/vtt"),
        api.client.get(f"/v2/transcript/{uuid.uuid4()}/srt"),
        api.client.get("/v2/transcript/not-a-uuid"),
    ):
        assert response.status_code == 404 and error_message(response) == "Transcript missing or expired"


def test_audio_duration_and_confidence(api):
    for duration_ms, seconds in ((1, 1), (999, 1), (1000, 1), (2001, 3), (10_800_000, 10_800)):
        transcript = api.transcript(completed(api, timed_result([], duration_ms=duration_ms)))
        assert transcript["audio_duration"] == seconds
        assert transcript["confidence"] is None and transcript["words"] == []

    confidences = [0.1, 0.2, 0.4, 1e-07, 0.3333333333333333]
    result = timed_result([f"w{i}" for i in range(5)])
    for word, confidence in zip(result["words"], confidences):
        word["confidence"] = confidence
    transcript = api.transcript(completed(api, result))
    assert transcript["confidence"] == pytest.approx(sum(confidences) / len(confidences))
    assert [w["confidence"] for w in transcript["words"]] == confidences


def test_captions_split_escape_and_format_hours(api):
    timings = [
        ("Hello", 0, 400),
        ("world.", 500, 1000),
        # A gap above 1.5 s starts a new caption.
        ("a<b", 2600, 3000),
        ("&", 3100, 3500),
        ("c>", 3600, 4000),
        ("long", 4100, 8000),
        # A caption never spans more than 6 s.
        ("tail", 8100, 9100),
        ("hour", 3_723_004, 3_723_500),
    ]
    result = timed_result([text for text, _, _ in timings], duration_ms=3_724_000)
    for word, (_, start, end) in zip(result["words"], timings):
        word.update(start=start, end=end)
    jid = completed(api, result)
    cues = [
        ("00:00:00{}000", "00:00:01{}000", "Hello world."),
        ("00:00:02{}600", "00:00:08{}000", "a&lt;b &amp; c&gt; long"),
        ("00:00:08{}100", "00:00:09{}100", "tail"),
        ("01:02:03{}004", "01:02:03{}500", "hour"),
    ]

    def render(sep):
        return "\n".join(
            f"{i + 1}\n{a.format(sep)} --> {b.format(sep)}\n{t}\n" for i, (a, b, t) in enumerate(cues)
        )

    assert api.client.get(f"/v2/transcript/{jid}/srt").text == render(",")
    assert api.client.get(f"/v2/transcript/{jid}/vtt").text == "WEBVTT\n\n" + render(".")


def test_chars_per_caption(api):
    jid = completed(api, timed_result("one two three four five six seven eight nine ten".split()))
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

    for value in (19, 201, 0, -5, "abc", "20.5"):
        response = api.client.get(f"/v2/transcript/{jid}/srt", params={"chars_per_caption": value})
        assert response.status_code == 400, value
        assert error_message(response) == "chars_per_caption must be 20..200"


def test_unfinished_transcripts(api):
    job, uid = api.queue(language_code="fr")
    for extension in ("srt", "vtt"):
        response = api.client.get(f"/v2/transcript/{job['id']}/{extension}")
        assert response.status_code == 409 and error_message(response) == "Transcript is not complete"
    deleted = api.client.delete(f"/v2/transcript/{job['id']}")
    # AssemblyAI reports deleted transcripts as completed with their content cleared.
    assert deleted.status_code == 200 and deleted.json() == {**job, "status": "completed"}
    assert uid not in api.audio_files()
    assert api.client.get(f"/v2/transcript/{job['id']}").status_code == 404


def test_supported_languages(api):
    for language in LANGUAGES:
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
        {"audio_url": [url]},
        {"audio_url": url, "language_code": 5},
        [url],
    ):
        response = api.client.post("/v2/transcript", json=body)
        assert response.status_code == 422 and error_message(response) == INVALID, body
        assert url not in response.text
    trailing = json.dumps({"audio_url": url}).encode() + b" {}"
    for content in (b"{", b"", b"null", b"[]", b"{'audio_url': 1}", trailing):
        response = api.client.post("/v2/transcript", content=content)
        assert response.status_code == 422 and error_message(response) == INVALID, content
    for language in ("xx", "EN", "en-US", "auto"):
        response = api.client.post("/v2/transcript", json={"audio_url": url, "language_code": language})
        assert response.status_code == 400
        assert error_message(response) == "Invalid transcription options or audio"
    assert api.audio_files() == files

    # The content type is not inspected, and an empty language code means unset.
    body = json.dumps({"audio_url": url, "language_code": ""})
    response = api.client.post("/v2/transcript", content=body, headers={"content-type": "text/plain"})
    assert response.status_code == 200, response.text
    assert response.json()["language_code"] is None
    assert api.client.delete(f"/v2/transcript/{response.json()['id']}").status_code == 200


URL_REJECTIONS = r"""
    http://audio.invalid/a.wav ftp://audio.invalid/a.wav file:///etc/passwd //audio.invalid/a.wav
    audio.invalid/a.wav https:audio.invalid/a.wav https://evil.invalid/a.wav
    https://audio.invalid.evil.invalid/a.wav https://169.254.169.254/latest/meta-data
    https://audio.invalid:8443/a.wav https://audio.invalid:80/a.wav https://user@audio.invalid/a.wav
    https://user:secret@audio.invalid/a.wav https://:secret@audio.invalid/a.wav
    https://audio.invalid@evil.invalid/a.wav https://evil.invalid\@audio.invalid/a.wav
    https://audio.invalid/a.wav#fragment
""".split()
# Control characters, spaces and non-ASCII are rejected before the URL is parsed.
CONTROL_REJECTIONS = [
    "https://audio.invalid/\x00",
    "https://audio.invalid/a\x7f",
    "https://audio.invalid/a\n.wav",
    "https://audio.invalid/a\r.wav",
    "https://audio\t.invalid/a.wav",
    "https://audio.invalid/a b.wav",
    " https://audio.invalid/a.wav",
    "https://audío.invalid/a.wav",
    "https://audio.invalid/café.wav",
]


@pytest.mark.parametrize("url", URL_REJECTIONS + CONTROL_REJECTIONS)
def test_audio_url_rejections(api, url):
    files = api.audio_files()
    response = api.client.post("/v2/transcript", json={"audio_url": url})
    assert response.status_code == 400
    message = error_message(response)
    assert message == BAD_URL if url in CONTROL_REJECTIONS else message in (UNTRUSTED, BAD_URL)
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
        prefix + uid + "/extra",
        prefix + uid + "?download=1",
        prefix + uid + "#x",
        prefix,
        # Another origin is not this server's upload URL, even with the same path.
        f"http://localhost:{api.port}/uploads/{uid}",
    ):
        response = api.client.post("/v2/transcript", json={"audio_url": candidate})
        assert response.status_code == 400, candidate
        assert error_message(response) in ("Invalid upload URL", UNTRUSTED, BAD_URL)
    missing = api.client.post("/v2/transcript", json={"audio_url": prefix + str(uuid.uuid4())})
    assert missing.status_code == 400 and error_message(missing) == "Upload missing or expired"
    assert api.audio_files() == files
    job = api.submit(url)
    assert api.client.delete(f"/v2/transcript/{job['id']}").status_code == 200


def test_trusted_audio_url_failures_are_generic(api):
    files = api.audio_files()
    for url in ("https://audio.invalid/a.wav", "https://AUDIO.invalid:443/a.wav?x=1"):
        response = api.client.post("/v2/transcript", json={"audio_url": url})
        assert response.status_code == 400 and error_message(response) == "Audio download failed"
    assert api.audio_files() == files


def test_assemblyai_sdk_round_trip(api, result, monkeypatch, no_proxy):
    import assemblyai as aai

    settings = aai.Settings(api_key=API_KEY, base_url=api.base, polling_interval=0.05)
    monkeypatch.setattr(aai, "settings", settings)
    monkeypatch.setattr(aai.Client, "_default", None)
    sdk = aai.Client(settings=settings)
    try:
        transcriber = aai.Transcriber(client=sdk)
        config = aai.TranscriptionConfig(language_code="en")
        transcript = transcriber.submit(io.BytesIO(b"sdk audio"), config=config)
        assert transcript.status == aai.TranscriptStatus.queued
        claim = api.claim()
        assert claim["options"] == {"language_code": "en"} and claim["bytes"] == 9
        assert api.fetch_audio(claim).content == b"sdk audio"
        api.finish(claim, result)

        transcript = transcript.wait_for_completion()
        assert transcript.status == aai.TranscriptStatus.completed and transcript.text == "Hello world."
        assert transcript.words[0].start == 120 and transcript.words[1].end == 1700
        assert transcript.confidence == pytest.approx(0.75) and transcript.audio_duration == 3
        assert transcript.export_subtitles_vtt() == VTT
        assert transcript.export_subtitles_srt(chars_per_caption=20) == SRT
        assert aai.Transcript.get_by_id(transcript.id).text == "Hello world."
        assert aai.Transcript.delete_by_id(transcript.id).text is None
        assert api.client.get(f"/v2/transcript/{transcript.id}").status_code == 404

        failing = transcriber.submit(io.BytesIO(b"noise"))
        assert api.complete(api.claim(), {"error": "Audio could not be decoded"}).status_code == 200
        failing = failing.wait_for_completion()
        assert failing.status == aai.TranscriptStatus.error and failing.error == "Audio could not be decoded"
    finally:
        sdk.http_client.close()
        if aai.Client._default is not None:
            aai.Client._default.http_client.close()
