"""Regenerate the golden fixtures from the Python reference implementation.

Run from a checkout of the Python API (commit e149506):
    PYTHONPATH=. uv run python internal/formats/testdata/generate.py

Request decoding cases go through the real FastAPI app, so the fixtures record the status codes
FastAPI and pydantic produce, not a reimplementation of them.
"""

import base64
import copy
import gzip
import hashlib
import json
import os
import random
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
from pathlib import Path

from fastapi import HTTPException
from fastapi.encoders import jsonable_encoder
from fastapi.testclient import TestClient
from starlette.responses import JSONResponse

import parakeet_api.api as api
from parakeet_api.alignment import validate_options
from parakeet_api.formats import Result, assembly, openai, segments, subtitles, timestamp
from parakeet_api.settings import Settings

HERE = Path(__file__).parent
INT64_MAX = 2**63 - 1


def render(content):
    return JSONResponse(jsonable_encoder(content)).body.decode()


def write(name, data):
    text = json.dumps(data, indent=1, ensure_ascii=False) + "\n"
    if name.endswith(".gz"):
        (HERE / name).write_bytes(gzip.compress(text.encode(), mtime=0))
    else:
        (HERE / name).write_text(text)


def with_body(case, raw):
    # Readable bodies where possible; bytes that are not strict UTF-8 go through base64.
    try:
        return dict(case, body_text=raw.decode("utf-8"))
    except UnicodeDecodeError:
        return dict(case, body_b64=base64.b64encode(raw).decode())


BASE = {
    "text": "Hello world.",
    "audio_duration_ms": 2100,
    "chunks": 1,
    "seam_fallbacks": 0,
    "words": [
        {"text": "Hello", "start": 120, "end": 610, "confidence": 0.8},
        {"text": "world.", "start": 900, "end": 1700, "confidence": 0.7},
    ],
}


def base(**fields):
    result = copy.deepcopy(BASE)
    result.update(fields)
    return result


def one_word(text="a", duration=1000, **word):
    w = {"text": text, "start": 0, "end": 10, "confidence": 0.5}
    w.update(word)
    return {"text": text, "audio_duration_ms": duration, "chunks": 1, "seam_fallbacks": 0, "words": [w]}


def splice(template, marker, literal):
    return json.dumps(template).replace(json.dumps(marker), literal)


