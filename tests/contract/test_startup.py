"""Process-level behavior: settings validation, data directory ownership and startup."""

import subprocess
import uuid

import pytest

from .support import API_KEY, free_port

INVALID_SETTINGS = {
    "missing client key": ({"API_KEY": None}, None),
    "short client key": ({"API_KEY": "x" * 31}, None),
    "short worker key": ({"WORKER_API_KEY": "y" * 31}, None),
    "equal keys": ({"WORKER_API_KEY": API_KEY}, None),
    "zero upload bytes": ({"MAX_UPLOAD_BYTES": 0}, None),
    "zero storage": ({"MAX_STORAGE_BYTES": 0}, None),
    "storage below upload": ({"MAX_UPLOAD_BYTES": 4096, "MAX_STORAGE_BYTES": 4095}, None),
    "zero pending jobs": ({"MAX_PENDING_JOBS": 0}, None),
    "negative retention": ({"RETENTION_SECONDS": -1}, None),
    "zero job age": ({"MAX_JOB_AGE_SECONDS": 0}, None),
    "short lease": ({"LEASE_SECONDS": 14}, None),
    "zero attempts": ({"MAX_ATTEMPTS": 0}, None),
    "zero sync timeout": ({"SYNC_TIMEOUT_SECONDS": 0}, None),
    "zero upload slots": ({"MAX_CONCURRENT_UPLOADS": 0}, None),
    "zero idle": ({"UPLOAD_IDLE_SECONDS": 0}, None),
    "zero rate": ({"MIN_UPLOAD_BYTES_PER_SECOND": 0}, None),
    "non-numeric limit": ({"MAX_UPLOAD_BYTES": "lots"}, "MAX_UPLOAD_BYTES"),
    "fractional limit": ({"MAX_PENDING_JOBS": "1.5"}, "MAX_PENDING_JOBS"),
    "non-numeric idle": ({"UPLOAD_IDLE_SECONDS": "soon"}, "UPLOAD_IDLE_SECONDS"),
    "host with scheme": ({"AUDIO_URL_HOSTS": "https://audio.invalid"}, "AUDIO_URL_HOSTS"),
    "host with port": ({"AUDIO_URL_HOSTS": "audio.invalid:443"}, "AUDIO_URL_HOSTS"),
    "host with path": ({"AUDIO_URL_HOSTS": "audio.invalid/a"}, "AUDIO_URL_HOSTS"),
    "host with userinfo": ({"AUDIO_URL_HOSTS": "user@audio.invalid"}, "AUDIO_URL_HOSTS"),
    "host with inner space": ({"AUDIO_URL_HOSTS": "audio .invalid"}, "AUDIO_URL_HOSTS"),
    "file database": ({"DB_MODE": "file"}, "DB_MODE"),
    "sqlite database": ({"DB_MODE": "sqlite"}, "DB_MODE"),
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
    overrides, variable = INVALID_SETTINGS[case]
    logs = refused(launcher, **overrides)
    if variable:
        assert variable in logs
    assert API_KEY not in logs


def test_settings_are_trimmed_and_hosts_normalized(start):
    server = start(
        AUDIO_URL_HOSTS=" Audio.INVALID , ,cdn.invalid,",
        UPLOAD_IDLE_SECONDS=" 0.5 ",
        MAX_UPLOAD_BYTES=" 4096\n",
        DB_MODE="memory",
    )
    for url in ("https://audio.invalid/a.wav", "https://CDN.invalid/a.wav"):
        response = server.client.post("/v2/transcript", json={"audio_url": url})
        assert response.status_code == 400 and response.json() == {"error": "Audio download failed"}
    other = server.client.post("/v2/transcript", json={"audio_url": "https://other.invalid/a.wav"})
    assert other.json() == {"error": "Upload audio first, or configure a trusted AUDIO_URL_HOSTS origin"}
    assert server.client.post("/v2/upload", content=b"x" * 4096).status_code == 200
    assert server.client.post("/v2/upload", content=b"x" * 4097).status_code == 413


def test_without_audio_url_hosts_only_uploads_are_accepted(start):
    server = start(AUDIO_URL_HOSTS=None, DB_MODE="")
    response = server.client.post("/v2/transcript", json={"audio_url": "https://audio.invalid/a.wav"})
    assert response.status_code == 400
    assert response.json() == {"error": "Upload audio first, or configure a trusted AUDIO_URL_HOSTS origin"}
    job, _ = server.queue()
    assert server.client.delete(f"/v2/transcript/{job['id']}").status_code == 200


def test_startup_discards_leftover_audio(launcher, tmp_path):
    data = tmp_path / "data"
    (data / "audio").mkdir(parents=True)
    for name in (str(uuid.uuid4()), "crash-orphan"):
        (data / "audio" / name).write_bytes(b"audio")
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
        assert first.client.delete(f"/v2/transcript/{job['id']}").status_code == 200
    finally:
        first.stop()


def test_healthcheck_subcommand(launcher, start):
    server = start()
    env = {**server.env}

    def check():
        return subprocess.run([launcher.binary, "healthcheck"], env=env, capture_output=True, timeout=10)

    assert check().returncode == 0
    env["LISTEN_ADDR"] = f"127.0.0.1:{free_port()}"
    assert check().returncode == 1
    # Only the port matters: the check always targets the loopback address.
    env["LISTEN_ADDR"] = f":{server.port}"
    assert check().returncode == 0


def test_sigterm_stops_the_server_cleanly(start):
    server = start()
    server.process.terminate()
    assert server.process.wait(15) == 0
