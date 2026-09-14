"""Harness for the black-box contract suite: every test talks to a real API process over TCP.

The session builds ./cmd/parakeet-api once. Each server gets its own DATA_DIR and limits from its
environment.
"""

import contextlib
import http.client
import os
import select
import signal
import socket
import subprocess
import time
from pathlib import Path

import httpx

ROOT = Path(__file__).resolve().parents[2]
API_KEY = "client-" + "x" * 32
WORKER_KEY = "worker-" + "y" * 32
UUID_PATTERN = r"[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}"
INVALID = "Invalid or unsupported request fields"
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
# Never inherit configuration from the developer's shell.
CONFIG = {*DEFAULTS, *PROXIES, "DATA_DIR", "DB_MODE", "PUBLIC_BASE_URL", "LISTEN_ADDR"}
CONFIG |= {"RETENTION_SECONDS", "MAX_JOB_AGE_SECONDS"}


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


def timed_result(words, step=100, length=90, duration_ms=None):
    """A result whose words start every step ms and last length ms."""
    timed = [
        {"text": w, "start": i * step, "end": i * step + length, "confidence": 0.5}
        for i, w in enumerate(words)
    ]
    duration_ms = duration_ms or len(words) * step
    return {
        "text": " ".join(words),
        "words": timed,
        "audio_duration_ms": duration_ms,
        "chunks": 1,
        "seam_fallbacks": 0,
    }


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def wait_until(predicate, timeout=10.0, interval=0.02):
    deadline = time.monotonic() + timeout
    while not (value := predicate()) and time.monotonic() < deadline:
        time.sleep(interval)
    return value


def error_message(response, v1=False):
    """Return the error message; v1 True or False demands that shape, None accepts either."""
    body = response if isinstance(response, dict) else response.json()
    if isinstance(body.get("error"), dict) and v1 is not False:
        assert set(body) == {"error"} and set(body["error"]) == {"message", "type", "code"}, body
        assert body["error"]["type"] == "invalid_request_error", body
        assert isinstance(response, dict) or body["error"]["code"] == str(response.status_code), body
        return body["error"]["message"]
    assert v1 is not True and set(body) == {"error"} and isinstance(body["error"], str), body
    return body["error"]


def multipart(parts, boundary="contract-boundary-7d1f"):
    """Encode (name, value, filename) parts in the given order; filename None means a plain field."""
    body = b""
    for name, value, filename in parts:
        disposition = f'form-data; name="{name}"' + (f'; filename="{filename}"' if filename else "")
        data = value if isinstance(value, bytes) else value.encode()
        body += f"--{boundary}\r\nContent-Disposition: {disposition}\r\n\r\n".encode() + data + b"\r\n"
    return body + f"--{boundary}--\r\n".encode(), {
        "content-type": f"multipart/form-data; boundary={boundary}"
    }


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
        # The process leads its own session, so the signals also reach any children.
        for sig in (signal.SIGTERM, signal.SIGKILL):
            with contextlib.suppress(ProcessLookupError):
                os.killpg(self.process.pid, sig)
            with contextlib.suppress(subprocess.TimeoutExpired):
                self.process.wait(10)
                return

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

    def transcript(self, job_id):
        return self.client.get(f"/v2/transcript/{job_id}").json()

    def metrics(self):
        response = self.client.get("/metrics")
        assert response.status_code == 200, response.text
        prefix = 'parakeet_jobs{status="'
        lines = [line.rsplit(" ", 1) for line in response.text.splitlines()]
        assert all(name.startswith(prefix) and name.endswith('"}') for name, _ in lines), response.text
        return {name[len(prefix) : -2]: int(value) for name, value in lines}

    def audio_files(self):
        return {path.name for path in self.audio.iterdir()}

    def claim(self):
        response = self.worker.post("/internal/jobs/claim")
        assert response.status_code in (200, 204), response.text
        return response.json() if response.status_code == 200 else None

    def wait_claim(self):
        claim = wait_until(self.claim, 10, 0.05)
        assert claim, "no job became claimable"
        return claim

    def leased(self, method, claim, action, token=None, **kwargs):
        headers = {"x-lease-token": claim["token"] if token is None else token}
        return self.worker.request(
            method, f"/internal/jobs/{claim['id']}/{action}", headers=headers, **kwargs
        )

    def heartbeat(self, claim, token=None):
        return self.leased("POST", claim, "heartbeat", token)

    def fetch_audio(self, claim, token=None):
        return self.leased("GET", claim, "audio", token)

    def complete(self, claim, body, token=None):
        kwargs = {"content": body} if isinstance(body, bytes) else {"json": body}
        return self.leased("POST", claim, "complete", token, **kwargs)

    def finish(self, claim, result=None):
        response = self.complete(claim, {"result": result or sample_result()})
        assert response.status_code == 200 and response.json() == {"ok": True}, response.text


