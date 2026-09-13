"""Exercise both real SDKs and the real CPU worker. Never print transcript contents."""

import json
import os
import secrets
import subprocess
import sys
import tempfile
import time
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

import assemblyai as aai
import httpx
from openai import OpenAI


def docker(*args):
    return subprocess.check_output(["docker", *args], text=True).strip()


def wait_ready(url, timeout=180):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            if httpx.get(url, timeout=2).status_code == 200:
                return
        except httpx.HTTPError:
            pass
        time.sleep(1)
    raise RuntimeError("API startup timed out")


def main():
    audio = Path(sys.argv[1])
    suffix = secrets.token_hex(4)
    network, api, worker = (f"parakeet-smoke-{suffix}-{kind}" for kind in ("net", "api", "worker"))
    key, worker_key = secrets.token_hex(32), secrets.token_hex(32)
    api_image = os.getenv("API_IMAGE", "parakeet-api:test")
    worker_image = os.getenv("WORKER_IMAGE", "parakeet-worker:test")
    started = time.monotonic()
    with tempfile.TemporaryDirectory() as scratch:
        env_file = Path(scratch) / "secrets"
        env_file.write_text(f"API_KEY={key}\nWORKER_API_KEY={worker_key}\n")
        env_file.chmod(0o600)
        try:
            docker("network", "create", network)
            docker(
                "run",
                "-d",
                "--name",
                api,
                "--network",
                network,
                "--network-alias",
                "api",
                "--env-file",
                str(env_file),
                "-p",
                "127.0.0.1::8080",
                "--cpus",
                "0.25",
                "--memory",
                "384m",
                "--read-only",
                "--tmpfs",
                "/data:uid=10001,gid=10001,size=512m",
                "--tmpfs",
                "/tmp:uid=10001,gid=10001,size=256m",
                api_image,
            )
            port = docker("port", api, "8080/tcp").rsplit(":", 1)[1]
            base = f"http://127.0.0.1:{port}"
            wait_ready(f"{base}/health/ready")
            # Populate the queue before starting a worker, proving requests wait.
            client = aai.Client(settings=aai.Settings(api_key=key, base_url=base, polling_interval=0.2))
            transcriber = aai.Transcriber(client=client)
            jobs = [transcriber.submit(str(audio)) for _ in range(3)]
            assert all(j.status == aai.TranscriptStatus.queued for j in jobs)
            docker(
                "run",
                "-d",
                "--name",
                worker,
                "--network",
                network,
                "--env-file",
                str(env_file),
                "-e",
                "API_URL=http://api:8080",
                "-e",
                "PARAKEET_THREADS=2",
                "-e",
                "TMPDIR=/scratch",
                "--cpus",
                "1.75",
                "--memory",
                "4g",
                "--read-only",
                "-v",
                "/scratch",
                worker_image,
            )
            for _ in range(180):
                status = docker("inspect", "--format", "{{.State.Status}}", worker)
                if status != "running":
                    raise RuntimeError("Worker failed to start; inspect its container logs")
                health = docker("inspect", "--format", "{{.State.Health.Status}}", worker)
                if health == "healthy":
                    break
                time.sleep(1)
            else:
                raise RuntimeError("Worker readiness timed out")
            sdk = OpenAI(api_key=key, base_url=f"{base}/v1", timeout=600, max_retries=0)
            with ThreadPoolExecutor(1) as pool:

                def openai_request():
                    with audio.open("rb") as source:
                        return sdk.audio.transcriptions.create(
                            model="parakeet",
                            file=source,
                            response_format="verbose_json",
                            timestamp_granularities=["word", "segment"],
                        )

                future = pool.submit(openai_request)
                outputs = []
                for job in jobs:
                    transcript = job.wait_for_completion()
                    assert transcript.status == aai.TranscriptStatus.completed, transcript.error
                    assert transcript.words and transcript.audio_duration > 0
                    assert all(
                        0 <= w.start < w.end <= transcript.audio_duration * 1000 for w in transcript.words
                    )
                    assert all(
                        a.start <= b.start and a.end <= b.end
                        for a, b in zip(transcript.words, transcript.words[1:])
                    )
                    assert "WEBVTT" in transcript.export_subtitles_vtt()
                    outputs.append(transcript)
                output = future.result(timeout=600)
            # Same audio must expose identical alignment in both units.
            assert len(output.words) == len(outputs[0].words)
            for a, b in zip(output.words, outputs[0].words):
                assert a.word == b.text and round(a.start * 1000) == b.start and round(a.end * 1000) == b.end
            aai.settings = client.settings
            aai.Client._default = client
            for transcript in outputs:
                aai.Transcript.delete_by_id(transcript.id)
            with httpx.Client(headers={"authorization": key}) as http:
                for job in jobs:
                    assert http.get(f"{base}/v2/transcript/{job.id}").status_code == 404
            print(
                json.dumps(
                    {
                        "sdk_roundtrips": 4,
                        "audio_seconds": output.duration,
                        "words": len(output.words),
                        "elapsed_seconds": round(time.monotonic() - started, 2),
                    }
                )
            )
            print(docker("stats", "--no-stream", "--format", "{{.Name}}: {{.MemUsage}}", api, worker))
        finally:
            for name in (worker, api):
                subprocess.run(
                    ["docker", "rm", "-f", "-v", name],
                    stdout=subprocess.DEVNULL,
                    stderr=subprocess.DEVNULL,
                    check=False,
                )
            subprocess.run(
                ["docker", "network", "rm", network],
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                check=False,
            )


if __name__ == "__main__":
    main()
