import pytest

from parakeet_api.alignment import words_from_result
from parakeet_api.chunking import merge_words, windows


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
    merged, seam = merge_words(old, new, 105000)
    assert seam is None
    assert [w["text"] for w in merged] == ["again", "the", "same", "phrase", "again"]
    assert all(w in old or w in new for w in merged)


def test_windows_cover_end_without_extra_chunk():
    assert list(windows(120 * 16000)) == [(0, 120 * 16000)]
    spans = list(windows(287 * 16000))
    assert len(spans) == 3 and spans[-1][1] == 287 * 16000
    assert spans[1][0] == 105 * 16000


def test_overlap_recovers_a_passage_the_previous_window_skipped():
    # The first window ends at 120 s and decoded nothing between "clocks." and "The",
    # while the second window heard the passage; both agree on "The hospital received".
    old = [word("clocks.", 104640), word("The", 117840), word("hospital", 118080), word("received", 118720)]
    skipped = [
        word(text, 105800 + 500 * i) for i, text in enumerate("Curators restored a rare clock".split())
    ]
    new = skipped + [
        word("The", 117800),
        word("hospital", 118120),
        word("received", 118840),
        word("new", 119400),
    ]
    merged, seam = merge_words(old, new, 105000)
    assert seam == "recovered"
    assert [
        w["text"] for w in merged
    ] == "clocks. Curators restored a rare clock The hospital received new".split()


def test_overlap_keeps_previous_window_for_small_differences():
    old = [word("one", 106000), word("the", 108000), word("same", 109000), word("phrase", 110000)]
    new = [
        word("one", 106010),
        word("two", 107000),
        word("the", 108010),
        word("same", 109010),
        word("phrase", 110010),
    ]
    merged, seam = merge_words(old, new, 105000)
    assert seam is None
    assert [w["text"] for w in merged] == ["one", "the", "same", "phrase"]
