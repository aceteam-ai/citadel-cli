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


FIXTURE_SHA256 = "e386064cb2c371cadc8bdc4eb6475adf78510a14fa9ae9aac450bcd830c185e0"
FIXTURE = Path(__file__).parent / "testdata" / "media_input_v1.json"


def _object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate key")
        result[key] = value
    return result


def _invalid_number(_):
    raise ValueError("non-integer number")


def _unicode(value):
    if isinstance(value, str):
        if any(0xD800 <= ord(c) <= 0xDFFF for c in value):
            raise ValueError("surrogate")
    elif isinstance(value, list):
        for item in value:
            _unicode(item)
    elif isinstance(value, dict):
        for key, item in value.items():
            _unicode(key)
            _unicode(item)


def validate(task, raw):
    if task not in ("image-to-image", "inpaint") or not 0 < len(raw) <= 10240:
        raise ValueError("invalid contract")
    value = json.loads(
        raw.decode("utf-8", errors="strict"),
        object_pairs_hook=_object,
        parse_float=_invalid_number,
        parse_constant=_invalid_number,
    )
    _unicode(value)
    if not isinstance(value, dict) or set(value) != {"schema", "inputs"}:
        raise ValueError("root keys")
    if value["schema"] != "media_input.v1" or not isinstance(value["inputs"], list):
        raise ValueError("schema")
    roles = ["source", "mask"] if task == "inpaint" else ["source"]
    if len(value["inputs"]) != len(roles):
        raise ValueError("cardinality")
    for role, item in zip(roles, value["inputs"]):
        if not isinstance(item, dict):
            raise ValueError("descriptor")
        origin = item.get("origin")
        if origin not in ("inbox", "workspace"):
            raise ValueError("origin")
        location = "capability" if origin == "inbox" else "path"
        if set(item) != {"role", "origin", "sha256", "size", "format", location}:
            raise ValueError("descriptor keys")
        if any(type(item[key]) is not str for key in ("role", "origin", "sha256", "format", location)):
            raise ValueError("string type")
        if item["role"] != role:
            raise ValueError("role ordering")
        if type(item["size"]) is not int or not 1 <= item["size"] <= 16777216:
            raise ValueError("size")
        if re.fullmatch(r"[0-9a-f]{64}", item["sha256"]) is None:
            raise ValueError("sha256")
        formats = ("png",) if role == "mask" else ("png", "jpeg", "webp")
        if item["format"] not in formats:
            raise ValueError("format")
        if origin == "workspace":
            path = item["path"]
            if (
                not path
                or path == "."
                or path.startswith("/")
                or ".." in path.split("/")
                or any(c in path for c in ("\\", ":", "\x00"))
                or any(unicodedata.category(c) == "Cc" for c in path)
                or PurePosixPath(path).as_posix() != path
                or unicodedata.normalize("NFC", path) != path
            ):
                raise ValueError("path")
        else:
            token = item["capability"]
            if (
                token in ("", ".", "..")
                or len(token) > 4096
                or re.fullmatch(r"[A-Za-z0-9_.-]+", token) is None
            ):
                raise ValueError("capability")
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
                with self.assertRaises(ValueError):
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


if __name__ == "__main__":
    unittest.main()
