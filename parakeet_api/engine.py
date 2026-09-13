"""Bounded-memory INT8 inference using sherpa-onnx TDT token alignment."""

import os
import subprocess
import time
from pathlib import Path

import numpy as np
import sherpa_onnx

from .alignment import words_from_result
from .chunking import merge_words, validate_duration, windows
from .formats import Result


class Engine:
    def __init__(self):
        models = Path(os.getenv("MODEL_DIR", "/models"))
        self.max_seconds = int(os.getenv("MAX_AUDIO_SECONDS", "10800"))
        self.timeout = int(os.getenv("JOB_TIMEOUT_SECONDS", "1800"))
        if not 1 <= self.max_seconds <= 10800 or self.timeout < 1:
            raise ValueError("Invalid worker duration/time limit")
        self.recognizer = sherpa_onnx.OfflineRecognizer.from_transducer(
            encoder=str(models / "encoder.int8.onnx"),
            decoder=str(models / "decoder.int8.onnx"),
            joiner=str(models / "joiner.int8.onnx"),
            tokens=str(models / "tokens.txt"),
            num_threads=int(os.getenv("PARAKEET_THREADS", "3")),
            model_type="nemo_transducer",
            provider="cpu",
            decoding_method="greedy_search",
        )

    def transcribe(self, directory, cancel, progress):
        deadline = time.monotonic() + self.timeout

        def check():
            if cancel.is_set():
                raise InterruptedError("Job cancelled")
            if time.monotonic() >= deadline:
                raise ValueError("Transcription time limit exceeded")
            progress()

        source, output = Path(directory) / "input", Path(directory) / "audio.f32"
        command = [
            "ffmpeg",
            "-nostdin",
            "-v",
            "error",
            "-threads",
            "1",
            "-protocol_whitelist",
            "file,pipe",
            "-format_whitelist",
            "aac,ac3,aiff,amr,ape,asf,au,avi,caf,flac,matroska,webm,mov,mp3,mpeg,mpegts,ogg,wav,wv",
            "-i",
            str(source),
            "-map",
            "0:a:0",
            "-vn",
            "-ac",
            "1",
            "-ar",
            "16000",
            "-t",
            str(self.max_seconds + 1),
            "-threads",
            "1",
            "-f",
            "f32le",
            str(output),
        ]
        with subprocess.Popen(command, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL) as process:
            try:
                decode_deadline = min(deadline, time.monotonic() + 300)
                while process.poll() is None:
                    check()
                    if time.monotonic() > decode_deadline:
                        raise ValueError("Audio decoding time limit exceeded")
                    time.sleep(0.1)
                if process.returncode != 0:
                    raise ValueError("Audio could not be decoded")
            finally:
                if process.poll() is None:
                    process.kill()
                    process.wait()
        count = output.stat().st_size // 4
        validate_duration(count, self.max_seconds)
        combined, chunks, fallbacks = [], 0, 0
        with output.open("rb") as pcm:
            for first, last in windows(count):
                check()
                pcm.seek(first * 4)
                samples = np.fromfile(pcm, dtype=np.float32, count=last - first)
                if len(samples) != last - first or not np.isfinite(samples).all():
                    raise ValueError("Invalid decoded audio")
                stream = self.recognizer.create_stream()
                stream.accept_waveform(16000, samples)
                self.recognizer.decode_stream(stream)
                decoded = stream.result
                fields = ("text", "tokens", "timestamps", "durations", "ys_log_probs")
                words = words_from_result(
                    {key: getattr(decoded, key) for key in fields}, round((last - first) / 16)
                )
                for word in words:
                    word["start"] += first // 16
                    word["end"] += first // 16
                combined, fallback = merge_words(combined, words, first // 16)
                chunks += 1
                fallbacks += int(fallback)
                if len(combined) > 100000:
                    raise ValueError("Transcript word limit exceeded")
                del stream, decoded, samples
                check()
        return Result(
            text=" ".join(w["text"] for w in combined),
            words=combined,
            audio_duration_ms=round(count / 16),
            chunks=chunks,
            seam_fallbacks=fallbacks,
        ).model_dump()
