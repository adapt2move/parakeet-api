"""Check the worker's compiled decoders using synthetic audio encoded by host FFmpeg."""

import array
import math
import os
import subprocess
import sys
import tempfile
import wave
from pathlib import Path

# Each fixture exercises a different container/codec combination.
FORMATS = [
    ("wav", "pcm_s16le"),
    ("wav", "pcm_s24le"),
    ("wav", "pcm_f32le"),
    ("wav", "adpcm_ima_wav"),
    ("m4a", "aac"),
    ("m4a", "alac"),
    ("mp3", "libmp3lame"),
    ("flac", "flac"),
    ("ogg", "libvorbis"),
    ("ogg", "libopus"),
    ("webm", "libopus"),
    ("aiff", "pcm_s16be"),
    ("au", "pcm_mulaw"),
    ("caf", "pcm_s16le"),
    ("wv", "wavpack"),
    ("wma", "wmav2"),
    ("ac3", "ac3"),
    ("aac", "aac"),
    ("ts", "ac3"),
    ("mpg", "mp2"),
]


def main():
    source = Path(sys.argv[1]).resolve()
    image = os.getenv("WORKER_IMAGE", "parakeet-worker:test")
    with wave.open(str(source)) as wav:
        duration = wav.getnframes() / wav.getframerate()
    with tempfile.TemporaryDirectory() as directory:
        Path(directory).chmod(0o755)
        for extension, codec in FORMATS:
            encoded = Path(directory) / f"{codec}.{extension}"
            subprocess.run(
                ["ffmpeg", "-nostdin", "-v", "error", "-i", str(source), "-c:a", codec, str(encoded)],
                check=True,
            )
            encoded.chmod(0o644)
            decoded = subprocess.check_output(
                [
                    "docker",
                    "run",
                    "--rm",
                    "--read-only",
                    "--network",
                    "none",
                    "--cap-drop=ALL",
                    "--security-opt=no-new-privileges",
                    "--entrypoint",
                    "ffmpeg",
                    "-v",
                    f"{directory}:/fixtures:ro",
                    image,
                    "-nostdin",
                    "-v",
                    "error",
                    "-threads",
                    "1",
                    "-protocol_whitelist",
                    "file,pipe",
                    "-i",
                    f"/fixtures/{encoded.name}",
                    "-map",
                    "0:a:0",
                    "-vn",
                    "-ac",
                    "1",
                    "-ar",
                    "16000",
                    "-f",
                    "f32le",
                    "pipe:1",
                ]
            )
            samples = array.array("f", decoded)
            assert abs(len(samples) / 16000 - duration) < 0.25, (extension, codec, len(samples))
            assert all(math.isfinite(sample) for sample in samples), (extension, codec)
            assert max(abs(sample) for sample in samples) > 0.01, (extension, codec)
            print(f"Decoded {extension}/{codec}", flush=True)


if __name__ == "__main__":
    main()
