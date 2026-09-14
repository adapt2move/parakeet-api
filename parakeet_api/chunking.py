"""Bounded audio windows and alignment of overlapping decoder words."""

import unicodedata


def validate_duration(sample_count, max_seconds, sample_rate=16000):
    if not 0 < sample_count <= max_seconds * sample_rate:
        raise ValueError(f"Audio must be nonempty and at most {max_seconds} seconds")


def windows(sample_count, sample_rate=16000, seconds=120, overlap=15):
    if not 0 < overlap < seconds or sample_count <= 0:
        raise ValueError("Invalid window configuration")
    size, stride = seconds * sample_rate, (seconds - overlap) * sample_rate
    for start in range(0, sample_count, stride):
        end = min(start + size, sample_count)
        yield start, end
        if end == sample_count:
            break


def normalized(text):
    return "".join(c for c in unicodedata.normalize("NFKC", text).casefold() if c.isalnum())


def ordered(words):
    return all(a["start"] <= b["start"] and a["end"] <= b["end"] for a, b in zip(words, words[1:]))


def merge_words(previous, incoming, window_start_ms, overlap_ms=15000):
    """Pick an unchanged acoustic alignment at a matched word inside the overlap.

    Only the overlap participates. Repeated phrases elsewhere are retained.
    Returns the merged words and the seam kind: None for a clean match,
    "recovered" when the incoming window filled a passage the previous window
    skipped, "fallback" for a temporal split without a common recognition.
    The caller records seam counts for quality review.
    """
    if not previous:
        return incoming, None
    if not incoming:
        return previous, None
    if previous[-1]["end"] <= incoming[0]["start"]:
        return previous + incoming, None
    old = [(i, w) for i, w in enumerate(previous) if w["end"] >= window_start_ms]
    new = [(i, w) for i, w in enumerate(incoming) if w["start"] <= window_start_ms + overlap_ms]
    # Maximum contiguous matching run with a timing constraint. Window sizes
    # bound this quadratic search independently of total recording duration.
    best = []
    for i in range(len(old)):
        for j in range(len(new)):
            run = []
            while i + len(run) < len(old) and j + len(run) < len(new):
                a, b = old[i + len(run)], new[j + len(run)]
                if (
                    not normalized(a[1]["text"])
                    or normalized(a[1]["text"]) != normalized(b[1]["text"])
                    or abs(a[1]["start"] - b[1]["start"]) > overlap_ms / 2
                ):
                    break
                run.append((a[0], b[0]))
            if len(run) > len(best):
                best = run
    # Try the center of the strongest match, then the other matching seams.
    if best:
        center = len(best) // 2
        candidates = [best[center]] + best[:center] + best[center + 1 :]
        for a, b in candidates:
            merged = recovered(previous, incoming, a, b, window_start_ms)
            if merged is not None:
                return merged, "recovered"
            merged = previous[:a] + incoming[b:]
            if ordered(merged):
                return merged, None
    cutoff = window_start_ms + overlap_ms / 2
    merged = [w for w in previous if (w["start"] + w["end"]) / 2 < cutoff] + [
        w for w in incoming if (w["start"] + w["end"]) / 2 >= cutoff
    ]
    if not ordered(merged):
        raise ValueError("Overlapping chunks have inconsistent word timing")
    return merged, "fallback"


def recovered(previous, incoming, a, b, window_start_ms, margin=3):
    """Take the incoming window before the seam when the previous window skipped words there.

    The decoder sometimes drops a passage right before a window edge. Both windows
    hear the overlap, so a clearly longer incoming reading of it is the better one.
    """
    heard = sum(w["end"] > window_start_ms for w in previous[:a])
    if b < heard + margin:
        return None
    merged = [w for w in previous if w["end"] <= incoming[0]["start"]] + incoming
    return merged if ordered(merged) else None
