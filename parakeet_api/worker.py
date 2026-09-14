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
STALL_SECONDS = int(os.getenv("WORKER_STALL_SECONDS", "300"))
BACKOFF_SECONDS = 1
state = {"ready": False, "busy": False, "progress": time.monotonic(), "completed": 0, "failed": 0}


def event(name, **fields):
    # Never log URLs, keys, filenames, audio or recognized text.
    print(json.dumps({"event": name, **fields}), flush=True)


def progress():
    state["progress"] = time.monotonic()


class LeaseLost(Exception):
    """The API fenced this worker out: the job expired, was deleted or belongs to another worker."""


def checked(response):
    if response.status_code == 409:
        raise LeaseLost()
    response.raise_for_status()
    return response


async def post(client, path, headers, payload=None, attempts=4):
    # Transient API failures must not discard finished inference. 4xx answers are final.
    for attempt in range(attempts):
        last = attempt == attempts - 1
        try:
            response = await client.post(path, headers=headers, json=payload)
        except httpx.TransportError:
            if last:
                raise
        else:
            if response.status_code < 500 or last:
                return checked(response)
        await asyncio.sleep(BACKOFF_SECONDS * 2**attempt)


def permanent(exc):
    """Failures that would repeat on every attempt, with a message that is safe for clients."""
    if isinstance(exc, httpx.HTTPStatusError) and exc.response.status_code < 500:
        return "Transcription rejected by API"
    if type(exc) is ValueError:
        # Only our own validation errors have messages safe for clients.
        return str(exc)
    return None


async def execute(client, engine, job, shutdown):
    jid, token = job["id"], job["token"]
    headers = {"x-lease-token": token}
    cancel = threading.Event()
    started = time.monotonic()
    state["busy"] = True
    progress()

    async def renew():
        renewed = started
        while True:
            await asyncio.sleep(job["lease_seconds"] / 3)
            if shutdown.is_set() or time.monotonic() - state["progress"] > STALL_SECONDS:
                cancel.set()
                return
            sent = time.monotonic()
            try:
                checked(await client.post(f"/internal/jobs/{jid}/heartbeat", headers=headers))
                renewed = sent
            except LeaseLost:
                cancel.set()
                return
            except httpx.HTTPError:
                # One failed renewal leaves most of the lease. Stop once it has certainly expired.
                if time.monotonic() - renewed >= job["lease_seconds"]:
                    cancel.set()
                    return

    heartbeat = asyncio.create_task(renew())
    try:
        with tempfile.TemporaryDirectory(prefix="parakeet-job-") as directory:
            size = 0
            async with client.stream("GET", f"/internal/jobs/{jid}/audio", headers=headers) as response:
                checked(response)
                with (Path(directory) / "input").open("wb") as target:
                    async for chunk in response.aiter_bytes():
                        if cancel.is_set():
                            raise InterruptedError()
                        size += len(chunk)
                        if size > job["bytes"]:
                            raise ValueError("Audio is larger than its upload")
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
            await post(client, f"/internal/jobs/{jid}/complete", headers, {"result": result})
            state["completed"] += 1
            event(
                "transcribed",
                audio_ms=result["audio_duration_ms"],
                words=len(result["words"]),
                chunks=result["chunks"],
                seam_fallbacks=result["seam_fallbacks"],
                processing_ms=round((time.monotonic() - started) * 1000),
            )
    except LeaseLost:
        cancel.set()
        event("lease_lost")
    except (InterruptedError, asyncio.CancelledError):
        cancel.set()
        if shutdown.is_set():
            raise asyncio.CancelledError()
    except Exception as exc:
        state["failed"] += 1
        event("job_failed", error_type=type(exc).__name__)
        if not cancel.is_set():
            error = permanent(exc)
            payload = {"error": (error or "Worker processing failed")[:200], "retry": error is None}
            with suppress(httpx.HTTPError, LeaseLost):
                await post(client, f"/internal/jobs/{jid}/complete", headers, payload)
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