def completion_cases():
    cases = []

    def body(name, raw, content_type="application/json"):
        cases.append((name, raw, content_type))

    def result(name, value):
        body(name, json.dumps({"result": value}, ensure_ascii=False))

    result("valid", base())
    result(
        "valid_speaker_channel_null", base(words=[dict(w, speaker=None, channel=None) for w in BASE["words"]])
    )
    result("valid_empty", {"text": "", "words": [], "audio_duration_ms": 1, "chunks": 1, "seam_fallbacks": 0})
    result(
        "empty_words_nonempty_text",
        {"text": "x", "words": [], "audio_duration_ms": 1, "chunks": 1, "seam_fallbacks": 0},
    )
    result("speaker_string", base(words=[dict(BASE["words"][0], speaker="A"), BASE["words"][1]]))
    result("channel_zero", base(words=[dict(BASE["words"][0], channel=0), BASE["words"][1]]))
    result("word_extra_field", base(words=[dict(BASE["words"][0], extra=1), BASE["words"][1]]))
    for field in ("text", "start", "end", "confidence"):
        word = dict(BASE["words"][0])
        del word[field]
        result(f"word_missing_{field}", base(words=[word, BASE["words"][1]]))
    for field in ("text", "words", "audio_duration_ms", "chunks", "seam_fallbacks"):
        value = base()
        del value[field]
        result(f"result_missing_{field}", value)
    result("result_extra_field", base(language="en"))
    result("result_case_key", {("Text" if k == "text" else k): v for k, v in base().items()})
    result("start_equals_end", one_word(start=10, end=10))
    result("start_after_end", one_word(start=11, end=10))
    result("end_equals_duration", one_word(end=1000))
    result("end_after_duration", one_word(end=1001))
    result("start_negative", one_word(start=-1))
    result("end_zero", one_word(start=0, end=0))
    result("duration_zero", one_word(duration=0))
    result("duration_negative", one_word(duration=-5))
    result("duration_max", one_word(duration=10_800_000))
    result("duration_over_max", one_word(duration=10_800_001))
    result("chunks_zero", base(chunks=0))
    result("chunks_big", base(chunks=10**30))
    result("chunks_int64_max", base(chunks=INT64_MAX))
    result("chunks_int64_over", base(chunks=INT64_MAX + 1))
    result("seam_negative", base(seam_fallbacks=-1))
    result("seam_big_negative", base(seam_fallbacks=-(10**30)))
    for value in (0, 1, -0.0, 5e-324, 1.0000000000000002, -1e-300, 0.9999999999999999, 2):
        result(f"confidence_{value!r}", one_word(confidence=value))
    result("word_text_empty", one_word(text=""))
    result("word_text_space", one_word(text=" "))
    result("word_text_nul", one_word(text="a\x00b"))
    ordered = [
        ("same_start", [(0, 5), (0, 8)], True),
        ("same_end", [(0, 8), (3, 8)], True),
        ("overlap", [(0, 500), (100, 600)], True),
        ("start_decreasing", [(100, 500), (50, 600)], False),
        ("end_decreasing", [(0, 500), (100, 400)], False),
        ("identical", [(5, 9), (5, 9)], True),
    ]
    for name, spans, _ in ordered:
        words = [{"text": "w", "start": a, "end": b, "confidence": 0.5} for a, b in spans]
        result(
            f"order_{name}",
            {"text": "w w", "words": words, "audio_duration_ms": 1000, "chunks": 1, "seam_fallbacks": 0},
        )
    result("text_double_space", base(text="Hello  world."))
    result("text_trailing_space", base(text="Hello world. "))
    result("text_other", base(text="Hello World."))
    result("text_unicode_nfc_vs_nfd", one_word(text="\xe9") | {"text": "e\u0301"})
    result("text_newline_word", {**one_word(text="a\nb")})
    result("words_object", base(words={}))
    result("words_null", base(words=None))
    result("result_list", [])
    result("result_string", "x")
    result("result_number", 1)

    # Lax coercions pydantic applies to JSON input; mirror them exactly.
    int_literals = [
        "5.0",
        "5.5",
        '"5"',
        '" 5 "',
        '"5.0"',
        '"5.00"',
        '"5."',
        '".0"',
        '"1e3"',
        '"05"',
        '"+5"',
        '"+5.0"',
        '"-0"',
        '"-0.0"',
        "true",
        "false",
        "null",
        "-0.0",
        "-0",
        "1.5e1",
        "1E1",
        "5.0000000000000001",
        '"0x10"',
        '"1_000"',
        "[]",
        "{}",
        '""',
        '" "',
        '"a"',
        '"\\u0665"',
        '"5\\n"',
        '"\\u00a05"',
        '"\\t5\\t"',
        '"\\u20035"',
        '"\\u200b5"',
        '"\\u00855"',
        '"\\u000b5"',
        '"\\u001c5"',
        '"5 5"',
        '"+-5"',
        '"5.1"',
        '"5.0e0"',
        '"5.0 "',
        '"5 .0"',
        '"0."',
        '"1.e0"',
        '"00"',
        '"-00.00"',
        '"5.0.0"',
        "1e-5",
        "NaN",
        "Infinity",
        "-Infinity",
        "1e400",
    ]
    for literal in int_literals:
        body(f"start_{literal}", splice({"result": one_word(start="@S", end=10)}, "@S", literal))
    big_literals = [
        "1e3",
        "1e18",
        "1e20",
        "1e308",
        "9.2e18",
        "9.223372036854775e18",
        "9.223372036854776e18",
        "100000000000000000000",
        '"99999999999999999999999"',
        '"9223372036854775808"',
        "9223372036854775808",
        '"' + "1" * 4300 + '"',
        '"' + "1" * 4301 + '"',
        '"' + "0" * 10 + "1" * 4300 + '"',
        '"+' + "1" * 4300 + '"',
        '"-' + "0" * 4301 + '"',
        '"' + "1" * 4300 + '.0"',
        "1" * 4300,
        "1" * 4301,
        "-" + "1" * 4300,
        "-" + "1" * 4301,
    ]
    for i, literal in enumerate(big_literals):
        body(f"chunks_literal_{i}", splice({"result": base(chunks="@C")}, "@C", literal))
        body(f"seam_literal_{i}", splice({"result": base(seam_fallbacks="@C")}, "@C", literal))
    float_literals = [
        "5.0",
        '"5"',
        "true",
        "false",
        "null",
        "-0.0",
        "-0",
        "1",
        "0",
        "1.0",
        "1e3",
        "1e-400",
        '"1e-400"',
        '"0.5"',
        '" 0.5"',
        '"0.5\\n"',
        '"\\u00a00.5"',
        '"1e-1"',
        '"1E-1"',
        '".5"',
        '"+.5"',
        '"5."',
        '"0."',
        '"-0"',
        '"-0.0"',
        '"nan"',
        '"inf"',
        '"Infinity"',
        '"infinity"',
        '"1_0"',
        '"0x1p0"',
        '"0.1f"',
        '"0,5"',
        '"  "',
        '""',
        '"0.50000000000000000000000000001"',
        "0.99999999999999999999",
        '"\\u0661"',
        '"5e"',
        '"e5"',
        '"."',
        '"+"',
        '"-.5"',
        '"0.5e-0"',
        '"1.e-1"',
        '"00.5"',
        "NaN",
        "Infinity",
        "1e400",
        "100000000000000000000",
        "[]",
        '"0.e0"',
        '"1."',
        '"+1."',
        '"-.0"',
        '"1e+0"',
        '"0.5e+00"',
        '"0.5E5"',
        '"1.0e-0"',
        '"-0.e-5"',
        '"0.5e"',
        '"0.5e+"',
        '"0.5 e1"',
        '"0.5\\u0000"',
        '"\\u00a0\\u3000.25\\u2029"',
    ]
    for literal in float_literals:
        body(f"confidence_{literal}", splice({"result": one_word(confidence="@F")}, "@F", literal))
    text_literals = ["5", "true", "null", "[]", '"\\u0035"', '"\\ud83d\\ude00"', '"\\ud800"', '"\\udc00"']
    for literal in text_literals:
        body(
            f"text_{literal}",
            '{"result": {"text": %s, "audio_duration_ms": 1000, "chunks": 1, "seam_fallbacks": 0, '
            '"words": [{"text": %s, "start": 0, "end": 10, "confidence": 0.5}]}}' % (literal, literal),
        )
    body("word_text_escaped_pair", '{"result": ' + json.dumps(one_word(text="\U0001f600")) + "}")

    for literal in [
        "true",
        "false",
        "0",
        "1",
        "2",
        "-1",
        "-0",
        "-0.0",
        "0.0",
        "1.0",
        "1.5",
        "1e0",
        "0e0",
        "null",
        "[]",
        "100000000000000000001",
        '"true"',
        '"True"',
        '"TRUE"',
        '"yes"',
        '"Yes"',
        '"on"',
        '"oN"',
        '"t"',
        '"y"',
        '"1"',
        '"0"',
        '"off"',
        '"n"',
        '"f"',
        '"no"',
        '"false"',
        '"FALSE"',
        '""',
        '" true"',
        '"true "',
        '"2"',
        '"none"',
        '"\\u0074rue"',
        "NaN",
    ]:
        body(f"retry_{literal}", '{"error": "boom", "retry": %s}' % literal)
    for name, value in [
        ("x200", "x" * 200),
        ("x201", "x" * 201),
        ("accent200", "\xe9" * 200),
        ("emoji200", "\U0001f600" * 200),
        ("emoji201", "\U0001f600" * 201),
        ("empty", ""),
        ("control", "a\x01\x7f"),
    ]:
        body(f"error_{name}", json.dumps({"error": value}, ensure_ascii=False))
    body("error_int", '{"error": 5}')
    body("error_bool", '{"error": true}')
    body("error_null", '{"error": null}')
    body("error_lone_surrogate", '{"error": "\\ud83dx"}')
    body("error_pair", '{"error": "\\uD83D\\uDE00x"}')
    body("error_high_high", '{"error": "\\ud83d\\ud83d"}')
    body("error_high_bad_escape", '{"error": "\\ud83d\\uzzzz"}')
    body("error_high_at_end", '{"error": "\\ud83d"}')
    body("both", json.dumps({"result": BASE, "error": "boom"}))
    body("neither", "{}")
    body("both_null", '{"result": null, "error": null}')
    body("result_null_retry", '{"result": null, "retry": true}')
    body("extra_top", '{"error": "boom", "x": 1}')
    body("dup_error_null", '{"error": "boom", "error": null}')
    body("dup_result_null", '{"result": %s, "result": null}' % json.dumps(BASE))
    body(
        "dup_text",
        '{"result": {"text": "a", "text": "", "audio_duration_ms": 1, "chunks": 1, '
        '"seam_fallbacks": 0, "words": []}}',
    )
    body("top_array", "[]")
    body("top_string", '"x"')
    body("top_null", "null")
    body("empty_body", "")
    body("whitespace_body", "   ")
    body("trailing_ws", '{"error": "boom"}  \n\t\r')
    body("leading_ws", ' \n{"error": "boom"}')
    body("trailing_ff", '{"error": "boom"}\x0c')
    body("trailing_nbsp", '{"error": "boom"}\xa0')
    body("trailing_garbage", '{"error": "boom"} x')
    body("trailing_comma_object", '{"error": "boom",}')
    body("trailing_comma_array", '{"error": "boom", "x": [1,]}')
    body("single_quotes", "{'error': 'boom'}")
    body("unterminated", '{"error": "boom"')
    body("unterminated_string", '{"error": "boom')
    body("bad_escape", '{"error": "\\x"}')
    body("escapes", '{"error": "\\"\\\\\\/\\b\\f\\n\\r\\t\\u00e9"}')
    body("raw_control", '{"error": "a\x01"}')
    body("raw_tab", '{"error": "a\tb"}')
    body("raw_del", '{"error": "a\x7f"}')
    body("leading_zero", '{"error": "boom", "retry": 01}')
    body("dot_no_digits", '{"error": "boom", "retry": 1.}')
    body("exp_no_digits", '{"error": "boom", "retry": 1e}')
    body("minus_alone", '{"error": "boom", "retry": -}')
    body("plus_number", '{"error": "boom", "retry": +1}')
    body("upper_true", '{"error": "boom", "retry": True}')
    body("nan_retry", '{"error": "boom", "retry": NaN}')
    body("key_not_string", '{error: "boom"}')
    body("missing_colon", '{"error" "boom"}')
    body("nested_ok", '{"error": "boom", "x": [[1, {"a": [true, null]}]]}')
    body("long_int_then_syntax", '{"error": "boom", "retry": ' + "1" * 4301 + ', "x": }')
    body("syntax_then_long_int", '{"error": "boom", "x": }, "retry": ' + "1" * 4301 + "}")
    body("long_int_in_extra", '{"error": "boom", "x": ' + "1" * 4301 + "}")
    body("long_float_in_extra", '{"error": "boom", "x": ' + "1" * 5000 + ".5}")
    raw = json.dumps({"error": "boom"})
    body("bom_utf8", b"\xef\xbb\xbf" + raw.encode())
    body("bom_utf8_twice", b"\xef\xbb\xbf\xef\xbb\xbf" + raw.encode())
    body("utf16", raw.encode("utf-16"))
    body("utf16_be", raw.encode("utf-16-be"))
    body("utf16_le", raw.encode("utf-16-le"))
    body("utf16_be_bom", b"\xfe\xff" + raw.encode("utf-16-be"))
    body("utf16_truncated", raw.encode("utf-16-be")[:-1])
    body(
        "utf16_lone_surrogate", raw.encode("utf-16-le").replace("boom".encode("utf-16-le"), b"\x00\xd8o\x00")
    )
    body("utf16_pair", json.dumps({"error": "\U0001f600"}, ensure_ascii=False).encode("utf-16-le"))
    body("utf32", raw.encode("utf-32"))
    body("utf32_be", b"\x00\x00\xfe\xff" + raw.encode("utf-32-be"))
    body("utf32_truncated", raw.encode("utf-32")[:-1])
    body("utf32_out_of_range", raw.encode("utf-32-le").replace("b".encode("utf-32-le"), b"\x00\x00\x11\x00"))
    body("utf32_surrogate", raw.encode("utf-32-le").replace("b".encode("utf-32-le"), b"\x00\xd8\x00\x00"))
    body("utf8_invalid", b'{"error": "\xff"}')
    body("utf8_latin1", b'{"error": "\xe9"}')
    body("utf8_overlong", b'{"error": "\xc0\x80"}')
    body("utf8_too_big", b'{"error": "\xf4\x90\x80\x80"}')
    body("utf8_truncated", b'{"error": "\xe2\x82"}')
    body("utf8_surrogate", b'{"error": "\xed\xa0\x80"}')
    body("utf8_surrogate_pair", b'{"error": "\xed\xa0\xbd\xed\xb8\x80"}')
    body("utf8_surrogate_then_long_int", b'{"error": "\xed\xa0\x80", "retry": ' + b"1" * 4301 + b"}")
    body("utf8_surrogate_then_syntax", b'{"error": "\xed\xa0\x80", "retry": }')
    body("utf8_invalid_in_extra_after_syntax", b'{"error": "boom", "x": } "\xff"')
    body("two_byte_body", b"{}")
    body("two_byte_nul", b"\x00{")
    body("utf16_two_bytes", "1".encode("utf-16-le"))
    for content_type in [
        None,
        "",
        "text/plain",
        "application/json; charset=utf-8",
        "application/vnd.x+json",
        "APPLICATION/JSON",
        "application/jsonx",
        "multipart/form-data",
        " application/json ",
        "application/json;",
        "application/json/x",
        "application",
        "application/+json",
        "application/json ; q=1",
        "json",
        "text/json",
    ]:
        body(f"content_type_{content_type!r}", raw, content_type)
    body("text_plain_invalid_utf8", b'{"error": "\xff"}', "text/plain")
    return cases


