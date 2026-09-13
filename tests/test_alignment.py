import pytest

from parakeet_api.alignment import words_from_result
from parakeet_api.chunking import merge_words, windows
from parakeet_api.formats import subtitles


def test_subwords_keep_decoder_timing():
    output = words_from_result(
        {
            "text": "Hello world.",
            "tokens": ["▁Hel", "lo", "▁world", "."],
            "timestamps": [0.12, 0.3, 0.9, 1.6],
            "durations": [0.18, 0.31, 0.7, 0.1],
            "ys_log_probs": [-0.1, -0.2, -0.3, -0.4],
        },
        2100,
    )
    assert [(w["text"], w["start"], w["end"]) for w in output] == [("Hello", 120, 610), ("world.", 900, 1700)]


def test_missing_alignment_is_an_error():
    with pytest.raises(ValueError):
        words_from_result(
            {"text": "Hi", "tokens": ["Hi"], "timestamps": [], "durations": [], "ys_log_probs": []}, 1000
        )


def word(text, start):
    return {"text": text, "start": start, "end": start + 500}


def test_overlap_deduplicates_only_the_overlap():
    old = [word("again", 1000), word("the", 108000), word("same", 109000), word("phrase", 110000)]
    new = [word("the", 108010), word("same", 109010), word("phrase", 110010), word("again", 120000)]
    merged, fallback = merge_words(old, new, 105000)
    assert not fallback
    assert [w["text"] for w in merged] == ["again", "the", "same", "phrase", "again"]
    assert all(w in old or w in new for w in merged)


def test_windows_cover_end_without_extra_chunk():
    assert list(windows(120 * 16000)) == [(0, 120 * 16000)]
    spans = list(windows(287 * 16000))
    assert len(spans) == 3 and spans[-1][1] == 287 * 16000
    assert spans[1][0] == 105 * 16000


def test_subtitles_keep_alignment_and_escape_markup(result):
    result["words"][0]["text"] = "<Hello>"
    assert "&lt;Hello&gt;" in subtitles(result)
    assert "00:00:00,120 --> 00:00:01,700" in subtitles(result)
