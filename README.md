# Parakeet API

CPU transcription using NVIDIA Parakeet TDT 0.6B v3 INT8, with OpenAI and AssemblyAI file transcription endpoints. One small API owns a SQLite job queue. Each worker runs one model and claims jobs over HTTP. No PostgreSQL, Redis, GPU or external inference service is required.

This is an initial implementation. The compatibility table below defines the supported subset. It is not a replacement for every feature of either hosted service.

## Run locally

Install Docker and Compose, then generate two independent keys:

```sh
cp .env.example .env
# Put the output of each command into the corresponding .env variable:
openssl rand -hex 32
openssl rand -hex 32
docker compose up --build -d
```

The API listens on `127.0.0.1:8080`. Compose limits the API to 0.25 CPU / 384 MiB and the worker to 1.75 CPU / 4 GiB. Uploaded audio and SQLite live in a Docker volume. Worker scratch uses a separate disk volume. `docker compose down -v` deletes these volumes and any unfinished work.

For published images, use `docker compose up -d --no-build`. Images are `ghcr.io/adapt2move/parakeet-api` and `ghcr.io/adapt2move/parakeet-worker`, for Linux amd64 and arm64. CI publishes `main`, full `sha-<commit>` tags, and version tags when a `v*` Git tag is pushed. Pin a digest for deployment. Each image pair is tested with real inference before publication.

## APIs

Use the client key in `Authorization: <API_KEY>` or `Authorization: Bearer <API_KEY>`. Both styles work. Configure the SDK base URL to point at this service. The two keys must differ and be at least 32 characters long.

| Endpoint | Supported behavior |
| --- | --- |
| `POST /v2/upload` | Raw audio bytes, returns an opaque `upload_url` |
| `POST /v2/transcript` | Submit `audio_url` and optional `language_code`; returns a queued job |
| `GET /v2/transcript/{id}` | Poll `queued`, `processing`, `completed` or `error`; words use milliseconds |
| `DELETE /v2/transcript/{id}` | Cancel/delete the job and its uploaded audio |
| `GET /v2/transcript/{id}/srt` or `/vtt` | Captions derived from word alignment; optional `chars_per_caption=20..200` |
| `POST /v1/audio/transcriptions` | Multipart `file`, returns a final response after queueing and inference |
| `GET /health/live`, `/health/ready` | API health, without authentication |
| `GET /metrics` | Current job counts, client authentication required |

The OpenAI endpoint supports `json`, `text`, `verbose_json`, `srt` and `vtt` response formats. Use `timestamp_granularities[]=word` and/or `segment` with `verbose_json`; timestamps are in seconds. The model name is `parakeet` or `parakeet-tdt-0.6b-v3`. `whisper-1` is accepted as a compatibility alias and still runs Parakeet. Decoding is greedy; only temperature zero is accepted.

```python
import os
from openai import OpenAI

client = OpenAI(api_key=os.environ["API_KEY"], base_url="http://localhost:8080/v1", timeout=1800)
with open("recording.m4a", "rb") as audio:
    result = client.audio.transcriptions.create(
        model="parakeet",
        file=audio,
        response_format="verbose_json",
        timestamp_granularities=["word", "segment"],
    )
print(result.text)
```

```python
import os
import assemblyai as aai

aai.settings.api_key = os.environ["API_KEY"]
aai.settings.base_url = "http://localhost:8080"
transcript = aai.Transcriber().transcribe("recording.m4a")
print(transcript.text)
print(transcript.export_subtitles_vtt())
aai.Transcript.delete_by_id(transcript.id)
```

The SDK tests pin their dependencies in `uv.lock`. AssemblyAI 0.65.0 needs `pydantic-settings` when used with Pydantic 2; install it with the SDK in that environment.

Speaker diarization, forced language selection, language identification, translation, prompts, word boosting, webhooks, summarization, realtime WebSockets and incremental SSE are not implemented. Unknown transcription options are rejected. `language_code` labels a request; the multilingual model still selects its own language. No language confidence is invented. AssemblyAI's separate synchronous `/transcribe` API is not implemented; its regular SDK offers a blocking call by polling `/v2/transcript`.

## Timestamps and long recordings

FFmpeg decodes the first audio track to mono 16 kHz PCM on disk. The model processes windows of 120 seconds with 15 seconds of overlap. AssemblyAI returns overall audio duration rounded up to whole seconds for SDK compatibility; OpenAI retains fractional seconds. Word starts and ends come from the TDT decoder's token timestamps and durations. Subwords and punctuation are joined; overlapping chunks are merged using matching words and their times. A temporal fallback handles disagreements and is counted in worker logs as `seam_fallbacks`.

These are acoustic decoder alignments, not independently verified forced alignments. Recognition errors and words at chunk boundaries can affect accuracy. Confidence is the mean of decoder token probabilities, not a calibrated probability of transcription correctness. Subtitle segments group those words without inventing individual word times. Segment token IDs and Whisper-specific log-probability fields are omitted.

Supported containers include WAV, M4A/MP4, MP3, FLAC, Ogg, WebM and other explicitly allowed FFmpeg formats. Audio must finish uploading before inference starts. HTTP chunked transfer is accepted by `/v2/upload`; that does not provide live transcription. Raw PCM without a container is not supported.

## Queue, storage and limits

Only **one API process / replica** may own the SQLite directory. A process lock catches accidental duplicate API processes. Use local block storage, not a shared SQLite file over NFS. Worker replicas never open SQLite or mount API storage.

