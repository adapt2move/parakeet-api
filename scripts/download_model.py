"""Fetch a checksum-pinned, publicly redistributable INT8 Parakeet model."""

import hashlib
import tarfile
import tempfile
import urllib.request
from pathlib import Path

URL = "https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/sherpa-onnx-nemo-parakeet-tdt-0.6b-v3-int8.tar.bz2"
SHA256 = "5793d0fd397c5778d2cf2126994d58e9d56b1be7c04d13c7a15bb1b4eafb16bf"
FILES = {"encoder.int8.onnx", "decoder.int8.onnx", "joiner.int8.onnx", "tokens.txt"}


def main():
    target = Path("/models")
    target.mkdir(exist_ok=True)
    with tempfile.TemporaryDirectory() as directory:
        archive = Path(directory) / "model.tar.bz2"
        urllib.request.urlretrieve(URL, archive)
        with archive.open("rb") as source:
            if hashlib.file_digest(source, "sha256").hexdigest() != SHA256:
                raise RuntimeError("Model checksum mismatch")
        found = set()
        with tarfile.open(archive) as tar:
            for member in tar:
                name = Path(member.name).name
                if name in FILES and member.isfile():
                    with tar.extractfile(member) as source, (target / name).open("wb") as output:
                        import shutil

                        shutil.copyfileobj(source, output)
                    found.add(name)
        if found != FILES:
            raise RuntimeError("Missing model files")


if __name__ == "__main__":
    main()
