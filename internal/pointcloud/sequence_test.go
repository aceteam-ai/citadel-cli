package pointcloud

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"reflect"
	"testing"
)

const fixtureHash = "8cad831a3f4da7e9f349d45156ade8499c1ee929e20d26652d9a5f86bf228190"

type fixture struct {
	FixtureSchema string        `json:"fixture_schema"`
	Synthetic     bool          `json:"synthetic"`
	FPS           FPS           `json:"fps"`
	InputFrames   [][][]float64 `json:"input_frames"`
	DecodedFrames [][][]float64 `json:"decoded_frames"`
	FrameCounts   []uint32      `json:"frame_counts"`
	Bounds        Bounds        `json:"bounds"`
	BinaryHex     string        `json:"binary_hex"`
	Size          int64         `json:"size"`
	SHA256        string        `json:"sha256"`
}

func loadFixture(t testing.TB) (fixture, []byte) {
	t.Helper()
	raw, err := os.ReadFile("testdata/sequence-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(raw)
	if hex.EncodeToString(hash[:]) != fixtureHash {
		t.Fatal("shared synthetic fixture changed without a pin review")
	}
	var f fixture
	if err := json.Unmarshal(raw, &f); err != nil || !f.Synthetic || f.FixtureSchema != "citadel.pointcloud-codec-tests.v1" {
		t.Fatal("invalid synthetic fixture")
	}
	data, err := hex.DecodeString(f.BinaryHex)
	if err != nil || int64(len(data)) != f.Size {
		t.Fatal("invalid fixture binary")
	}
	hash = sha256.Sum256(data)
	if hex.EncodeToString(hash[:]) != f.SHA256 {
		t.Fatal("fixture artifact hash mismatch")
	}
	return f, data
}

func fixturePoints(t testing.TB, rows [][]float64) []Point {
	t.Helper()
	points := make([]Point, len(rows))
	for i, row := range rows {
		if len(row) != 6 {
			t.Fatal("invalid fixture point")
		}
		points[i] = Point{float32(row[0]), float32(row[1]), float32(row[2]), uint8(row[3]), uint8(row[4]), uint8(row[5])}
	}
	return points
}

func ownedFile(t *testing.T) *os.File {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "sequence-*.bin")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

func TestSharedSequenceWriterAndReader(t *testing.T) {
	f, expected := loadFixture(t)
	file := ownedFile(t)
	writer, err := NewWriter(file, uint32(len(f.InputFrames)), f.FPS)
	if err != nil {
		t.Fatal(err)
	}
	for _, frame := range f.InputFrames {
		if err := writer.AppendFrame(fixturePoints(t, frame)); err != nil {
			t.Fatal(err)
		}
	}
	summary, err := writer.Finish()
	if err != nil || summary.Size != f.Size || summary.TotalPoints != 3 ||
		!reflect.DeepEqual(summary.FrameCounts, f.FrameCounts) || summary.Bounds == nil || *summary.Bounds != f.Bounds {
		t.Fatalf("incorrect quantized summary: %v", err)
	}
	actual, err := os.ReadFile(file.Name())
	if err != nil || !bytes.Equal(actual, expected) {
		t.Fatal("writer differs from the independent binary fixture")
	}
	if err := VerifySHA256(file, f.Size, f.SHA256); err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(file, f.Size)
	if err != nil || reader.Header().FPS != f.FPS || !reflect.DeepEqual(reader.FrameCounts(), f.FrameCounts) {
		t.Fatalf("reader header: %v", err)
	}
	for i, frame := range f.DecodedFrames {
		points, err := reader.Frame(i)
		if err != nil || !reflect.DeepEqual(points, fixturePoints(t, frame)) {
			t.Fatalf("frame %d differs from synthetic quantized fixture: %v", i, err)
		}
	}
	counts := reader.FrameCounts()
	counts[0] = 999
	if reader.FrameCounts()[0] != 1 {
		t.Fatal("caller mutated validated offsets")
	}
	if _, err := reader.Frame(-1); !errors.Is(err, ErrInvalid) {
		t.Fatal("negative frame accepted")
	}
	if _, err := reader.Frame(3); !errors.Is(err, ErrInvalid) {
		t.Fatal("out-of-range frame accepted")
	}
}

func TestReaderRejectsMalformedLengthsCountsFPSAndOffsets(t *testing.T) {
	_, valid := loadFixture(t)
	mutations := map[string]func([]byte) []byte{
		"magic":         func(b []byte) []byte { b[0] ^= 1; return b },
		"version":       func(b []byte) []byte { binary.LittleEndian.PutUint16(b[8:], 2); return b },
		"header size":   func(b []byte) []byte { binary.LittleEndian.PutUint16(b[10:], 0); return b },
		"zero frames":   func(b []byte) []byte { binary.LittleEndian.PutUint32(b[12:], 0); return b },
		"many frames":   func(b []byte) []byte { binary.LittleEndian.PutUint32(b[12:], MaxFrames+1); return b },
		"huge total":    func(b []byte) []byte { binary.LittleEndian.PutUint64(b[16:], ^uint64(0)); return b },
		"zero fps":      func(b []byte) []byte { binary.LittleEndian.PutUint32(b[24:], 0); return b },
		"zero divisor":  func(b []byte) []byte { binary.LittleEndian.PutUint32(b[28:], 0); return b },
		"fps above cap": func(b []byte) []byte { binary.LittleEndian.PutUint32(b[24:], 31); return b },
		"unreduced fps": withUnreducedFPS,
		"first offset":  func(b []byte) []byte { binary.LittleEndian.PutUint64(b[32:], 1); return b },
		"decreasing":    func(b []byte) []byte { binary.LittleEndian.PutUint64(b[48:], 0); return b },
		"wrong final":   func(b []byte) []byte { binary.LittleEndian.PutUint64(b[56:], 2); return b },
		"trailing byte": func(b []byte) []byte { return append(b, 0) },
		"truncated":     func(b []byte) []byte { return b[:len(b)-1] },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			data := mutate(append([]byte(nil), valid...))
			if _, err := NewReader(bytes.NewReader(data), int64(len(data))); !errors.Is(err, ErrInvalid) {
				t.Fatalf("malformed structure accepted: %v", err)
			}
		})
	}
	if _, err := NewReader(bytes.NewReader(valid), MaxBinaryBytes+1); !errors.Is(err, ErrInvalid) {
		t.Fatal("oversize artifact accepted")
	}
	for _, bits := range []uint16{0x7c00, 0xfc00, 0x7e00} {
		data := append([]byte(nil), valid...)
		binary.LittleEndian.PutUint16(data[64:], bits)
		reader, err := NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatal("nonfinite record should reach frame-level validation")
		}
		if points, err := reader.Frame(0); !errors.Is(err, ErrInvalid) || points != nil {
			t.Fatal("nonfinite frame exposed partial points")
		}
	}
	if VerifySHA256(bytes.NewReader(valid), int64(len(valid)), string(bytes.Repeat([]byte("0"), 64))) != ErrInvalid {
		t.Fatal("wrong artifact digest accepted")
	}
}