def large_completion_cases():
    """Bodies too large to commit; Go rebuilds them from the recipe and checks the SHA-256."""
    cases = []

    def word(name, unit, repeat):
        recipe = {"kind": "word", "unit_json": json.dumps(unit)[1:-1], "repeat": repeat}
        cases.append((name, json.dumps({"result": one_word(text=unit * repeat)}), recipe))

    def words(name, count, length, last):
        items = [
            {"text": "x" * length, "start": i * 100, "end": i * 100 + 50, "confidence": 0.5}
            for i in range(count)
        ]
        items[-1]["text"] = "y" * last
        value = {
            "text": " ".join(w["text"] for w in items),
            "audio_duration_ms": 10_800_000,
            "chunks": 1,
            "seam_fallbacks": 0,
            "words": items,
        }
        recipe = {"kind": "words", "count": count, "length": length, "last": last}
        cases.append((name, json.dumps({"result": value}), recipe))

    word("word_text_4096", "a", 4096)
    word("word_text_4097", "a", 4097)
    word("word_text_4096_emoji", "\U0001f600", 4096)
    word("word_text_4097_emoji", "\U0001f600", 4097)
    word("word_text_4096_accent", "\xe9", 4096)
    words("words_100000", 100_000, 3, 3)
    words("words_100001", 100_001, 3, 3)
    words("text_2000000", 100_000, 19, 20)
    words("text_2000001", 100_000, 19, 21)
    return cases


