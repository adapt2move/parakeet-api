"""Harness for the black-box contract suite: every test talks to a real API process over TCP.

The session builds ./cmd/parakeet-api once. Each server gets its own DATA_DIR and limits from its
environment.
"""

import contextlib
import os
import select
import signal
import socket
import subprocess
import time
from pathlib import Path

import httpx

ROOT = Path(__file__).resolve().parents[2]
SERVER = os.getenv("API_SERVER", "go")
API_KEY = "client-" + "x" * 32
WORKER_KEY = "worker-" + "y" * 32
UUID_PATTERN = r"[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}"

# Never inherit configuration from the developer's shell.
CONFIG = (
    "DATA_DIR",
    "DB_MODE",
    "API_KEY",
    "WORKER_API_KEY",
    "PUBLIC_BASE_URL",
    "MAX_UPLOAD_BYTES",
    "MAX_STORAGE_BYTES",
    "MAX_PENDING_JOBS",
    "RETENTION_SECONDS",
    "MAX_JOB_AGE_SECONDS",
    "LEASE_SECONDS",
    "MAX_ATTEMPTS",
    "SYNC_TIMEOUT_SECONDS",
    "MAX_CONCURRENT_UPLOADS",
    "UPLOAD_IDLE_SECONDS",
    "MIN_UPLOAD_BYTES_PER_SECOND",
    "AUDIO_URL_HOSTS",
    "LISTEN_ADDR",
)
PROXIES = ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy")

DEFAULTS = {
    "API_KEY": API_KEY,
    "WORKER_API_KEY": WORKER_KEY,
    "MAX_UPLOAD_BYTES": "4096",
    "MAX_STORAGE_BYTES": str(1024 * 1024),
    "MAX_PENDING_JOBS": "32",
    "LEASE_SECONDS": "15",
    "MAX_ATTEMPTS": "3",
    "SYNC_TIMEOUT_SECONDS": "30",
    "MAX_CONCURRENT_UPLOADS": "4",
    "UPLOAD_IDLE_SECONDS": "5",
    "MIN_UPLOAD_BYTES_PER_SECOND": "1024",
    # .invalid never resolves (RFC 6761), so allowed URLs fail without leaving the machine.
    "AUDIO_URL_HOSTS": "audio.invalid",
}


def sample_result():
    return {
        "text": "Hello world.",
        "audio_duration_ms": 2100,
        "chunks": 1,
        "seam_fallbacks": 0,
        "words": [
            {"text": "Hello", "start": 120, "end": 610, "confidence": 0.8},
            {"text": "world.", "start": 900, "end": 1700, "confidence": 0.7},
        ],
    }


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def wait_until(predicate, timeout=10.0, interval=0.02):
    deadline = time.monotonic() + timeout
    while True:
        value = predicate()
        if value or time.monotonic() > deadline:
            return value
        time.sleep(interval)


class Server:
    def __init__(self, process, port, data, log, env):
        self.process, self.port, self.data, self.log, self.env = process, port, data, log, env
        self.base = f"http://127.0.0.1:{port}"
        self.audio = data / "audio"
        self.client = self.http({"authorization": API_KEY})
        self.worker = self.http({"authorization": WORKER_KEY})
        self.anonymous = self.http({})

    def http(self, headers):
        return httpx.Client(base_url=self.base, headers=headers, timeout=60, trust_env=False)

    def logs(self):
        return self.log.read_text(errors="replace")

    def stop(self):
        for client in (self.client, self.worker, self.anonymous):
            client.close()
        stop_process(self.process)

    # Client side

    def upload(self, data=b"audio"):
        response = self.client.post("/v2/upload", content=data)
        assert response.status_code == 200, response.text
        url = response.json()["upload_url"]
        return url, url.rsplit("/", 1)[1]

    def submit(self, url, **fields):
        response = self.client.post("/v2/transcript", json={"audio_url": url, **fields})
        assert response.status_code == 200, response.text
        return response.json()

    def queue(self, data=b"audio", **fields):
        url, uid = self.upload(data)
        return self.submit(url, **fields), uid

    def metrics(self):
        response = self.client.get("/metrics")
        assert response.status_code == 200, response.text
        counts = {}
        for line in response.text.splitlines():
            name, value = line.rsplit(" ", 1)
            assert name.startswith('parakeet_jobs{status="') and name.endswith('"}'), line
            counts[name[len('parakeet_jobs{status="') : -2]] = int(value)
        return counts

    def audio_files(self):
        return {path.name for path in self.audio.iterdir()}

    # Worker side

    def claim(self):
        response = self.worker.post("/internal/jobs/claim")
        if response.status_code == 204:
            return None
        assert response.status_code == 200, response.text
        return response.json()

    def wait_claim(self, timeout=10.0):
        claim = wait_until(self.claim, timeout, 0.05)
        assert claim, "no job became claimable"
        return claim

    def lease(self, claim, token=None):
        return {"x-lease-token": claim["token"] if token is None else token}

    def heartbeat(self, claim, token=None):
        return self.worker.post(f"/internal/jobs/{claim['id']}/heartbeat", headers=self.lease(claim, token))

    def fetch_audio(self, claim, token=None):
        return self.worker.get(f"/internal/jobs/{claim['id']}/audio", headers=self.lease(claim, token))

    def complete(self, claim, body, token=None):
        return self.worker.post(
            f"/internal/jobs/{claim['id']}/complete", headers=self.lease(claim, token), json=body
        )

    def finish(self, claim, result=None):
        response = self.complete(claim, {"result": result or sample_result()})
        assert response.status_code == 200, response.text
        assert response.json() == {"ok": True}


