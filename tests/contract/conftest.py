"""Fixtures for the black-box contract suite; support.py holds the server harness.

Run with `uv run pytest tests/contract -q`; it needs a Go toolchain. Scenarios that need lease
expiry, retention or job age are left to Go unit tests with an injectable clock.
"""

import subprocess

import pytest

from .support import PROXIES, ROOT, SERVER, Launcher, sample_result


@pytest.fixture(scope="session")
def server_command(tmp_path_factory):
    """Returns a function mapping a port to the argv that serves the API on it."""
    if SERVER != "go":
        raise pytest.UsageError("API_SERVER must be go; the Python API was removed")
    binary = tmp_path_factory.mktemp("go-build") / "parakeet-api"
    subprocess.run(["go", "build", "-o", str(binary), "./cmd/parakeet-api"], cwd=ROOT, check=True)
    return lambda port: [str(binary)]


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
