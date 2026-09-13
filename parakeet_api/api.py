"""Authenticated file transcription APIs and a private worker queue."""

import asyncio
import fcntl
import hmac
import json
import logging
import uuid
from contextlib import asynccontextmanager, suppress
from urllib.parse import urlsplit

import anyio
import httpx
from fastapi import FastAPI, HTTPException, Request
from fastapi.exceptions import RequestValidationError
from pydantic import BaseModel, ConfigDict, Field
from starlette.responses import FileResponse, JSONResponse, PlainTextResponse, Response

from .alignment import validate_options
from .formats import Result, assembly, openai, subtitles
from .settings import Settings
from .store import Store

log = logging.getLogger(__name__)


class Submission(BaseModel):
    model_config = ConfigDict(extra="forbid")
    audio_url: str = Field(max_length=8192)
    language_code: str | None = None


class Completion(BaseModel):
    model_config = ConfigDict(extra="forbid")
    result: Result | None = None
    error: str | None = Field(default=None, max_length=200)
    retry: bool = False


def create_app(settings=None):
    s = settings or Settings()

    @asynccontextmanager
    async def lifespan(app):
        s.validate()
        s.data.mkdir(parents=True, exist_ok=True)
        # Fail fast on accidentally running multiple uvicorn/API processes.
        with (s.data / "api.lock").open("w") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            app.state.store = Store(s)

            async def janitor():
                while True:
                    try:
                        await anyio.to_thread.run_sync(app.state.store.cleanup)
                    except Exception:
                        log.error("Queue cleanup failed", exc_info=False)
                    await asyncio.sleep(30)

            task = asyncio.create_task(janitor())
            try:
                yield
            finally:
                task.cancel()
                with suppress(asyncio.CancelledError):
                    await task
                app.state.store.close()

    app = FastAPI(title="Parakeet API", version="0.1.0", lifespan=lifespan, docs_url=None, redoc_url=None)

    class Guard:
        def __init__(self, app):
            self.app = app
            self.uploads = 0

        async def __call__(self, scope, receive, send):
            if scope["type"] != "http":
                return await self.app(scope, receive, send)
            path = scope["path"]
            if path in ("/health/live", "/health/ready"):
                return await self.app(scope, receive, send)
            headers = dict(scope["headers"])
            expected = s.worker_key if path.startswith("/internal/") else s.api_key
            key = headers.get(b"authorization", b"").removeprefix(b"Bearer ")
            if not expected or not hmac.compare_digest(key, expected.encode()):
                return await JSONResponse({"error": "Unauthorized"}, 401)(scope, receive, send)
            upload = path in ("/v2/upload", "/v1/audio/transcriptions") and scope["method"] == "POST"
            limit = (
                s.max_bytes + 65536 if upload else 16 * 1024 * 1024 if path.endswith("/complete") else 65536
            )
            length = headers.get(b"content-length", b"")
            if length and (not length.isdigit() or int(length) > limit):
                return await JSONResponse({"error": "Request too large"}, 413)(scope, receive, send)
            # Bound multipart spooling separately from the persistent audio quota.
            if upload and self.uploads >= 2:
                return await JSONResponse({"error": "Upload slots full"}, 429, headers={"Retry-After": "5"})(
                    scope, receive, send
                )
            if upload:
                self.uploads += 1
            released = False

            def release_upload():
                nonlocal released
                if upload and not released:
                    self.uploads -= 1
                    released = True

            scope["release_upload_slot"] = release_upload
            received = 0

            async def bounded():
                nonlocal received
                try:
                    async with asyncio.timeout(60):
                        message = await receive()
                except TimeoutError:
                    raise HTTPException(408, "Upload stalled") from None
                received += len(message.get("body", b""))
                if received > limit:
                    raise HTTPException(413, "Request too large")
                return message

            try:
                async with asyncio.timeout(s.sync_timeout + 3600):
                    await self.app(scope, bounded, send)
            finally:
                release_upload()

    app.add_middleware(Guard)

    @app.exception_handler(HTTPException)
    async def http_error(request, exc):
        error = {"message": str(exc.detail), "type": "invalid_request_error", "code": str(exc.status_code)}
        return JSONResponse(
            {"error": error if request.url.path.startswith("/v1/") else str(exc.detail)},
            exc.status_code,
            headers=exc.headers,
        )

    @app.exception_handler(RequestValidationError)
    async def validation_error(request, exc):
        # Pydantic errors can include the original URL or request data. Never echo them.
        return await http_error(request, HTTPException(422, "Invalid or unsupported request fields"))

    @app.exception_handler(ValueError)
    async def value_error(request, exc):
        return await http_error(request, HTTPException(400, "Invalid transcription options or audio"))

    def store():
        return app.state.store

    async def save(chunks):
        uid = store().reserve()
        size = 0
        try:
            async with asyncio.timeout(3600):
                async with await anyio.open_file(store().blobs / uid, "wb") as target:
                    async for chunk in chunks:
                        size += len(chunk)
                        if size > s.max_bytes:
                            raise HTTPException(413, "Audio too large")
                        await target.write(chunk)
            if size == 0:
                raise HTTPException(400, "Audio is empty")
            store().upload_ready(uid, size)
            return uid
        except BaseException:
            store().discard_upload(uid)
            raise

    async def resolve_audio(url):
        prefix = f"{s.public_url}/uploads/"
        if url.startswith(prefix):
            value = url[len(prefix) :]
            try:
                if str(uuid.UUID(value)) != value:
                    raise ValueError()
            except ValueError:
                raise HTTPException(400, "Invalid upload URL") from None
            return value, False
        parsed = urlsplit(url)
        # Explicit administrator allowlist; HTTPS only, no redirect chains.
        if (
            parsed.scheme != "https"
            or parsed.hostname not in s.url_hosts
            or parsed.port not in (None, 443)
            or parsed.username
            or parsed.password
            or parsed.fragment
        ):
            raise HTTPException(400, "Upload audio first, or configure a trusted AUDIO_URL_HOSTS origin")
        try:
            async with httpx.AsyncClient(timeout=60, follow_redirects=False, trust_env=False) as client:
                async with client.stream("GET", url) as response:
                    if response.status_code != 200:
                        raise HTTPException(400, "Audio download failed")
                    return await save(response.aiter_bytes()), True
        except httpx.HTTPError:
            raise HTTPException(400, "Audio download failed") from None

    @app.get("/health/live")
    async def live():
        return {"status": "ok"}

    @app.get("/health/ready")
    async def ready():
        store().counts()
        return {"status": "ok"}

    @app.get("/metrics")
    async def metrics():
        return PlainTextResponse(
            "".join(f'parakeet_jobs{{status="{k}"}} {v}\n' for k, v in store().counts().items())
        )

    @app.post("/v2/upload")
    async def upload(request: Request):
        uid = await save(request.stream())
        request.scope["release_upload_slot"]()
        return {"upload_url": f"{s.public_url}/uploads/{uid}"}

    @app.post("/v2/transcript")
    async def submit(body: Submission):
        options = {"language_code": body.language_code} if body.language_code else {}
        validate_options(options)
        uid, owned = await resolve_audio(body.audio_url)
        try:
            return assembly(store().submit(uid, options), s.public_url)
        except BaseException:
            if owned:
                store().discard_upload(uid)
            raise

    @app.get("/v2/transcript/{jid}")
    async def get(jid: str):
        return assembly(store().get(jid), s.public_url)

    @app.delete("/v2/transcript/{jid}")
    async def delete(jid: str):
        result = assembly(store().get(jid), s.public_url)
        store().delete(jid)
        return {**result, "status": "completed", "text": None, "words": None}

    @app.get("/v2/transcript/{jid}/{extension}")
    async def captions(jid: str, extension: str, chars_per_caption: int = 80):
        if extension not in ("srt", "vtt"):
            raise HTTPException(404, "Unknown endpoint")
        if not 20 <= chars_per_caption <= 200:
            raise HTTPException(400, "chars_per_caption must be 20..200")
        job = store().get(jid)
        if job["status"] != "completed":
            raise HTTPException(409, "Transcript is not complete")
        return PlainTextResponse(
            subtitles(json.loads(job["result"]), extension == "vtt", chars_per_caption),
            media_type="text/vtt" if extension == "vtt" else "application/x-subrip",
        )

    @app.post("/v1/audio/transcriptions")
    async def transcriptions(request: Request):
        uid = jid = None
        try:
            async with request.form(max_files=1, max_fields=10, max_part_size=65536) as form:
                allowed = {
                    "file",
                    "model",
                    "language",
                    "response_format",
                    "timestamp_granularities[]",
                    "temperature",
                }
                if set(form) - allowed:
                    raise HTTPException(
                        422, "Unsupported transcription fields; streaming and prompts are not supported"
                    )
                for key in form:
                    if key != "timestamp_granularities[]" and len(form.getlist(key)) != 1:
                        raise HTTPException(400, "Duplicate multipart field")
                if form.get("model", "parakeet") not in ("parakeet", "parakeet-tdt-0.6b-v3", "whisper-1"):
                    raise HTTPException(400, "Unknown model; use parakeet")
                if form.get("temperature", "0") not in ("0", "0.0"):
                    raise HTTPException(422, "Only greedy decoding is supported")
                fmt = form.get("response_format", "json")
                granularity = form.getlist("timestamp_granularities[]") or ["segment"]
                if fmt not in ("json", "text", "verbose_json", "srt", "vtt") or set(granularity) - {
                    "word",
                    "segment",
                }:
                    raise HTTPException(400, "Invalid response format or timestamp granularity")
                options = {"language_code": form["language"]} if form.get("language") else {}
                validate_options(options)
                audio = form.get("file")
                if not hasattr(audio, "read"):
                    raise HTTPException(400, "file must be an audio file")

                async def chunks():
                    while chunk := await audio.read(1024 * 1024):
                        yield chunk

                uid = await save(chunks())
            request.scope["release_upload_slot"]()
            jid = store().submit(uid, options)["id"]
            async with asyncio.timeout(s.sync_timeout):
                while True:
                    job = store().get(jid)
                    if job["status"] == "error":
                        raise HTTPException(422, job["error"])
                    if job["status"] == "completed":
                        break
                    if await request.is_disconnected():
                        raise HTTPException(499, "Client disconnected")
                    await asyncio.sleep(0.2)
            result = json.loads(job["result"])
            if fmt == "json":
                return {"text": result["text"]}
            if fmt == "verbose_json":
                return openai(result, granularity)
            return PlainTextResponse(
                result["text"] if fmt == "text" else subtitles(result, fmt == "vtt"),
                media_type="text/vtt" if fmt == "vtt" else "text/plain",
            )
        except TimeoutError:
            raise HTTPException(504, "Transcription wait timed out; use the asynchronous /v2 API") from None
        finally:
            if jid:
                with suppress(HTTPException):
                    store().delete(jid)
            elif uid:
                store().discard_upload(uid)

    def lease(request):
        token = request.headers.get("x-lease-token", "")
        if not token:
            raise HTTPException(409, "Missing lease token")
        return token

    @app.post("/internal/jobs/claim")
    async def claim():
        job = store().claim()
        return job if job else Response(status_code=204)

    @app.post("/internal/jobs/{jid}/heartbeat")
    async def heartbeat(jid: str, request: Request):
        store().heartbeat(jid, lease(request))
        return {"ok": True}

    @app.get("/internal/jobs/{jid}/audio")
    async def audio(jid: str, request: Request):
        return FileResponse(store().audio(jid, lease(request)), media_type="application/octet-stream")

    @app.post("/internal/jobs/{jid}/complete")
    async def complete(jid: str, body: Completion, request: Request):
        if (body.result is None) == (body.error is None):
            raise HTTPException(400, "Provide exactly one of result or error")
        store().finish(
            jid, lease(request), body.result.model_dump() if body.result else None, body.error, body.retry
        )
        return {"ok": True}

    return app


app = create_app()
