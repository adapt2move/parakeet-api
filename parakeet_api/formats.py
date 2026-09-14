"""The result a worker posts to the API: decoder-aligned words, stored in milliseconds."""

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
