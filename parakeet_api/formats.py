"""Both public APIs use the same decoder-aligned words, stored in milliseconds."""

import json
import math

from pydantic import BaseModel, ConfigDict, Field, model_validator


class Word(BaseModel):
    model_config = ConfigDict(extra="forbid", allow_inf_nan=False)
    text: str = Field(min_length=1, max_length=4096)
    start: int = Field(ge=0)
    end: int = Field(gt=0)
    confidence: float = Field(ge=0, le=1)
    speaker: None = None
    channel: None = None


class Result(BaseModel):
    model_config = ConfigDict(extra="forbid", allow_inf_nan=False)
    text: str = Field(max_length=2_000_000)
    words: list[Word] = Field(max_length=100_000)
    audio_duration_ms: int = Field(gt=0, le=10_800_000)
    chunks: int = Field(ge=1)
    seam_fallbacks: int = Field(ge=0)

    @model_validator(mode="after")
    def aligned(self):
        previous = (-1, -1)
        for word in self.words:
            if not word.start < word.end <= self.audio_duration_ms:
                raise ValueError("Word outside audio duration")
            if word.start < previous[0] or word.end < previous[1]:
                raise ValueError("Word timing must be ordered")
            previous = (word.start, word.end)
        if self.text != " ".join(w.text for w in self.words):
            raise ValueError("Text must match words")
        return self


def assembly(job, public_url):
    result = json.loads(job["result"]) if job["result"] else {}
    words = result.get("words")
    return {
        "id": job["id"],
        "status": job["status"],
        "error": job["error"],
        "audio_url": f"{public_url}/uploads/{job['upload_id']}",
        "text": result.get("text"),
        "words": words,
        "confidence": sum(w["confidence"] for w in words) / len(words) if words else None,
        "audio_duration": math.ceil(result["audio_duration_ms"] / 1000) if result else None,
        "language_code": json.loads(job["options"]).get("language_code"),
        "language_confidence": None,
        "utterances": None,
    }


def segments(result, max_chars=80):
    groups, current = [], []
    for word in result["words"]:
        if current and (
            len(" ".join(w["text"] for w in current)) + len(word["text"]) + 1 > max_chars
            or word["end"] - current[0]["start"] > 6000
            or word["start"] - current[-1]["end"] > 1500
        ):
            groups.append(current)
            current = []
        current.append(word)
    if current:
        groups.append(current)
    return [
        {
            "id": i,
            "start": words[0]["start"] / 1000,
            "end": words[-1]["end"] / 1000,
            "text": " ".join(w["text"] for w in words),
        }
        for i, words in enumerate(groups)
    ]


def timestamp(seconds, vtt):
    ms = round(seconds * 1000)
    hours, ms = divmod(ms, 3600000)
    minutes, ms = divmod(ms, 60000)
    seconds, ms = divmod(ms, 1000)
    return f"{hours:02}:{minutes:02}:{seconds:02}{'.' if vtt else ','}{ms:03}"


def subtitles(result, vtt=False, max_chars=80):
    blocks = ["WEBVTT\n"] if vtt else []
    for i, segment in enumerate(segments(result, max_chars)):
        # Model text must not inject subtitle markup or cue separators.
        text = segment["text"].replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")
        blocks.append(
            f"{i + 1}\n{timestamp(segment['start'], vtt)} --> {timestamp(segment['end'], vtt)}\n{text}\n"
        )
    return "\n".join(blocks)


def openai(result, granularities):
    body = {"task": "transcribe", "duration": result["audio_duration_ms"] / 1000, "text": result["text"]}
    if "word" in granularities:
        body["words"] = [
            {"word": w["text"], "start": w["start"] / 1000, "end": w["end"] / 1000} for w in result["words"]
        ]
    if "segment" in granularities:
        body["segments"] = segments(result)
    return body
