"""Process-level behavior: settings validation, data directory ownership and Go-only startup changes."""

import subprocess
import uuid

import pytest

from .support import API_KEY, free_port, skip_unless_go

INVALID_SETTINGS = {
    "missing client key": {"API_KEY": None},
    "short client key": {"API_KEY": "x" * 31},
    "short worker key": {"WORKER_API_KEY": "y" * 31},
    "equal keys": {"WORKER_API_KEY": API_KEY},
    "zero upload bytes": {"MAX_UPLOAD_BYTES": 0},
    "zero storage": {"MAX_STORAGE_BYTES": 0},
    "storage below upload": {"MAX_UPLOAD_BYTES": 4096, "MAX_STORAGE_BYTES": 4095},
    "zero pending jobs": {"MAX_PENDING_JOBS": 0},
    "negative retention": {"RETENTION_SECONDS": -1},
    "zero job age": {"MAX_JOB_AGE_SECONDS": 0},
    "short lease": {"LEASE_SECONDS": 14},
    "zero attempts": {"MAX_ATTEMPTS": 0},
    "zero sync timeout": {"SYNC_TIMEOUT_SECONDS": 0},
    "zero upload slots": {"MAX_CONCURRENT_UPLOADS": 0},
    "zero idle": {"UPLOAD_IDLE_SECONDS": 0},
    "zero rate": {"MIN_UPLOAD_BYTES_PER_SECOND": 0},
    "non-numeric limit": {"MAX_UPLOAD_BYTES": "lots"},
    "host with scheme": {"AUDIO_URL_HOSTS": "https://audio.invalid"},
    "host with port": {"AUDIO_URL_HOSTS": "audio.invalid:443"},
    "host with path": {"AUDIO_URL_HOSTS": "audio.invalid/a"},
    "host with userinfo": {"AUDIO_URL_HOSTS": "user@audio.invalid"},
    "bracketed host": {"AUDIO_URL_HOSTS": "[::1]"},
    "non-ascii host": {"AUDIO_URL_HOSTS": "audío.invalid"},
    "host with inner space": {"AUDIO_URL_HOSTS": "audio .invalid"},
}


def refused(launcher, **overrides):
    server = launcher.spawn(**overrides)
    try:
        code = server.process.wait(60)
    except subprocess.TimeoutExpired:
        pytest.fail(f"server kept running:\n{server.logs()[-2000:]}")
    finally:
        server.stop()
    assert code != 0
    return server.logs()


@pytest.mark.parametrize("case", INVALID_SETTINGS)
def test_invalid_settings_refuse_to_start(launcher, case):
    refused(launcher, **INVALID_SETTINGS[case])


def test_audio_url_hosts_are_normalized(start):
    server = start(AUDIO_URL_HOSTS=" Audio.INVALID , ,cdn.invalid,", UPLOAD_IDLE_SECONDS="0.5")
    for url in ("https://audio.invalid/a.wav", "https://CDN.invalid/a.wav"):
        response = server.client.post("/v2/transcript", json={"audio_url": url})
        assert response.status_code == 400
        assert response.json() == {"error": "Audio download failed"}
    other = server.client.post("/v2/transcript", json={"audio_url": "https://other.invalid/a.wav"})
    assert other.json() == {"error": "Upload audio first, or configure a trusted AUDIO_URL_HOSTS origin"}


def test_without_audio_url_hosts_only_uploads_are_accepted(start):
    server = start(AUDIO_URL_HOSTS=None)
    response = server.client.post("/v2/transcript", json={"audio_url": "https://audio.invalid/a.wav"})
    assert response.status_code == 400
    assert response.json() == {"error": "Upload audio first, or configure a trusted AUDIO_URL_HOSTS origin"}
    job, _ = server.queue()
    assert server.client.delete(f"/v2/transcript/{job['id']}").status_code == 200


def test_startup_discards_leftover_audio(launcher, tmp_path):
    data = tmp_path / "data"
    (data / "audio").mkdir(parents=True)
    leftovers = [data / "audio" / str(uuid.uuid4()), data / "audio" / "crash-orphan"]
    for path in leftovers:
        path.write_bytes(b"audio")
    server = launcher.start(data=data)
    try:
        assert server.audio_files() == set()
        assert (data / "api.lock").exists()
        assert server.metrics() == {}
    finally:
        server.stop()


def test_second_process_on_the_same_data_dir_refuses_to_start(launcher, tmp_path):
    data = tmp_path / "data"
    first = launcher.start(data=data)
    try:
        url, uid = first.upload()
        refused(launcher, data=data)
        # The refused process must not have touched the running server's files.
        assert uid in first.audio_files()
        assert first.anonymous.get("/health/ready").status_code == 200
        job = first.submit(url)
        assert job["status"] == "queued"
        assert first.client.delete(f"/v2/transcript/{job['id']}").status_code == 200
    finally:
        first.stop()


def test_memory_db_mode_is_accepted(start):
    server = start(DB_MODE="memory")
    assert server.anonymous.get("/health/live").json() == {"status": "ok"}


@skip_unless_go
def test_empty_db_mode_is_accepted(start):
    server = start(DB_MODE="")
    assert server.anonymous.get("/health/live").json() == {"status": "ok"}


@skip_unless_go
@pytest.mark.parametrize("mode", ["file", "sqlite"])
def test_file_db_mode_was_removed(launcher, mode):
    logs = refused(launcher, DB_MODE=mode)
    assert "DB_MODE" in logs and "file" in logs.lower()


@skip_unless_go
def test_healthcheck_subcommand(launcher, start):
    binary = launcher.command(0)[0]
    server = start()
    env = {**server.env}
    healthy = subprocess.run([binary, "healthcheck"], env=env, capture_output=True, timeout=10)
    assert healthy.returncode == 0, healthy
    env["LISTEN_ADDR"] = f"127.0.0.1:{free_port()}"
    unhealthy = subprocess.run([binary, "healthcheck"], env=env, capture_output=True, timeout=10)
    assert unhealthy.returncode == 1
    # Only the port matters: the check always targets the loopback address.
    env["LISTEN_ADDR"] = f":{server.port}"
    assert subprocess.run([binary, "healthcheck"], env=env, capture_output=True, timeout=10).returncode == 0
