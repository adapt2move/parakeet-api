import pytest
from fastapi.testclient import TestClient

from parakeet_api.api import create_app
from parakeet_api.settings import Settings


@pytest.fixture
def settings(tmp_path):
    return Settings(
        data=tmp_path,
        api_key="client-" + "x" * 32,
        worker_key="worker-" + "y" * 32,
        max_bytes=1024,
        storage_bytes=4096,
        retention=60,
        lease=15,
    )


@pytest.fixture
def client(settings):
    with TestClient(create_app(settings), headers={"authorization": settings.api_key}) as client:
        yield client


@pytest.fixture
def result():
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