class Launcher:
    def __init__(self, binary, tmp_path_factory):
        self.binary, self.tmp = binary, tmp_path_factory

    def spawn(self, data=None, **overrides):
        """Start a process without waiting for readiness; an override of None unsets the variable."""
        data = data or self.tmp.mktemp("data")
        port = free_port()
        env = {k: v for k, v in os.environ.items() if k not in CONFIG} | DEFAULTS | {"DATA_DIR": str(data)}
        env |= {"PUBLIC_BASE_URL": f"http://127.0.0.1:{port}", "LISTEN_ADDR": f"127.0.0.1:{port}"}
        env |= {k: str(v) for k, v in overrides.items()}
        env = {k: v for k, v in env.items() if overrides.get(k, "") is not None}
        log = data.parent / f"{data.name}-{port}.log"
        with log.open("wb") as out:
            process = subprocess.Popen(
                [self.binary], cwd=ROOT, env=env, stdout=out, stderr=subprocess.STDOUT, start_new_session=True
            )
        return Server(process, port, data, log, env)

    def start(self, data=None, **overrides):
        for _ in range(3):
            server = self.spawn(data, **overrides)
            if wait_until(lambda: server.process.poll() is not None or healthy(server), 60, 0.05) and healthy(
                server
            ):
                return server
            server.stop()
            # A lost race for the free port is the only failure worth retrying.
            if "address already in use" not in server.logs().lower():
                raise RuntimeError(f"API server did not become ready:\n{server.logs()[-4000:]}")
        raise RuntimeError("API server could not bind a free port")


def healthy(server):
    try:
        return server.anonymous.get("/health/ready", timeout=1).status_code == 200
    except httpx.TransportError:
        return False


class RawRequest:
    """An HTTP/1.1 request over a plain socket, so tests control body timing byte by byte."""

    def __init__(self, server, method, path, headers, authorization=API_KEY):
        self.sock = socket.create_connection(("127.0.0.1", server.port), timeout=15)
        headers = {"Host": f"127.0.0.1:{server.port}", **headers}
        if authorization:
            headers["Authorization"] = authorization
        head = f"{method} {path} HTTP/1.1\r\n" + "".join(f"{k}: {v}\r\n" for k, v in headers.items())
        self.sock.sendall((head + "\r\n").encode())

    def send(self, data):
        try:
            self.sock.sendall(data)
            return True
        except OSError:
            return False

    def responded(self, wait=0.0):
        return bool(select.select([self.sock], [], [], wait)[0])

    def response(self):
        """Return the status and body of the response."""
        reply = http.client.HTTPResponse(self.sock)
        reply.begin()
        return reply.status, reply.read()

    def close(self):
        self.sock.close()


def raw_response(server, method, path, headers, authorization=API_KEY):
    """Send only request headers and return the status and body the server answers with."""
    request = RawRequest(server, method, path, headers, authorization)
    try:
        return request.response()
    finally:
        request.close()
