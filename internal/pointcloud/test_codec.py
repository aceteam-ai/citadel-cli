"""Synthetic, stdlib-only wire/projection tests. Never imports a model or service."""

import hashlib
import io
import json
import math
from pathlib import Path
import struct
import unittest

import codec

FIXTURE = Path(__file__).parent / "testdata" / "sequence-v1.json"
FIXTURE_SHA256 = "8cad831a3f4da7e9f349d45156ade8499c1ee929e20d26652d9a5f86bf228190"


class SequenceTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        raw = FIXTURE.read_bytes()
        if hashlib.sha256(raw).hexdigest() != FIXTURE_SHA256:
            raise AssertionError("shared synthetic fixture changed without a pin review")
        cls.fixture = json.loads(raw)
        if cls.fixture["fixture_schema"] != "citadel.pointcloud-codec-tests.v1" or cls.fixture["synthetic"] is not True:
            raise AssertionError("not a synthetic fixture")
        cls.binary = bytes.fromhex(cls.fixture["binary_hex"])
        if len(cls.binary) != cls.fixture["size"] or hashlib.sha256(cls.binary).hexdigest() != cls.fixture["sha256"]:
            raise AssertionError("fixture artifact length/hash differs")

    def assert_invalid(self, operation):
        with self.assertRaisesRegex(ValueError, "^" + codec.ERROR + "$"):
            operation()

    def test_shared_fixture_exact_bytes_roundtrip_and_quantized_bounds(self):
        f = self.fixture
        stream = io.BytesIO()
        writer = codec.Writer(stream, len(f["input_frames"]), f["fps"]["numerator"], f["fps"]["denominator"])
        for frame in f["input_frames"]:
            writer.append_frame(frame)
        summary = writer.finish()
        self.assertEqual(stream.getvalue(), self.binary)
        self.assertEqual(summary["bounds"], [f["bounds"]["min"], f["bounds"]["max"]])
        self.assertEqual(summary["frame_counts"], f["frame_counts"])
        codec.verify_sha256(stream, f["size"], f["sha256"])
        reader = codec.Reader(stream, f["size"])
        self.assertEqual(reader.frame_counts(), f["frame_counts"])
        for index, expected in enumerate(f["decoded_frames"]):
            self.assertEqual([list(point) for point in reader.frame(index)], expected)
        counts = reader.frame_counts()
        counts[0] = 999
        self.assertEqual(reader.frame_counts()[0], 1)
        self.assert_invalid(lambda: reader.frame(-1))
        self.assert_invalid(lambda: reader.frame(3))

    def test_header_offsets_lengths_and_nonfinite_records(self):
        mutations = [(0, b"X"), (8, struct.pack("<H", 2)), (10, b"\0\0"),
                     (12, struct.pack("<I", 0)), (12, struct.pack("<I", codec.MAX_FRAMES + 1)),
                     (16, struct.pack("<Q", 0xFFFFFFFFFFFFFFFF)), (24, struct.pack("<I", 0)),
                     (24, struct.pack("<I", 31)), (28, struct.pack("<I", 0)),
                     (32, struct.pack("<Q", 1)), (48, struct.pack("<Q", 0)), (56, struct.pack("<Q", 2))]
        for offset, patch in mutations:
            with self.subTest(offset=offset, patch=patch.hex()):
                raw = bytearray(self.binary)
                raw[offset:offset + len(patch)] = patch
                self.assert_invalid(lambda: codec.Reader(io.BytesIO(raw), len(raw)))
        for raw in (self.binary[:-1], self.binary + b"\0"):
            self.assert_invalid(lambda: codec.Reader(io.BytesIO(raw), len(raw)))
        self.assert_invalid(lambda: codec.Reader(io.BytesIO(self.binary), codec.MAX_BINARY_BYTES + 1))
        self.assert_invalid(lambda: codec.verify_sha256(io.BytesIO(self.binary), len(self.binary), "0" * 64))
        for bits in (0x7C00, 0xFC00, 0x7E00):
            raw = bytearray(self.binary)
            raw[64:66] = struct.pack("<H", bits)
            reader = codec.Reader(io.BytesIO(raw), len(raw))
            self.assert_invalid(lambda: reader.frame(0))

    def test_writer_is_empty_only_and_failures_are_sticky(self):
        self.assert_invalid(lambda: codec.Writer(io.BytesIO(b"owned existing bytes"), 1, 15, 1))
        for n, d in ((0, 1), (1, 0), (31, 1), (30, 2), (True, 1)):
            self.assert_invalid(lambda: codec.Writer(io.BytesIO(), 1, n, d))
        for point in ((math.nan, 0, -1, 0, 0, 0), (0, math.inf, -1, 0, 0, 0),
                      (65505, 0, -1, 0, 0, 0), (0, 0, -1, 256, 0, 0), (0, 0, -1, True, 0, 0)):
            writer = codec.Writer(io.BytesIO(), 1, 15, 1)
            self.assert_invalid(lambda: writer.append_frame([point]))
            self.assert_invalid(writer.finish)
        writer = codec.Writer(io.BytesIO(), 2, 15, 1)
        writer.append_frame([])
        self.assert_invalid(writer.finish)
        self.assert_invalid(lambda: writer.append_frame([]))

    def test_short_write_is_not_a_publishable_sequence(self):
        class ShortStream(io.BytesIO):
            def __init__(self, fail_at):
                super().__init__()
                self.calls, self.fail_at = 0, fail_at

            def write(self, data):
                self.calls += 1
                return 0 if self.calls == self.fail_at else super().write(data)

        for fail_at in (1, 2, 3):
            def attempt():
                writer = codec.Writer(ShortStream(fail_at), 1, 15, 1)
                writer.append_frame([(0, 0, -1, 1, 2, 3)])
                writer.finish()
            self.assert_invalid(attempt)

    def test_binary32_normalization_precedes_binary16_midpoint_rounding(self):
        # Each binary64 input is just off a binary16 midpoint but rounds to
        # that exact midpoint in binary32. Direct binary64-to-binary16
        # conversion instead produces the different, odd record pinned here.
        cases = (
            (1 + 2**-11 + 2**-25, 0x3C00, 0x3C01, 1),
            (1 + 3 * 2**-11 - 2**-25, 0x3C02, 0x3C01, 1 + 2**-9),
            (-(1 + 2**-11 + 2**-25), 0xBC00, 0xBC01, -1),
            (-(1 + 3 * 2**-11 - 2**-25), 0xBC02, 0xBC01, -(1 + 2**-9)),
        )
        for value, expected_bits, direct_bits, decoded in cases:
            with self.subTest(expected_bits=expected_bits):
                self.assertEqual(struct.pack("<e", value), struct.pack("<H", direct_bits))
                stream = io.BytesIO()
                writer = codec.Writer(stream, 1, 15, 1)
                writer.append_frame([(value, 0, 0, 1, 2, 3)])
                summary = writer.finish()
                expected_record = struct.pack("<H", expected_bits) + bytes(4) + bytes((1, 2, 3))
                self.assertEqual(stream.getvalue()[codec.HEADER.size + 16:], expected_record)
                self.assertEqual(summary["bounds"], [[decoded, 0, 0], [decoded, 0, 0]])
                self.assertEqual(codec.Reader(stream, summary["size"]).frame(0),
                                 [(decoded, 0, 0, 1, 2, 3)])

    def test_per_frame_cap_and_empty_rational_fps_sequence(self):
        count = codec.MAX_FRAME_POINTS + 1
        raw = codec.HEADER.pack(codec.MAGIC, 1, codec.HEADER.size, 2, count, 15, 1)
        raw += struct.pack("<QQQ", 0, count, count) + bytes(codec.POINT.size * count)
        self.assert_invalid(lambda: codec.Reader(io.BytesIO(raw), len(raw)))
        stream = io.BytesIO()
        writer = codec.Writer(stream, 2, 30000, 1001)
        writer.append_frame([])
        writer.append_frame([])
        summary = writer.finish()
        self.assertIsNone(summary["bounds"])
        self.assertEqual(summary["total_points"], 0)
        self.assertEqual(codec.Reader(stream, summary["size"]).frame(1), [])

    def test_projection_mask_clamp_axes_and_even_sampling(self):
        distance = [1, 2, 3, math.nan]
        rgb = bytes([255, 0, 0, 0, 255, 0, 0, 0, 255, 255, 255, 255])
        options = (90, 0.5, 2, 2)
        self.assertEqual(codec.project(2, 2, distance, rgb, None, *options),
                         [(-0.5, 0.5, -1, 255, 0, 0), (-1, -1, -2, 0, 0, 255)])
        self.assertEqual(codec.project(2, 2, distance, rgb, [False, True, False, False], *options),
                         [(1, 1, -2, 0, 255, 0)])
        self.assertEqual(codec.project(2, 2, distance, rgb, None, 90, 0.5, 2, 1), [(1, 1, -2, 0, 255, 0)])
        self.assertEqual(codec.project(2, 2, [0, -1, math.inf, math.nan], rgb, None, *options), [])
        self.assert_invalid(lambda: codec.project(1, 1, [1], b"rgb", [], 60, 0.1, 100, 1))
        self.assert_invalid(lambda: codec.project(4097, 1, [1], b"rgb", None, 60, 0.1, 100, 1))
        self.assert_invalid(lambda: codec.project(1, 1, [1], b"rgb", None, math.nan, 0.1, 100, 1))
        self.assert_invalid(lambda: codec.project(1, 1, [1], b"rgb", None, 60, 0.1, 100, 0))

    def test_projection_mask_type_and_shape_have_static_errors(self):
        for mask in (0, False, object(), {}, {0: True}, b"\x01", "x", [1], [None], []):
            with self.subTest(mask_type=type(mask).__name__):
                self.assert_invalid(lambda: codec.project(1, 1, [1], b"rgb", mask, 60, 0.1, 100, 1))
        for mask in ([True], (True,)):
            self.assertEqual(codec.project(1, 1, [1], b"rgb", mask, 60, 0.1, 100, 1),
                             [(0, 0, -1, 114, 103, 98)])
        for mask in ([False], (False,)):
            self.assertEqual(codec.project(1, 1, [1], b"rgb", mask, 60, 0.1, 100, 1), [])


if __name__ == "__main__":
    unittest.main()