def stop_process(process):
    if process.poll() is not None:
        return
    # The process leads its own session, so this also reaches any children.
    try:
        os.killpg(process.pid, signal.SIGTERM)
        process.wait(10)
    except ProcessLookupError:
        process.wait(10)
    except subprocess.TimeoutExpired:
        with contextlib.suppress(ProcessLookupError):
            os.killpg(process.pid, signal.SIGKILL)
        process.wait(10)


class Launcher:
    def __init__(self, command, tmp_path_factory):
        self.command, self.tmp = command, tmp_path_factory

    def environment(self, port, data, overrides):
        env = {k: v for k, v in os.environ.items() if k not in CONFIG and k not in PROXIES}
        env.update(DEFAULTS)
        env.update(
            DATA_DIR=str(data),
            PUBLIC_BASE_URL=f"http://127.0.0.1:{port}",
            LISTEN_ADDR=f"127.0.0.1:{port}",
            PYTHONUNBUFFERED="1",
        )
        for key, value in overrides.items():
            if value is None:
                env.pop(key, None)
            else:
                env[key] = str(value)
        return env

    def spawn(self, data=None, **overrides):
        """Start a process without waiting for readiness."""
        data = data or self.tmp.mktemp("data")
        port = free_port()
        env = self.environment(port, data, overrides)
        log = data.parent / f"{data.name}-{port}.log"
        with log.open("wb") as output:
            process = subprocess.Popen(
                self.command(port),
                cwd=ROOT,
                env=env,
                stdout=output,
                stderr=subprocess.STDOUT,
                start_new_session=True,
            )
        return Server(process, port, data, log, env)

    def start(self, data=None, **overrides):
        for _ in range(3):
            server = self.spawn(data, **overrides)
            if self.ready(server):
                return server
            stop_process(server.process)
            logs = server.logs()
            # A lost race for the free port is the only failure worth retrying.
            if "address already in use" not in logs.lower():
                raise RuntimeError(f"API server did not become ready:\n{logs[-4000:]}")
        raise RuntimeError("API server could not bind a free port")

    @staticmethod
    def ready(server, timeout=60.0):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if server.process.poll() is not None:
                return False
            try:
                if server.anonymous.get("/health/ready", timeout=1).status_code == 200:
                    return True
            except httpx.TransportError:
                pass
            time.sleep(0.05)
        return False


def error_message(response, v1=False):
    body = response.json()
    if v1:
        assert body == {
            "error": {
                "message": body["error"]["message"],
                "type": "invalid_request_error",
                "code": str(response.status_code),
            }
        }, body
        return body["error"]["message"]
    assert set(body) == {"error"} and isinstance(body["error"], str), body
    return body["error"]


def multipart(parts, boundary="contract-boundary-7d1f"):
    """Encode (name, value, filename) parts in the given order; filename None means a plain field."""
    body = b""
    for name, value, filename in parts:
        disposition = f'form-data; name="{name}"'
        headers = ""
        if filename is not None:
            disposition += f'; filename="{filename}"'
            headers = "Content-Type: application/octet-stream\r\n"
        data = value if isinstance(value, bytes) else value.encode()
        body += f"--{boundary}\r\nContent-Disposition: {disposition}\r\n{headers}\r\n".encode()
        body += data + b"\r\n"
    body += f"--{boundary}--\r\n".encode()
    return body, {"content-type": f"multipart/form-data; boundary={boundary}"}


class RawRequest:
    """An HTTP/1.1 request over a plain socket, so tests control body timing byte by byte."""

    def __init__(self, server, method, path, headers, authorization=API_KEY, timeout=15.0):
        self.sock = socket.create_connection(("127.0.0.1", server.port), timeout=timeout)
        lines = [f"{method} {path} HTTP/1.1", f"Host: 127.0.0.1:{server.port}"]
        if authorization:
            lines.append(f"Authorization: {authorization}")
        lines += [f"{name}: {value}" for name, value in headers.items()]
        self.sock.sendall(("\r\n".join(lines) + "\r\n\r\n").encode())
        self.buffer = b""

    def send(self, data):
        try:
            self.sock.sendall(data)
            return True
        except OSError:
            return False

    def responded(self, wait=0.0):
        return bool(self.buffer) or bool(select.select([self.sock], [], [], wait)[0])

    def response(self):
        """Return (status, headers, body) of the response, reading until it is complete."""
        while b"\r\n\r\n" not in self.buffer:
            if not self.recv():
                raise ConnectionError(f"connection closed before response headers: {self.buffer!r}")
        head, body = self.buffer.split(b"\r\n\r\n", 1)
        status_line, *header_lines = head.decode("latin-1").split("\r\n")
        headers = {}
        for line in header_lines:
            name, value = line.split(":", 1)
            headers[name.strip().lower()] = value.strip()
        length = int(headers.get("content-length", "-1"))
        while length < 0 or len(body) < length:
            chunk = self.recv()
            if not chunk:
                break
            body += chunk
        return int(status_line.split(" ")[1]), headers, body

    def recv(self):
        try:
            chunk = self.sock.recv(65536)
        except ConnectionResetError:
            chunk = b""
        self.buffer += chunk
        return chunk

    def close(self):
        self.sock.close()
