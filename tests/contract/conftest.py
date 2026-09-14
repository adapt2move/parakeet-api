"""Fixtures for the black-box contract suite; support.py holds the server harness.

Run against the Python reference with `uv run pytest tests/contract -q`, or against the Go
rewrite with `API_SERVER=go uv run pytest tests/contract -q`. Scenarios that need lease
expiry, retention or job age are left to unit tests with an injectable clock.
"""

import subprocess

import pytest

from .support import PROXIES, ROOT, SERVER, Launcher, sample_result


@pytest.fixture(scope="session")
def server_command(tmp_path_factory):
    """Returns a function mapping a port to the argv that serves the API on it."""
    if SERVER == "python":
        return lambda port: [
            "uv",
            "run",
            "uvicorn",
            "parakeet_api.api:app",
            "--host",
            "127.0.0.1",
            "--port",
            str(port),
        ]
    if SERVER == "go":
        binary = tmp_path_factory.mktemp("go-build") / "parakeet-api"
        subprocess.run(["go", "build", "-o", str(binary), "./cmd/parakeet-api"], cwd=ROOT, check=True)
        return lambda port: [str(binary)]
    raise pytest.UsageError("API_SERVER must be python or go")


@pytest.fixture(scope="session")
def launcher(server_command, tmp_path_factory):
    return Launcher(server_command, tmp_path_factory)


@pytest.fixture(scope="module")
def server(launcher):
    """A server with default contract limits, shared by the tests of one module."""
    instance = launcher.start()
    yield instance
    instance.stop()


@pytest.fixture
def api(server):
    """The module server; fails the test that leaves a claimable job behind."""
    yield server
    assert server.claim() is None, "test left a queued job behind"


@pytest.fixture
def start(launcher):
    """Start dedicated servers for special limits; they stop after the test."""
    started = []

    def run(**overrides):
        instance = launcher.start(**overrides)
        started.append(instance)
        return instance

    yield run
    for instance in started:
        instance.stop()


@pytest.fixture
def result():
    return sample_result()


@pytest.fixture
def no_proxy(monkeypatch):
    for name in PROXIES:
        monkeypatch.delenv(name, raising=False)
