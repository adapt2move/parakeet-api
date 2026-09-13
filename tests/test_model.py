import pytest

from parakeet_api.model import model_files


@pytest.mark.parametrize("precision,suffix", [("int8", ".int8.onnx"), ("fp32", ".onnx")])
def test_precision_selects_exported_files(tmp_path, monkeypatch, precision, suffix):
    monkeypatch.setenv("MODEL_DIR", str(tmp_path))
    monkeypatch.setenv("MODEL_PRECISION", precision)
    for name in [part + suffix for part in ("encoder", "decoder", "joiner")] + ["tokens.txt"]:
        (tmp_path / name).touch()
    files = model_files()
    assert files["encoder"] == str(tmp_path / ("encoder" + suffix))
    custom = tmp_path / "custom-encoder.onnx"
    custom.touch()
    monkeypatch.setenv("MODEL_ENCODER", str(custom))
    assert model_files()["encoder"] == str(custom)


def test_missing_model_is_rejected_before_native_initialization(tmp_path, monkeypatch):
    monkeypatch.setenv("MODEL_DIR", str(tmp_path))
    with pytest.raises(ValueError, match="Model files missing"):
        model_files()