def run_completion(client, captured, cases):
    out = []
    for name, raw, content_type in cases:
        captured.clear()
        headers = {} if content_type is None else {"content-type": content_type}
        raw_bytes = raw if isinstance(raw, bytes) else raw.encode()
        response = client.post("/internal/jobs/job/complete", content=raw_bytes, headers=headers)
        case = {"name": name, "content_type": content_type, "status": response.status_code}
        if response.status_code == 200:
            result, error, retry = captured[0]
            case["error"], case["retry"] = error, retry
            case["stored"] = None
            if result is not None:
                clamped = dict(result, **{k: min(result[k], INT64_MAX) for k in ("chunks", "seam_fallbacks")})
                case["stored"] = render(clamped)
        else:
            case["body"] = response.text
        out.append((case, raw_bytes))
    return out


def submission_cases():
    cases = []

    def body(name, raw, content_type="application/json"):
        cases.append((name, raw, content_type))

    body("valid", '{"audio_url": "https://example.com/a.wav"}')
    for literal in ['"en"', '"de"', '"EN"', '"xx"', '""', "null", "5", '"en "', "true", "[]"]:
        body(
            f"language_{literal}", '{"audio_url": "https://example.com/a.wav", "language_code": %s}' % literal
        )
    body("url_missing", "{}")
    body("url_null", '{"audio_url": null}')
    body("url_int", '{"audio_url": 5}')
    body("url_8192", json.dumps({"audio_url": "https://example.com/" + "a" * (8192 - 20)}))
    body("url_8193", json.dumps({"audio_url": "https://example.com/" + "a" * (8193 - 20)}))
    body("url_surrogate", '{"audio_url": "https://example.com/\\ud800"}')
    body("url_escaped_slash", '{"audio_url": "https:\\/\\/example.com\\/a"}')
    body("extra", '{"audio_url": "https://example.com/a", "speech_model": "best"}')
    body("top_array", "[]")
    body("empty", "")
    body("text_plain", '{"audio_url": "https://example.com/a"}', "text/plain")
    body("no_content_type", '{"audio_url": "https://example.com/a"}', None)
    body("invalid_utf8", b'{"audio_url": "https://example.com/\xff"}')
    body("long_int", '{"audio_url": "https://example.com/a", "x": ' + "1" * 4301 + "}")
    return cases