func withUnreducedFPS(data []byte) []byte {
	binary.LittleEndian.PutUint32(data[24:], 30)
	binary.LittleEndian.PutUint32(data[28:], 2)
	return data
}

type failedWriteFile struct {
	*os.File
	calls, failAt int
}

func (f *failedWriteFile) Write(data []byte) (int, error) {
	f.calls++
	if f.calls == f.failAt {
		return 0, io.ErrShortWrite
	}
	return f.File.Write(data)
}

func TestWriterIsCreateOnlyBoundedAndPermanentlyFailsClosed(t *testing.T) {
	file := ownedFile(t)
	if _, err := file.Write([]byte("existing owned bytes")); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWriter(file, 1, FPS{15, 1}); err != ErrInvalid {
		t.Fatal("nonempty destination accepted")
	}
	if data, err := os.ReadFile(file.Name()); err != nil || string(data) != "existing owned bytes" {
		t.Fatal("refused destination was overwritten")
	}
	for _, point := range []Point{{X: float32(math.NaN())}, {Z: float32(math.Inf(1))}, {Y: 65505}} {
		writer, err := NewWriter(ownedFile(t), 1, FPS{15, 1})
		if err != nil {
			t.Fatal(err)
		}
		if writer.AppendFrame([]Point{point}) != ErrInvalid {
			t.Fatal("nonfinite/out-of-range input accepted")
		}
		if _, err := writer.Finish(); err != ErrInvalid {
			t.Fatal("failed writer became publishable")
		}
	}
	writer, err := NewWriter(ownedFile(t), 1, FPS{15, 1})
	if err != nil {
		t.Fatal(err)
	}
	if writer.AppendFrame(make([]Point, MaxFramePoints+1)) != ErrInvalid {
		t.Fatal("oversize frame accepted")
	}
	writer, err = NewWriter(ownedFile(t), 2, FPS{15, 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.AppendFrame(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Finish(); err != ErrInvalid || writer.AppendFrame(nil) != ErrInvalid {
		t.Fatal("incomplete finish was not permanently refused")
	}
	for failAt := 1; failAt <= 3; failAt++ {
		f := &failedWriteFile{File: ownedFile(t), failAt: failAt}
		writer, err := NewWriter(f, 1, FPS{15, 1})
		if err == nil {
			_ = writer.AppendFrame([]Point{{Z: -1}})
			_, err = writer.Finish()
		}
		if err != ErrInvalid {
			t.Fatal("partial destination write did not poison the writer")
		}
	}
	var zero Writer
	if _, err := zero.Finish(); err != ErrInvalid {
		t.Fatal("zero writer must refuse")
	}
}

func TestReducedRationalFPS(t *testing.T) {
	for _, fps := range []FPS{{1, 1}, {15, 1}, {30, 1}, {30000, 1001}} {
		if !fps.Valid() {
			t.Fatal("valid reduced FPS refused")
		}
	}
	for _, fps := range []FPS{{0, 1}, {1, 0}, {31, 1}, {30, 2}, {1, 2}} {
		if fps.Valid() {
			t.Fatal("invalid/unreduced FPS accepted")
		}
	}
}

func TestPerFrameCapCannotBeHiddenInAValidSizedIndex(t *testing.T) {
	count := uint64(MaxFramePoints + 1)
	data := make([]byte, HeaderBytes+24+int(count)*PointBytes)
	copy(data, magic[:])
	binary.LittleEndian.PutUint16(data[8:], 1)
	binary.LittleEndian.PutUint16(data[10:], HeaderBytes)
	binary.LittleEndian.PutUint32(data[12:], 2)
	binary.LittleEndian.PutUint64(data[16:], count)
	binary.LittleEndian.PutUint32(data[24:], 15)
	binary.LittleEndian.PutUint32(data[28:], 1)
	binary.LittleEndian.PutUint64(data[40:], count)
	binary.LittleEndian.PutUint64(data[48:], count)
	if _, err := NewReader(bytes.NewReader(data), int64(len(data))); err != ErrInvalid {
		t.Fatal("correct total/length concealed an oversize frame")
	}
}

func TestEmptyFramesAreValidWithoutInventedBounds(t *testing.T) {
	file := ownedFile(t)
	writer, err := NewWriter(file, 2, FPS{30000, 1001})
	if err != nil {
		t.Fatal(err)
	}
	if writer.AppendFrame(nil) != nil || writer.AppendFrame(nil) != nil {
		t.Fatal("empty frames refused")
	}
	summary, err := writer.Finish()
	if err != nil || summary.TotalPoints != 0 || summary.Bounds != nil || summary.Size != HeaderBytes+24 {
		t.Fatal("empty sequence summary differs")
	}
	reader, err := NewReader(file, summary.Size)
	if err != nil {
		t.Fatal(err)
	}
	if points, err := reader.Frame(1); err != nil || len(points) != 0 {
		t.Fatal("empty frame was not readable")
	}
}

func FuzzSequenceReader(f *testing.F) {
	_, valid := loadFixture(f)
	f.Add(valid)
	f.Add([]byte{})
	f.Add([]byte("not a sequence"))
	f.Fuzz(func(t *testing.T, data []byte) {
		reader, err := NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			if err != ErrInvalid {
				t.Fatal("non-static structural error")
			}
			return
		}
		if !reader.Header().FPS.Valid() || reader.Header().FrameCount > MaxFrames {
			t.Fatal("accepted invalid header")
		}
		for i := 0; i < int(reader.Header().FrameCount); i++ {
			points, err := reader.Frame(i)
			if err != nil {
				if err != ErrInvalid || points != nil {
					t.Fatal("nonfinite frame leaked partial records")
				}
				continue
			}
			if len(points) > MaxFramePoints {
				t.Fatal("oversize accepted frame")
			}
			for _, point := range points {
				if !finiteCoordinate(point.X) || !finiteCoordinate(point.Y) || !finiteCoordinate(point.Z) {
					t.Fatal("accepted nonfinite record")
				}
			}
		}
	})
}
