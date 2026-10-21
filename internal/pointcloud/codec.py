"""Proposed point-cloud binary reference codec: stdlib only, no model/filesystem startup.

Streams must be caller-owned and frozen while reading. Syntax/hash validation
does not authenticate metadata, grant Files authority or prove process exit.
"""

import hashlib
import math
import struct

MAGIC = b"CITPCD1\0"
HEADER = struct.Struct("<8sHHIQII")
POINT = struct.Struct("<eeeBBB")
MAX_FRAMES = 1800
MAX_FRAME_POINTS = 150000
MAX_BINARY_BYTES = 2 << 30
ERROR = "point-cloud sequence invalid"


def _invalid():
    raise ValueError(ERROR) from None


def _fps_valid(numerator, denominator):
    return (
        type(numerator) is int and type(denominator) is int
        and 1 <= denominator <= numerator <= 0xFFFFFFFF
        and numerator <= 30 * denominator and math.gcd(numerator, denominator) == 1
    )


def _coordinate(value):
    # Normalize to binary32 before binary16, matching Go's float32 Point API
    # and avoiding language-dependent double rounding at half-way boundaries.
    try:
        if type(value) not in (int, float) or not math.isfinite(value):
            _invalid()
        result = struct.unpack("<f", struct.pack("<f", value))[0]
    except (ValueError, TypeError, OverflowError, struct.error):
        raise ValueError(ERROR) from None
    if not math.isfinite(result) or abs(result) > 65504:
        _invalid()
    return result


def _read_at(stream, count, offset):
    try:
        if stream.seek(offset) != offset:
            _invalid()
        data = stream.read(count)
        if not isinstance(data, bytes) or len(data) != count:
            _invalid()
        return data
    except (OSError, ValueError, TypeError, AttributeError):
        raise ValueError(ERROR) from None


class Reader:
    """Bounded random frame reads; the stream is not shared/concurrent."""

    def __init__(self, stream, size):
        if type(size) is not int or not HEADER.size <= size <= MAX_BINARY_BYTES:
            _invalid()
        magic, version, header_size, frames, total, n, d = HEADER.unpack(_read_at(stream, HEADER.size, 0))
        if magic != MAGIC or version != 1 or header_size != HEADER.size:
            _invalid()
        if not 1 <= frames <= MAX_FRAMES or not _fps_valid(n, d):
            _invalid()
        start = HEADER.size + 8 * (frames + 1)
        if start > size or total > (size - start) // POINT.size or start + POINT.size * total != size:
            _invalid()
        offsets = struct.unpack("<" + "Q" * (frames + 1), _read_at(stream, 8 * (frames + 1), HEADER.size))
        if offsets[0] != 0 or offsets[-1] != total:
            _invalid()
        if any(b < a or b > total or b - a > MAX_FRAME_POINTS for a, b in zip(offsets, offsets[1:])):
            _invalid()
        self._stream, self._start, self._offsets = stream, start, offsets
        self._header = {"frame_count": frames, "total_points": total, "fps_numerator": n, "fps_denominator": d}

    @property
    def header(self):
        return dict(self._header)

    def frame_counts(self):
        return [b - a for a, b in zip(self._offsets, self._offsets[1:])]

    def frame(self, index):
        if type(index) is not int or not 0 <= index < self._header["frame_count"]:
            _invalid()
        count = self._offsets[index + 1] - self._offsets[index]
        data = _read_at(self._stream, count * POINT.size, self._start + self._offsets[index] * POINT.size)
        points = list(POINT.iter_unpack(data))
        if any(not math.isfinite(value) for point in points for value in point[:3]):
            _invalid()
        return points


def verify_sha256(stream, size, expected):
    if type(size) is not int or not HEADER.size <= size <= MAX_BINARY_BYTES:
        _invalid()
    if not isinstance(expected, str) or len(expected) != 64 or any(c not in "0123456789abcdef" for c in expected):
        _invalid()
    digest = hashlib.sha256()
    for offset in range(0, size, 64 << 10):
        digest.update(_read_at(stream, min(64 << 10, size - offset), offset))
    if digest.hexdigest() != expected:
        _invalid()


