"""One CPU model per worker; lease renewal is independent of inference."""

import asyncio
import json
import os
import shutil
import tempfile
import threading
import time
from contextlib import asynccontextmanager, suppress
from pathlib import Path

import httpx
from fastapi import FastAPI
from starlette.responses import JSONResponse

from .engine import Engine

API_URL = os.getenv("API_URL", "http://api:8080").rstrip("/")
KEY = os.getenv("WORKER_API_KEY", "")
MAX_BYTES = int(os.getenv("MAX_UPLOAD_BYTES", str(128 * 1024 * 1024)))
STALL_SECONDS = int(os.getenv("WORKER_STALL_SECONDS", "300"))
state = {"ready": False, "busy": False, "progress": time.monotonic(), "completed": 0, "failed": 0}


def event(name, **fields):
    # Never log URLs, keys, filenames, audio or recognized text.
    print(json.dumps({"event": name, **fields}), flush=True)


def progress():
    state["progress"] = time.monotonic()


async def execute(client, engine, job, shutdown):
    jid, token = job["id"], job["token"]
    headers = {"x-lease-token": token}
    cancel = threading.Event()
    started = time.monotonic()
    state["busy"] = True
    progress()

    async def renew():
        while True:
            await asyncio.sleep(job["lease_seconds"] / 3)
            if shutdown.is_set() or time.monotonic() - state["progress"] > STALL_SECONDS:
                cancel.set()
                return
            try:
                response = await client.post(f"/internal/jobs/{jid}/heartbeat", headers=headers)
                response.raise_for_status()
            except httpx.HTTPError:
                # Do not continue producing results after uncertain ownership.
                cancel.set()
                return

    heartbeat = asyncio.create_task(renew())
    try:
        with tempfile.TemporaryDirectory(prefix="parakeet-job-") as directory:
            size = 0
            async with client.stream("GET", f"/internal/jobs/{jid}/audio", headers=headers) as response:
                response.raise_for_status()
                with (Path(directory) / "input").open("wb") as target:
                    async for chunk in response.aiter_bytes():
                        if cancel.is_set():
                            raise InterruptedError()
                        size += len(chunk)
                        if size > MAX_BYTES:
                            raise ValueError("Audio exceeds worker upload limit")
                        target.write(chunk)
                        progress()
            # Shield native inference so shutdown never deletes its input mid-decode.
            inference = asyncio.create_task(asyncio.to_thread(engine.transcribe, directory, cancel, progress))
            try:
                result = await asyncio.shield(inference)
            except asyncio.CancelledError:
                cancel.set()
                with suppress(Exception):
                    await inference
                raise
            if cancel.is_set():
                raise InterruptedError()
            response = await client.post(
                f"/internal/jobs/{jid}/complete", headers=headers, json={"result": result}
            )
            response.raise_for_status()
            state["completed"] += 1
            event(
                "transcribed",
                audio_ms=result["audio_duration_ms"],
                words=len(result["words"]),
                chunks=result["chunks"],
                seam_fallbacks=result["seam_fallbacks"],
                processing_ms=round((time.monotonic() - started) * 1000),
            )
    except (InterruptedError, asyncio.CancelledError):
        cancel.set()
        if shutdown.is_set():
            raise asyncio.CancelledError()
    except Exception as exc:
        state["failed"] += 1
        event("job_failed", error_type=type(exc).__name__)
        if not cancel.is_set():
            with suppress(httpx.HTTPError):
                # Only our own validation errors have messages safe for clients.
                error = str(exc) if type(exc) is ValueError else "Worker processing failed"
                await client.post(
                    f"/internal/jobs/{jid}/complete",
                    headers=headers,
                    json={"error": error[:200], "retry": not isinstance(exc, ValueError)},
                )
    finally:
        cancel.set()
        heartbeat.cancel()
        with suppress(asyncio.CancelledError):
            await heartbeat
        state["busy"] = False
        progress()


@asynccontextmanager
async def lifespan(app):
    if len(KEY) < 32:
        raise ValueError("Set WORKER_API_KEY to at least 32 characters")
    # Each container owns its scratch volume. Remove files left by a container crash.
    for path in Path(tempfile.gettempdir()).glob("parakeet-job-*"):
        if path.is_dir():
            shutil.rmtree(path)
    engine = await asyncio.to_thread(Engine)
    state["ready"] = True
    event("model_ready")
    shutdown = threading.Event()

    async def run():
        async with httpx.AsyncClient(
            base_url=API_URL, headers={"Authorization": KEY}, timeout=30, trust_env=False
        ) as client:
            while not shutdown.is_set():
                try:
                    response = await client.post("/internal/jobs/claim")
                    response.raise_for_status()
                    if response.status_code == 204:
                        await asyncio.sleep(1)
                    else:
                        await execute(client, engine, response.json(), shutdown)
                except httpx.HTTPError:
                    event("api_unavailable")
                    await asyncio.sleep(5)

    task = asyncio.create_task(run())
    app.state.loop = task
    yield
    state["ready"] = False
    shutdown.set()
    task.cancel()
    with suppress(asyncio.CancelledError):
        await task


app = FastAPI(lifespan=lifespan, docs_url=None, redoc_url=None, openapi_url=None)


@app.get("/health")
async def health():
    ready = state["ready"] and not app.state.loop.done()
    healthy = ready and (not state["busy"] or time.monotonic() - state["progress"] < STALL_SECONDS)
    return JSONResponse(
        {"ready": healthy, "busy": state["busy"], "completed": state["completed"], "failed": state["failed"]},
        200 if healthy else 503,
    )
