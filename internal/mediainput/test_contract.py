"""Offline, stdlib-only replay of media_input.v1 input contract vectors.

This is a fixture-consumer reference, not the platform's production parser.
It imports no sidecar, image codec, model, network or ML package.
Run: python3 -m unittest discover -s internal/mediainput -p 'test_contract.py'
"""

import hashlib
import json
from pathlib import Path, PurePosixPath
import re
import unicodedata
import unittest


FIXTURE_SHA256 = "6d19921e8d842ecd00dfba709ad073eacc0511dce7f12e9c2107b0e0f8fcca1e"
FIXTURE = Path(__file__).parent / "testdata" / "media_input_v1.json"


def _object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate key")
        result[key] = value
    return result


def _invalid_number(_):
    raise ValueError("invalid_schema")


class _NonInteger(str):
    """Preserve a JSON float's lexical kind without coercing it to an integer."""


def _shape(value, depth=0, budget=None):
    if budget is None:
        budget = [128]
    budget[0] -= 1
    if depth >= 4 or budget[0] < 0:
        raise ValueError("invalid_schema")
    if isinstance(value, dict):
        if len(value) > 8:
            raise ValueError("invalid_schema")
        for item in value.values():
            _shape(item, depth + 1, budget)
    elif isinstance(value, list):
        if len(value) > 2:
            raise ValueError("invalid_schema")
        for item in value:
            _shape(item, depth + 1, budget)


def valid_path(name):
    """Slash-only lexical rule shared with Go, never file-opening authority."""
    if not isinstance(name, str) or name in ("", ".") or name.startswith("/"):
        return False
    if any(c in name for c in ("\\", ":", "\x00", "\u00a0", "\uff0e")):
        return False
    if any(unicodedata.category(c) in ("Cc", "Cf", "Zl", "Zp") for c in name):
        return False
    if PurePosixPath(name).as_posix() != name or unicodedata.normalize("NFC", name) != name:
        return False
    try:
        return all(component != ".." and len(component.encode("utf-8")) <= 255 for component in name.split("/"))
    except UnicodeError:
        return False


def _unicode(value):
    if isinstance(value, str):
        if any(0xD800 <= ord(c) <= 0xDFFF for c in value):
            raise ValueError("invalid_schema")
    elif isinstance(value, list):
        for item in value:
            _unicode(item)
    elif isinstance(value, dict):
        for key, item in value.items():
            _unicode(key)
            _unicode(item)


def validate(task, raw):
    if task not in ("image-to-image", "inpaint"):
        raise ValueError("unsupported_task")
    if not 0 < len(raw) <= 10240:
        raise ValueError("invalid_schema")
    try:
        value = json.loads(
            raw.decode("utf-8", errors="strict"), object_pairs_hook=_object,
            parse_float=_NonInteger, parse_constant=_invalid_number,
        )
    except (ValueError, UnicodeError, RecursionError):
        raise ValueError("invalid_schema") from None
    _shape(value)
    _unicode(value)
    if not isinstance(value, dict) or set(value) != {"schema", "inputs"}:
        raise ValueError("invalid_schema")
    if value["schema"] != "media_input.v1":
        raise ValueError("invalid_schema")
    if not isinstance(value["inputs"], list):
        raise ValueError("invalid_inputs")
    roles = ["source", "mask"] if task == "inpaint" else ["source"]
    if len(value["inputs"]) != len(roles):
        raise ValueError("invalid_roles")
    for role, item in zip(roles, value["inputs"]):
        if not isinstance(item, dict):
            raise ValueError("invalid_schema")
        origin = item.get("origin")
        location = "capability" if origin == "inbox" else "path"
        if set(item) != {"role", "origin", "sha256", "size", "format", location}:
            raise ValueError("invalid_schema")
        if any(type(item[key]) is not str for key in ("role", "origin", "sha256", "format", location)):
            raise ValueError("invalid_schema")
        if item["role"] != role:
            raise ValueError("invalid_roles")
        if isinstance(item["size"], _NonInteger):
            raise ValueError("invalid_size")
        if type(item["size"]) is not int:
            raise ValueError("invalid_schema")
        if not 1 <= item["size"] <= 16777216:
            raise ValueError("invalid_size")
        if re.fullmatch(r"[0-9a-f]{64}", item["sha256"]) is None:
            raise ValueError("invalid_sha256")
        formats = ("png",) if role == "mask" else ("png", "jpeg", "webp")
        if item["format"] not in formats:
            raise ValueError("invalid_format")
        if origin == "workspace":
            if not valid_path(item["path"]):
                raise ValueError("invalid_input_path")
        elif origin == "inbox":
            token = item["capability"]
            if (
                token in ("", ".", "..")
                or len(token) > 4096
                or re.fullmatch(r"[A-Za-z0-9_.-]+", token) is None
            ):
                raise ValueError("invalid_capability")
        else:
            raise ValueError("invalid_origin")
    return value