class Writer:
    """Append one bounded frame to an empty owned stream; finish patches the index.

    The caller must sync/hash and create-only publish the completed artifact.
    A failed/incomplete Writer is permanently poisoned and cannot publish.
    """

    def __init__(self, stream, frames, fps_numerator, fps_denominator):
        if type(frames) is not int or not 1 <= frames <= MAX_FRAMES or not _fps_valid(fps_numerator, fps_denominator):
            _invalid()
        self._stream, self._frames, self._fps = stream, frames, (fps_numerator, fps_denominator)
        self._offsets, self._bounds = [0], None
        self._size, self._failed, self._done = HEADER.size + 8 * (frames + 1), False, False
        try:
            if stream.seek(0, 2) != 0 or stream.seek(0) != 0:
                _invalid()
        except (OSError, ValueError, TypeError, AttributeError):
            self._fail()
        self._write(bytes(self._size))

    def _fail(self):
        self._failed = True
        _invalid()

    def _write(self, data):
        try:
            if self._stream.write(data) != len(data):
                self._fail()
        except (OSError, ValueError, TypeError, AttributeError):
            self._fail()

    def append_frame(self, points):
        if not isinstance(points, (list, tuple)):
            self._fail()
        if self._failed or self._done or len(self._offsets) > self._frames or len(points) > MAX_FRAME_POINTS:
            self._fail()
        if self._size + POINT.size * len(points) > MAX_BINARY_BYTES:
            self._fail()
        data = bytearray()
        try:
            for point in points:
                if len(point) != 6 or any(type(c) is not int or not 0 <= c <= 255 for c in point[3:]):
                    self._fail()
                data.extend(POINT.pack(*(_coordinate(v) for v in point[:3]), *point[3:]))
        except (ValueError, TypeError, OverflowError, struct.error):
            self._fail()
        self._write(data)
        for point in POINT.iter_unpack(data):
            xyz = point[:3]
            if self._bounds is None:
                self._bounds = [list(xyz), list(xyz)]
            self._bounds = [[min(a, b) for a, b in zip(self._bounds[0], xyz)],
                            [max(a, b) for a, b in zip(self._bounds[1], xyz)]]
        self._size += len(data)
        self._offsets.append(self._offsets[-1] + len(points))

    def finish(self):
        if self._failed or self._done or len(self._offsets) != self._frames + 1:
            self._fail()
        self._done = True
        header = HEADER.pack(MAGIC, 1, HEADER.size, self._frames, self._offsets[-1], *self._fps)
        header += struct.pack("<" + "Q" * len(self._offsets), *self._offsets)
        try:
            if self._stream.seek(0) != 0:
                self._fail()
            self._write(header)
            if self._stream.seek(self._size) != self._size:
                self._fail()
        except (OSError, ValueError, TypeError, AttributeError):
            self._fail()
        return {
            "frame_count": self._frames, "total_points": self._offsets[-1],
            "fps_numerator": self._fps[0], "fps_denominator": self._fps[1],
            "size": self._size, "frame_counts": [b - a for a, b in zip(self._offsets, self._offsets[1:])],
            "bounds": None if self._bounds is None else [list(v) for v in self._bounds],
        }


def project(width, height, distance, rgb, mask, fov, near, far, point_budget):
    """Synthetic/reference projection of already converted camera distance."""
    if type(width) is not int or type(height) is not int or not 1 <= width <= 4096 or not 1 <= height <= 4096:
        _invalid()
    pixels = width * height
    if not isinstance(distance, (list, tuple)) or not isinstance(rgb, bytes):
        _invalid()
    if pixels > 16777216 or len(distance) != pixels or len(rgb) != 3 * pixels:
        _invalid()
    if mask is not None and (
        not isinstance(mask, (list, tuple))
        or len(mask) != pixels
        or any(type(v) is not bool for v in mask)
    ):
        _invalid()
    if any(type(v) not in (float, int) or abs(v) > 65504 or not math.isfinite(v) for v in (fov, near, far)):
        _invalid()
    if not 0 < fov < 180 or not 0 < near < far <= 65504 or type(point_budget) is not int or not 1 <= point_budget <= MAX_FRAME_POINTS:
        _invalid()
    if any(type(v) not in (int, float) for v in distance):
        _invalid()

    def eligible(index):
        value = sample(index)
        return (mask is None or mask[index]) and math.isfinite(value) and value > 0

    def sample(index):
        try:
            return struct.unpack("<f", struct.pack("<f", distance[index]))[0]
        except OverflowError:
            return math.inf if distance[index] > 0 else -math.inf

    count = sum(eligible(i) for i in range(pixels))
    want = min(count, point_budget)
    if not want:
        return []

    def target(selected):
        return (count - 1) // 2 if want == 1 else selected * (count - 1) // (want - 1)

    tangent = math.tan(fov * math.pi / 360)
    if not math.isfinite(tangent) or tangent <= 0:
        _invalid()
    focal = width / (2 * tangent)
    if not math.isfinite(focal) or focal <= 0:
        _invalid()
    result, seen, next_sample = [], 0, target(0)
    for index, value in enumerate(distance):
        if not eligible(index):
            continue
        if seen == next_sample:
            depth = max(near, min(sample(index), far))
            x = (index % width + 0.5 - width / 2) * depth / focal
            y = -(index // width + 0.5 - height / 2) * depth / focal
            result.append((_coordinate(x), _coordinate(y), _coordinate(-depth), *rgb[3 * index:3 * index + 3]))
            if len(result) == want:
                break
            next_sample = target(len(result))
        seen += 1
    return result
