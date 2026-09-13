import time
from concurrent.futures import ThreadPoolExecutor

from openai import OpenAI


def submit(client):
    uploaded = client.post("/v2/upload", content=b"audio")
    assert uploaded.status_code == 200
    job = client.post("/v2/transcript", json={"audio_url": uploaded.json()["upload_url"]})
    assert job.status_code == 200
    return job.json()


def complete(client, settings, result):
    headers = {"authorization": settings.worker_key}
    claim = client.post("/internal/jobs/claim", headers=headers).json()
    headers["x-lease-token"] = claim["token"]
    assert client.get(f"/internal/jobs/{claim['id']}/audio", headers=headers).content == b"audio"
    assert client.post(f"/internal/jobs/{claim['id']}/heartbeat", headers=headers).status_code == 200
    response = client.post(f"/internal/jobs/{claim['id']}/complete", headers=headers, json={"result": result})
    assert response.status_code == 200, response.text
    return claim


def test_assembly_cycle(client, settings, result):
    job = submit(client)
    assert job["status"] == "queued" and job["words"] is None
    complete(client, settings, result)
    transcript = client.get(f"/v2/transcript/{job['id']}").json()
    assert transcript["status"] == "completed"
    assert transcript["words"][0]["start"] == 120
    assert transcript["audio_duration"] == 3
    assert "00:00:00.120 --> 00:00:01.700" in client.get(f"/v2/transcript/{job['id']}/vtt").text
    assert client.delete(f"/v2/transcript/{job['id']}").status_code == 200
    assert client.get(f"/v2/transcript/{job['id']}").status_code == 404


def test_auth_roles_and_input_limits(client, settings):
    assert client.post("/v2/upload", content=b"x", headers={"authorization": "wrong"}).status_code == 401
    assert client.post("/internal/jobs/claim").status_code == 401
    assert (
        client.post("/v2/upload", content=b"x", headers={"authorization": settings.worker_key}).status_code
        == 401
    )
    assert client.post("/v2/upload", content=b"x" * 1025).status_code == 413
    assert client.post("/v2/upload", content=b"").status_code == 400
    assert not list(client.app.state.store.blobs.iterdir())
    assert client.post("/v2/transcript", json={"audio_url": "http://169.254.169.254/"}).status_code == 400
    assert (
        client.post(
            "/v2/transcript", json={"audio_url": "https://example.org/a", "speaker_labels": True}
        ).status_code
        == 422
    )


def test_rejects_invented_or_unordered_alignment(client, settings, result):
    submit(client)
    headers = {"authorization": settings.worker_key}
    claim = client.post("/internal/jobs/claim", headers=headers).json()
    headers["x-lease-token"] = claim["token"]
    result["words"][1]["start"] = 10
    assert (
        client.post(
            f"/internal/jobs/{claim['id']}/complete", headers=headers, json={"result": result}
        ).status_code
        == 422
    )


def test_openai_sdk_uses_word_seconds_and_deletes_job(client, settings, result):
    sdk = OpenAI(api_key=settings.api_key, base_url="http://testserver/v1", http_client=client, max_retries=0)
    with ThreadPoolExecutor() as pool:
        pending = pool.submit(
            sdk.audio.transcriptions.create,
            model="parakeet",
            file=("test.wav", b"audio"),
            response_format="verbose_json",
            timestamp_granularities=["word", "segment"],
        )
        for _ in range(100):
            if client.app.state.store.counts().get("queued"):
                break
            time.sleep(0.01)
        complete(client, settings, result)
        output = pending.result(timeout=5)
    assert output.words[0].start == 0.12
    assert output.words[1].end == 1.7
    assert output.duration == 2.1
    assert not client.app.state.store.counts()
    assert not list(client.app.state.store.blobs.iterdir())


def test_queued_requests_do_not_hold_upload_slots(client, settings, result):
    with ThreadPoolExecutor(3) as pool:
        pending = []
        for expected in range(1, 4):
            pending.append(
                pool.submit(
                    client.post,
                    "/v1/audio/transcriptions",
                    files={"file": ("a.wav", b"audio")},
                    data={"model": "parakeet"},
                )
            )
            for _ in range(200):
                if client.app.state.store.counts().get("queued") == expected:
                    break
                time.sleep(0.01)
            assert client.app.state.store.counts().get("queued") == expected
        for _ in range(3):
            complete(client, settings, result)
        assert all(p.result(timeout=5).status_code == 200 for p in pending)


def test_assemblyai_sdk_upload_submit_poll_and_subtitles(client, settings, result, monkeypatch):
    import assemblyai as aai

    sdk_client = aai.Client(settings=aai.Settings(api_key=settings.api_key, base_url="http://testserver"))
    sdk_client.http_client.close()
    sdk_client._http_client = client
    transcriber = aai.Transcriber(client=sdk_client)
    import io

    transcript = transcriber.submit(io.BytesIO(b"audio"), config=aai.TranscriptionConfig(language_code="en"))
    complete(client, settings, result)
    transcript = transcript.wait_for_completion()
    assert transcript.text == "Hello world."
    assert transcript.words[0].start == 120
    assert "WEBVTT" in transcript.export_subtitles_vtt()
    monkeypatch.setattr(aai, "settings", sdk_client.settings)
    monkeypatch.setattr(aai.Client, "_default", sdk_client)
    aai.Transcript.delete_by_id(transcript.id)
