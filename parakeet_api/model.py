"""Model selection stays independent of API protocol and deployment layout."""

import os
from pathlib import Path


def model_files():
    directory = Path(os.getenv("MODEL_DIR", "/models"))
    precision = os.getenv("MODEL_PRECISION", "int8")
    if precision not in ("int8", "fp32"):
        raise ValueError(
            "MODEL_PRECISION must be int8 or fp32; custom exports can use explicit MODEL_* paths"
        )
    suffix = ".int8.onnx" if precision == "int8" else ".onnx"
    files = {
        part: str(Path(os.getenv(f"MODEL_{part.upper()}", str(directory / (part + suffix)))))
        for part in ("encoder", "decoder", "joiner")
    }
    files["tokens"] = os.getenv("MODEL_TOKENS", str(directory / "tokens.txt"))
    if not all(Path(path).is_file() for path in files.values()):
        raise ValueError(
            "Model files missing; mount a compatible ONNX model and configure MODEL_DIR / MODEL_* paths"
        )
    return files
