"""One API process owns SQLite; workers only use HTTP."""

import os
from dataclasses import dataclass
from pathlib import Path


@dataclass(frozen=True)
class Settings:
    data: Path = Path(os.getenv("DATA_DIR", "/data"))
    db_mode: str = os.getenv("DB_MODE", "memory")
    api_key: str = os.getenv("API_KEY", "")
    worker_key: str = os.getenv("WORKER_API_KEY", "")
    public_url: str = os.getenv("PUBLIC_BASE_URL", "http://localhost:8080").rstrip("/")
    max_bytes: int = int(os.getenv("MAX_UPLOAD_BYTES", str(128 * 1024 * 1024)))
    storage_bytes: int = int(os.getenv("MAX_STORAGE_BYTES", str(2 * 1024**3)))
    queue_size: int = int(os.getenv("MAX_PENDING_JOBS", "32"))
    retention: int = int(os.getenv("RETENTION_SECONDS", "3600"))
    max_job_age: int = int(os.getenv("MAX_JOB_AGE_SECONDS", "21600"))
    lease: int = int(os.getenv("LEASE_SECONDS", "90"))
    attempts: int = int(os.getenv("MAX_ATTEMPTS", "3"))
    sync_timeout: int = int(os.getenv("SYNC_TIMEOUT_SECONDS", "1800"))
    url_hosts: tuple[str, ...] = tuple(filter(None, os.getenv("AUDIO_URL_HOSTS", "").split(",")))

    def validate(self):
        if self.db_mode not in ("memory", "file"):
            raise ValueError("DB_MODE must be memory or file")
        if min(len(self.api_key), len(self.worker_key)) < 32 or self.api_key == self.worker_key:
            raise ValueError("Set distinct API_KEY and WORKER_API_KEY values of at least 32 characters")
        if (
            min(
                self.max_bytes,
                self.storage_bytes,
                self.queue_size,
                self.retention,
                self.max_job_age,
                self.lease,
                self.attempts,
                self.sync_timeout,
            )
            <= 0
        ):
            raise ValueError("Limits must be positive")
        if self.storage_bytes < self.max_bytes or self.lease < 15:
            raise ValueError("Storage must fit an upload; lease must be at least 15 seconds")