class ContractVectors(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        raw = FIXTURE.read_bytes()
        if hashlib.sha256(raw).hexdigest() != FIXTURE_SHA256:
            raise AssertionError("shared fixture changed without review")
        cls.fixture = json.loads(raw)
        if cls.fixture["fixture_schema"] != "media_input.contract-tests.v1" or cls.fixture["synthetic"] is not True:
            raise AssertionError("not a synthetic contract fixture")
        cls.by_name = {v["name"]: v for v in cls.fixture["valid"]}

    def test_shared_valid_vectors(self):
        for vector in self.fixture["valid"]:
            with self.subTest(vector["name"]):
                raw = json.dumps(vector["value"], separators=(",", ":"), ensure_ascii=False).encode()
                self.assertEqual(validate(vector["task"], raw), vector["value"])

    def test_shared_negative_vectors(self):
        for vector in self.fixture["invalid"]:
            with self.subTest(vector["name"]):
                base = self.by_name[vector["base"]]
                raw = json.dumps(vector.get("value", base["value"]), separators=(",", ":"), ensure_ascii=False)
                if "replace_first" in vector:
                    old, new = vector["replace_first"]
                    self.assertIn(old, raw, "ineffective fixture mutation")
                    self.assertNotEqual(old, new)
                    raw = raw.replace(old, new, 1)
                with self.assertRaisesRegex(ValueError, "^" + re.escape(vector["reason"]) + "$"):
                    validate(vector.get("task", base["task"]), raw.encode())

    def test_reversed_roles_refused(self):
        value = self.by_name["inpaint"]["value"]
        reversed_value = {**value, "inputs": list(reversed(value["inputs"]))}
        with self.assertRaises(ValueError):
            validate("inpaint", json.dumps(reversed_value).encode())

    def test_no_raw_bytes_or_urls_in_descriptors(self):
        for vector in self.fixture["valid"]:
            for item in vector["value"]["inputs"]:
                self.assertNotIn("url", item)
                self.assertNotIn("b64", item)
                self.assertNotIn("content", item)

    def test_exactly_four_json_levels(self):
        _shape([[["leaf"]]])
        with self.assertRaisesRegex(ValueError, "^invalid_schema$"):
            _shape([[[["leaf"]]]])

    def test_standalone_path_rule_is_portable_and_byte_bounded(self):
        for name in ("NUL", "con.png", "dir/ordinary file.png", "a" * 255, "é" * 127 + "a"):
            with self.subTest(name=name):
                self.assertTrue(valid_path(name))
        for name in ("", ".", "..", "../a", "/a", "a//b", "a/./b", "a/../b", "a/", "a\\b", "C:/a", "a\x00b", "a\u00a0b", "a\uff0eb", "a\u200bb", "a\u202eb", "a\u2028b", "a\u2029b", "a\ufeffb", "cafe\u0301.png", "\ud800", "a" * 256, "é" * 128):
            with self.subTest(name=name):
                self.assertFalse(valid_path(name))


if __name__ == "__main__":
    unittest.main()