def run_submission(client, cases, seen):
    out = []
    for name, raw, content_type in cases:
        seen.clear()
        headers = {} if content_type is None else {"content-type": content_type}
        raw_bytes = raw if isinstance(raw, bytes) else raw.encode()
        response = client.post("/v2/transcript", content=raw_bytes, headers=headers)
        case = {
            "name": name,
            "content_type": content_type,
            "status": response.status_code,
            "body": response.text,
        }
        case.update(seen)
        out.append((case, raw_bytes))
    return out


def requests():
    settings = Settings(
        data=Path(tempfile.mkdtemp()), db_mode="memory", api_key="c" * 40, worker_key="w" * 40
    )
    captured, seen = [], {}
    original_validate, original_split = api.validate_options, api.urlsplit

    def validate(options):
        seen["language_code"] = options.get("language_code")
        return original_validate(options)

    def split(url):
        seen["audio_url"] = url
        raise HTTPException(418, "Reached audio resolution")

    api.validate_options, api.urlsplit = validate, split
    try:
        with TestClient(create_client_app(settings)) as client:
            client.app.state.store.finish = lambda jid, token, result, error, retry: captured.append(
                (result, error, retry)
            )
            client.headers.update({"authorization": settings.worker_key, "x-lease-token": "token"})
            small = run_completion(client, captured, completion_cases())
            recipes = large_completion_cases()
            large = run_completion(client, captured, [(n, raw, "application/json") for n, raw, _ in recipes])
            client.headers.update({"authorization": settings.api_key})
            submissions = run_submission(client, submission_cases(), seen)
    finally:
        api.validate_options, api.urlsplit = original_validate, original_split

    write("completion.json", [with_body(case, raw) for case, raw in small + uvicorn_depth_cases()])
    large_out = []
    for (case, raw), (_, _, recipe) in zip(large, recipes):
        case["recipe"], case["body_sha256"] = recipe, hashlib.sha256(raw).hexdigest()
        if case.get("stored") is not None:
            case["stored_sha256"] = hashlib.sha256(case.pop("stored").encode()).hexdigest()
        large_out.append(case)
    write("completion_large.json", large_out)
    write("submission.json", [with_body(case, raw) for case, raw in submissions])