Workers receive a 90-second lease and renew it during processing. An expired lease returns the job to the queue, up to three attempts. A lease token prevents an old worker from committing after reassignment or deletion. Execution is at least once. Re-submitting the same upload with the same options returns its existing job, which makes that submission idempotent. A new upload creates a new job.

| Setting | Default | Applies to |
| --- | --- | --- |
| `MAX_UPLOAD_BYTES` | 134217728, 128 MiB | API and worker; keep equal |
| `MAX_STORAGE_BYTES` | 2147483648, 2 GiB | API uploaded-audio quota |
| `MAX_PENDING_JOBS` | 32 queued + processing | API |
| `RETENTION_SECONDS` | 3600 | Completed/failed jobs and audio |
| `MAX_JOB_AGE_SECONDS` | 21600, 6 hours | Total queued + processing age |
| `LEASE_SECONDS` / `MAX_ATTEMPTS` | 90 / 3 | API |
| `SYNC_TIMEOUT_SECONDS` | 1800 | OpenAI request wait |
| `MAX_AUDIO_SECONDS` | 10800, 3 hours | Worker, hard ceiling 3 hours |
| `JOB_TIMEOUT_SECONDS` | 1800 | Worker, per inference attempt |
| `PARAKEET_THREADS` | 3 | CPU inference threads |
| `WORKER_STALL_SECONDS` | 300 | Worker health / lease renewal watchdog |
| `PUBLIC_BASE_URL` | `http://localhost:8080` | Exact public origin used for opaque upload URLs |
| `AUDIO_URL_HOSTS` | Empty | Comma-separated trusted HTTPS download origins |

Reservations bound stored uploads. Two simultaneous body uploads are allowed; completed uploads release their slots while requests wait in the queue. A full queue or upload quota returns `429` with `Retry-After`. Byte and duration limits are explicit; oversized audio is rejected rather than silently truncated. The API volume also needs room for SQLite and its WAL, beyond the audio quota.

A cleanup task runs every 30 seconds. Unsubmitted uploads expire after at least one hour. OpenAI jobs and audio are deleted after the final result is constructed, on timeout or on disconnect. AssemblyAI jobs remain pollable for the retention period and can be deleted earlier. Deletion stops lease renewal; a worker may need to finish its current native inference window before removing its temporary copy.

SQLite stores job state, leases, options and results. It is not a media archive. Use a PVC to recover jobs after Pod replacement. Use `emptyDir` instead if losing unfinished jobs and results on Pod replacement is acceptable. Worker scratch can always be `emptyDir`. Deleting rows and files is not a secure erasure guarantee for storage snapshots or backups; keep this service out of long-lived media backups.

## Deployment

`deploy/colocated.yaml` puts API and worker in one Pod. `deploy/distributed.yaml` separates them so workers can scale independently. Both use namespace `parakeet`, one API replica, a 4 GiB API PVC and disk scratch. Apply **one** variant after creating the Secret shown in `deploy/README.md`.

Start the server with an API limit of 0.5 CPU / 512 MiB and one worker at 3.5 CPU / 6 GiB, totaling 4 CPU / 6.5 GiB. A second worker adds another model and its RAM requirement; parallel workers do not fit the same total budget automatically. On the Mac, use the smaller Compose settings for initial testing. Server latency still needs measurement on the actual CPU.

Keep `/internal/*` reachable only by workers. Use a ClusterIP service, TLS at your ingress, and reject `/internal/*` at that ingress if you expose the public API. Worker authentication uses its separate key. The manifests expose no external ingress. `AUDIO_URL_HOSTS` is empty by default, so clients upload files directly. Only explicitly trusted HTTPS hosts may be downloaded; redirects and ambient HTTP proxies are disabled. Treat that allowlist as trusted configuration, especially with private DNS.

Run readiness/liveness checks for the API and the worker independently. API readiness means SQLite is available; it does not imply a worker is connected. Alert on queued jobs without progress, repeated `job_failed` events, worker health failures and volume capacity. Logs contain counts and durations, never recognized text or input URLs. All callers sharing the static key share access to jobs. This service provides no tenant isolation.

## Development and CI

```sh
uv sync --frozen
uv run ruff check .
uv run ruff format --check .
uv run pytest -q
```

Unit and SDK tests cover fenced leases, concurrent claims, retry exhaustion, queue/storage limits, deletion, retention, request authentication and timing conversion. CI builds native amd64 and arm64 images and transcribes synthetic speech through both SDKs before pushing either image. No customer recordings belong in this repository or CI artifacts.

To test a local recording without printing or saving its transcript:

```sh
docker build --target api -t parakeet-api:test .
docker build --target worker -t parakeet-worker:test .
uv run python scripts/smoke.py /path/to/recording.m4a
```

The smoke test submits three queued AssemblyAI requests and one OpenAI request, checks matching word timestamps, deletes results, then removes its containers and volumes. A quality benchmark needs a separate reference transcript; this smoke test does not measure word error rate.

## License

Application code is licensed under [EUPL-1.2](LICENSE). The worker contains [NVIDIA Parakeet TDT 0.6B v3](https://huggingface.co/nvidia/parakeet-tdt-0.6b-v3) weights under CC-BY-4.0, converted to INT8 ONNX by [sherpa-onnx](https://github.com/k2-fsa/sherpa-onnx). See [NOTICE](NOTICE) for attribution and the pinned archive checksum. Dependencies retain their own licenses. This project is not affiliated with OpenAI or AssemblyAI.
