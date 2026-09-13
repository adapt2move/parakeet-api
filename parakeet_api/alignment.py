"""Convert decoder token timing and scores to AssemblyAI's word shape."""

import math
import re


def words_from_result(result, duration_ms):
    tokens = result["tokens"]
    starts = result["timestamps"]
    durations = result["durations"]
    scores = result["ys_log_probs"]
    if not (len(tokens) == len(starts) == len(durations) == len(scores)):
        raise ValueError("Decoder did not return token durations and scores")
    words = []
    current = None
    for token, start, duration, score in zip(tokens, starts, durations, scores):
        if not all(math.isfinite(x) for x in (start, duration, score)) or duration < 0:
            raise ValueError("Invalid decoder timing or score")
        normalized = token.replace("▁", " ").replace("Ġ", " ")
        for part in re.finditer(r"\s+|\S+", normalized):
            if part.group().isspace():
                current = None
                continue
            # TDT timing is decoder alignment, not an independent forced aligner.
            first = max(0, min(duration_ms - 1, round(start * 1000)))
            last = min(duration_ms, max(first + 1, round((start + duration) * 1000)))
            probability = math.exp(min(0, score))
            if current is None:
                current = {
                    "text": part.group(),
                    "start": first,
                    "end": last,
                    "confidence": probability,
                    "speaker": None,
                    "channel": None,
                    "_scores": [probability],
                }
                words.append(current)
            else:
                current["text"] += part.group()
                current["end"] = max(current["end"], last)
                current["_scores"].append(probability)
                current["confidence"] = sum(current["_scores"]) / len(current["_scores"])
    for word in words:
        del word["_scores"]

    def content(x):
        return re.sub(r"\s", "", x)

    if content(" ".join(w["text"] for w in words)) != content(result["text"]):
        raise ValueError("Decoder text and tokens disagree")
    return words


def validate_options(options):
    if not isinstance(options, dict):
        raise ValueError("config must be an object")
    allowed = {"language_code", "sample_rate", "channels", "_pcm", "timestamps"}
    if set(options) - allowed:
        raise ValueError("Unsupported config fields: " + ", ".join(sorted(set(options) - allowed)))
    if "timestamps" in options and type(options["timestamps"]) is not bool:
        raise ValueError("timestamps must be boolean")
    language = options.get("language_code")
    if language is not None and (
        not isinstance(language, str)
        or language
        not in {
            "bg",
            "hr",
            "cs",
            "da",
            "nl",
            "en",
            "et",
            "fi",
            "fr",
            "de",
            "el",
            "hu",
            "it",
            "lv",
            "lt",
            "mt",
            "pl",
            "pt",
            "ro",
            "ru",
            "sk",
            "sl",
            "es",
            "sv",
            "uk",
        }
    ):
        raise ValueError("Unsupported language_code")
    if "sample_rate" in options and (
        type(options["sample_rate"]) is not int
        or options["sample_rate"] not in {8000, 16000, 22050, 24000, 32000, 44100, 48000}
    ):
        raise ValueError("Unsupported sample_rate")
    if "channels" in options and (type(options["channels"]) is not int or options["channels"] not in {1, 2}):
        raise ValueError("channels must be 1 or 2")
    # Parakeet decodes multilingually; this is a caller label, not forced decoding or language identification.
    return language