def uvicorn_depth_cases():
    """json.loads hits RecursionError at a depth that depends on the call stack, so measure it
    under uvicorn, which production ran, rather than under TestClient."""
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
    env = dict(
        os.environ,
        DATA_DIR=tempfile.mkdtemp(),
        API_KEY="c" * 40,
        WORKER_API_KEY="w" * 40,
    )
    command = ["uvicorn", "parakeet_api.api:app", "--host", "127.0.0.1", "--port", str(port)]
    server = subprocess.Popen(command, env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    out = []
    try:
        url = f"http://127.0.0.1:{port}"
        for _ in range(100):
            try:
                urllib.request.urlopen(url + "/health/live", timeout=1)
                break
            except OSError:
                time.sleep(0.1)
        bodies = [
            (f"depth_{n}", '{"x": ' + "[" * (n - 1) + "]" * (n - 1) + "}") for n in (10, 960, 961, 962, 963)
        ]
        bodies += [
            ("depth_5000", '{"x": ' + "[" * 4999 + "]" * 4999 + "}"),
            ("depth_unterminated", '{"x": ' + "[" * 5000),
        ]
        for name, raw in bodies:
            request = urllib.request.Request(
                url + "/internal/jobs/job/complete",
                data=raw.encode(),
                headers={"authorization": "w" * 40, "content-type": "application/json"},
            )
            try:
                with urllib.request.urlopen(request) as response:
                    status, text = response.status, response.read().decode()
            except urllib.error.HTTPError as error:
                status, text = error.code, error.read().decode()
            out.append(
                (
                    {"name": name, "content_type": "application/json", "status": status, "body": text},
                    raw.encode(),
                )
            )
    finally:
        server.terminate()
        server.wait()
    return out


def create_client_app(settings):
    return api.create_app(settings)


def stored(value):
    # The store keeps model_dump() as JSON; responses parse it back.
    return json.loads(json.dumps(Result.model_validate(value).model_dump()))


def words_result(spans, duration=None, texts=None, confidences=None):
    words = []
    for i, (start, end) in enumerate(spans):
        words.append(
            {
                "text": texts[i] if texts else f"w{i}",
                "start": start,
                "end": end,
                "confidence": confidences[i] if confidences else 0.5,
            }
        )
    return {
        "text": " ".join(w["text"] for w in words),
        "audio_duration_ms": duration or (spans[-1][1] if spans else 1),
        "chunks": 1,
        "seam_fallbacks": 0,
        "words": words,
    }


def random_result(rng, count, max_gap, max_len, alphabet):
    spans, texts, confidences, t, last_end = [], [], [], 0, 0
    specials = [0.0, 1.0, 1e-5, 5e-324, 2.5e-7, 0.1, 1 / 3, 0.30000000000000004, 1e-16, 0.9999999999999999]
    for _ in range(count):
        t += rng.randint(0, max_gap)
        length = rng.randint(1, 900)
        last_end = max(last_end, t + length)
        spans.append((t, last_end))
        if rng.random() < 0.3:
            t += rng.randint(0, length)
        else:
            t += length
        texts.append("".join(rng.choice(alphabet) for _ in range(rng.randint(1, max_len))))
        confidences.append(rng.choice(specials) if rng.random() < 0.2 else rng.random())
    duration = max(end for _, end in spans) + rng.randint(0, 5000)
    return words_result(spans, duration, texts, confidences)


def outputs():
    rng = random.Random(20260914)
    results = []

    def add(name, value, **job):
        results.append((name, stored(value), job))

    add("hello", BASE)
    add("empty", {"text": "", "words": [], "audio_duration_ms": 1, "chunks": 1, "seam_fallbacks": 0})
    add(
        "empty_long",
        {"text": "", "words": [], "audio_duration_ms": 10_800_000, "chunks": 3, "seam_fallbacks": 1},
    )
    add("single_ms", words_result([(0, 1)]))
    add(
        "escaping",
        words_result(
            [(0, 100), (100, 200), (200, 300), (300, 400), (400, 500), (500, 600), (600, 700), (700, 800)],
            texts=[
                "a&b",
                "<i>",
                ">",
                "&amp;",
                "-->",
                "line\nbreak",
                '"quote"\\',
                "tab\t\x01\x7f\u2028\u2029",
            ],
        ),
    )
    add(
        "unicode",
        words_result(
            [(0, 50), (60, 90), (95, 99), (100, 5000)],
            texts=["\xe9t\xe9", "\u6f22\u5b57", "\U0001f600\U0001f680", "e\u0301\u0301"],
        ),
    )
    add("gap_1500", words_result([(0, 100), (1600, 1700)]))
    add("gap_1501", words_result([(0, 100), (1601, 1700)]))
    add("span_6000", words_result([(0, 100), (5000, 6000)]))
    add("span_6001", words_result([(0, 100), (5000, 6001)]))
    add("chars_80", words_result([(i * 10, i * 10 + 5) for i in range(20)], texts=["abc"] * 19 + ["abcd"]))
    add("chars_81", words_result([(i * 10, i * 10 + 5) for i in range(20)], texts=["abc"] * 19 + ["abcde"]))
    add(
        "chars_20_emoji",
        words_result([(i * 10, i * 10 + 5) for i in range(12)], texts=["\U0001f600" * 3] * 12),
    )
    add(
        "long_words",
        words_result([(0, 1000), (1000, 2000), (2000, 3000)], texts=["a" * 300, "b", "\U0001f600" * 250]),
    )
    add(
        "long_duration",
        words_result(
            [
                (3_599_999, 3_600_000),
                (3_600_000, 3_600_001),
                (7_199_500, 7_200_500),
                (10_799_999, 10_800_000),
            ],
            duration=10_800_000,
        ),
    )
    add(
        "rounding_ms",
        words_result([(1, 5), (5, 10), (10, 100), (1005, 1015), (12345, 12346), (59999, 60001)]),
    )
    add("confidence_tenths", words_result([(i, i + 1) for i in range(10)], confidences=[0.1] * 10))
    add(
        "confidence_mixed",
        words_result([(i, i + 1) for i in range(7)], confidences=[1e-5, 0.7, 1.0, 0.0, 1 / 3, 5e-324, 0.2]),
    )
    add("confidence_exp", words_result([(0, 1)], confidences=[1e-7]))
    add("confidence_zero", words_result([(0, 1), (1, 2)], confidences=[0.0, -0.0]))
    add("confidence_negzero", words_result([(0, 1)], confidences=[-0.0]))
    add("overlap", words_result([(0, 500), (100, 600), (100, 600), (650, 700)]))
    ascii_letters = "abcdefghijklmnopqrstuvwxyz.,?!'-&<>"
    mixed = ascii_letters + '\xe9\xfc\xdf\u6f22\U0001f600"\\'
    for i in range(6):
        add(f"random_{i}", random_result(rng, rng.choice([5, 50, 300]), 2500, 30, ascii_letters))
    for i in range(3):
        add(f"random_mixed_{i}", random_result(rng, 150, 3000, 12, mixed))
    add("random_big", random_result(rng, 1500, 2500, 15, ascii_letters))

    cases = []
    for name, result, _ in results:
        for status, error, language in [("completed", None, None), ("completed", None, "en")]:
            job = {
                "id": "00000000-0000-4000-8000-000000000001",
                "status": status,
                "error": error,
                "upload_id": "11111111-1111-4111-8111-111111111111",
                "options": json.dumps({"language_code": language} if language else {}),
                "result": json.dumps(result),
            }
            case = job_case(f"{name}_{language}", job, result)
            if language is None:
                case["openai"] = {
                    ",".join(g): render(openai(result, g))
                    for g in (["segment"], ["word"], ["word", "segment"], [])
                }
                case["segments"] = {str(n): render(segments(result, n)) for n in (20, 80, 200)}
                case["srt"] = {str(n): subtitles(result, False, n) for n in (20, 80, 200)}
                case["vtt"] = {str(n): subtitles(result, True, n) for n in (20, 80, 200)}
                case["text_json"] = render({"text": result["text"]})
            cases.append(case)
    for status, error in [
        ("queued", None),
        ("processing", None),
        ("error", "Transcription failed"),
        ("error", 'bad <&> "x" \xe9\U0001f600\n\x01'),
    ]:
        job = {
            "id": "22222222-2222-4222-8222-222222222222",
            "status": status,
            "error": error,
            "upload_id": "33333333-3333-4333-8333-333333333333",
            "options": json.dumps({"language_code": "de"}),
            "result": None,
        }
        cases.append(job_case(f"{status}_{error}", job, None))
    write("outputs.json.gz", cases)


def job_case(name, job, result):
    public = "https://api.example.com/base"
    body = assembly(job, public)
    return {
        "name": name,
        "job": {
            "id": job["id"],
            "status": job["status"],
            "error": job["error"],
            "upload_id": job["upload_id"],
            "language_code": json.loads(job["options"]).get("language_code"),
            "result": render(result) if result is not None else None,
        },
        "public_url": public,
        "assembly": render(body),
        "deleted": render({**body, "status": "completed", "text": None, "words": None}),
    }


def encoding():
    rng = random.Random(7)
    values = [
        0.0,
        -0.0,
        1.0,
        0.5,
        0.1,
        1 / 3,
        2 / 3,
        1e-4,
        1e-5,
        1.5e-5,
        0.00012345,
        1e15,
        1e16,
        1.5e16,
        123456789012345.6,
        1234567890123456.7,
        12345678901234567.0,
        9007199254740993.0,
        5e-324,
        2.2250738585072014e-308,
        1.7976931348623157e308,
        0.30000000000000004,
        100.0,
        1e21,
        1e22,
        123e18,
        0.001,
        10.8,
        10800.0,
        3.6,
        -1.5,
        1e-7,
        4.35,
        2.675,
    ]
    values += [rng.random() for _ in range(40)]
    values += [rng.uniform(0, 11000) for _ in range(20)]
    values += [rng.randint(0, 10_800_000) / 1000 for _ in range(40)]
    values += [2.0**e for e in range(-60, 70, 7)]
    floats = [{"hex": v.hex(), "json": render([v])[1:-1]} for v in values]
    strings = [
        "",
        "plain",
        '"quoted" \\ back/slash',
        "\b\f\n\r\t",
        "".join(chr(i) for i in range(32)),
        "\x7f\x80",
        "<script>&amp;</script>",
        "\u2028\u2029",
        "\xe9\u6f22\U0001f600",
        "\ufeff\ufffd",
        "\x00",
    ]
    stamps = [
        0.0,
        0.0005,
        0.0015,
        0.0025,
        0.0035,
        1.0005,
        2.5e-4,
        0.001,
        0.1,
        59.9995,
        59.9994999,
        3599.9995,
        3600.0,
        10800.0,
        10799.9995,
        1.2345,
        123.4565,
        0.49999999999999994e-3,
        0.0004999,
        86399.9995,
        360000.0005,
        0.3,
        0.7,
        1.1,
        2.2,
        3.3,
        1e-9,
    ]
    stamps += [rng.randint(0, 10_800_000) / 1000 for _ in range(60)]
    stamps += [rng.uniform(0, 11000) for _ in range(40)]
    stamps += [(rng.randint(0, 10_800_000) + 0.5) / 1000 for _ in range(40)]
    write(
        "encoding.json",
        {
            "floats": floats,
            "strings": [{"value": s, "json": render([s])[1:-1]} for s in strings],
            "timestamps": [
                {"hex": v.hex(), "srt": timestamp(v, False), "vtt": timestamp(v, True)} for v in stamps
            ],
            "languages": [
                {"code": code, "valid": valid_language(code)}
                for code in "bg hr cs da nl en et fi fr de el hu it lv lt mt pl pt ro ru sk sl es sv uk".split()
                + ["EN", "en ", "", "xx", "zh", "ja", "en-US", "english", "eng", "no", "nb", "ca"]
            ],
        },
    )


def valid_language(code):
    try:
        validate_options({"language_code": code})
        return True
    except ValueError:
        return False


if __name__ == "__main__":
    requests()
    outputs()
    encoding()
