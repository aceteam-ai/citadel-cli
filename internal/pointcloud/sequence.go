// Package pointcloud implements the proposed, inactive point-cloud sequence
// format. A valid codec result grants no file, model or execution authority.
package pointcloud

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"strings"

	"github.com/x448/float16"
)

const HeaderBytes = 32
const PointBytes = 9
const MaxFrames = 1800
const MaxFramePoints = 150000
const MaxBinaryBytes int64 = 2 << 30

var magic = [8]byte{'C', 'I', 'T', 'P', 'C', 'D', '1', 0}

// ErrInvalid is deliberately static: no path, file bytes or coordinates leak.
var ErrInvalid = errors.New("point-cloud sequence invalid")

type FPS struct{ Numerator, Denominator uint32 }

func (f FPS) Valid() bool {
	if f.Denominator == 0 || f.Numerator < f.Denominator || uint64(f.Numerator) > 30*uint64(f.Denominator) {
		return false
	}
	a, b := f.Numerator, f.Denominator
	for b != 0 {
		a, b = b, a%b
	}
	return a == 1
}

type Point struct {
	X, Y, Z float32
	R, G, B uint8
}

type Bounds struct{ Min, Max [3]float32 }

type Header struct {
	FrameCount  uint32
	TotalPoints uint64
	FPS         FPS
}

// Summary describes actual quantized records, not model/calibration accuracy.
type Summary struct {
	Header
	Size        int64
	FrameCounts []uint32
	Bounds      *Bounds
}

func finiteCoordinate(v float32) bool {
	return !math.IsNaN(float64(v)) && !math.IsInf(float64(v), 0) && math.Abs(float64(v)) <= 65504
}

// Reader reads one bounded frame at a time. The caller owns and freezes the
// underlying ReaderAt and must verify the independently bound artifact hash
// before use. Header validation is not a filesystem or content-authenticity gate.
type Reader struct {
	r       io.ReaderAt
	header  Header
	offsets []uint64
	start   int64
}

func readAt(r io.ReaderAt, data []byte, offset int64) error {
	n, err := r.ReadAt(data, offset)
	if n != len(data) || err != nil && err != io.EOF {
		return ErrInvalid
	}
	return nil
}

func NewReader(r io.ReaderAt, size int64) (*Reader, error) {
	if r == nil || size < HeaderBytes || size > MaxBinaryBytes {
		return nil, ErrInvalid
	}
	var raw [HeaderBytes]byte
	if readAt(r, raw[:], 0) != nil || string(raw[:8]) != string(magic[:]) ||
		binary.LittleEndian.Uint16(raw[8:10]) != 1 || binary.LittleEndian.Uint16(raw[10:12]) != HeaderBytes {
		return nil, ErrInvalid
	}
	h := Header{
		FrameCount:  binary.LittleEndian.Uint32(raw[12:16]),
		TotalPoints: binary.LittleEndian.Uint64(raw[16:24]),
		FPS:         FPS{binary.LittleEndian.Uint32(raw[24:28]), binary.LittleEndian.Uint32(raw[28:32])},
	}
	if h.FrameCount == 0 || h.FrameCount > MaxFrames || !h.FPS.Valid() {
		return nil, ErrInvalid
	}
	start := int64(HeaderBytes + 8*(uint64(h.FrameCount)+1))
	// Check the quotient before multiplication: hostile uint64 totals cannot
	// wrap the expected-size calculation or drive a large allocation.
	if start > size || h.TotalPoints > uint64((size-start)/PointBytes) ||
		start+int64(h.TotalPoints)*PointBytes != size {
		return nil, ErrInvalid
	}
	table := make([]byte, 8*(int(h.FrameCount)+1))
	if readAt(r, table, HeaderBytes) != nil {
		return nil, ErrInvalid
	}
	offsets := make([]uint64, int(h.FrameCount)+1)
	for i := range offsets {
		offsets[i] = binary.LittleEndian.Uint64(table[8*i:])
		if offsets[i] > h.TotalPoints || i == 0 && offsets[i] != 0 ||
			i > 0 && (offsets[i] < offsets[i-1] || offsets[i]-offsets[i-1] > MaxFramePoints) {
			return nil, ErrInvalid
		}
	}
	if offsets[len(offsets)-1] != h.TotalPoints {
		return nil, ErrInvalid
	}
	return &Reader{r: r, header: h, offsets: offsets, start: start}, nil
}

func (r *Reader) Header() Header { return r.header }

func (r *Reader) FrameCounts() []uint32 {
	counts := make([]uint32, r.header.FrameCount)
	for i := range counts {
		counts[i] = uint32(r.offsets[i+1] - r.offsets[i])
	}
	return counts
}

func (r *Reader) Frame(index int) ([]Point, error) {
	if index < 0 || index >= int(r.header.FrameCount) {
		return nil, ErrInvalid
	}
	count := int(r.offsets[index+1] - r.offsets[index])
	data := make([]byte, PointBytes*count)
	if count > 0 && readAt(r.r, data, r.start+int64(r.offsets[index])*PointBytes) != nil {
		return nil, ErrInvalid
	}
	points := make([]Point, count)
	for i := range points {
		p := data[PointBytes*i : PointBytes*(i+1)]
		x, y, z := float16.Frombits(binary.LittleEndian.Uint16(p)).Float32(),
			float16.Frombits(binary.LittleEndian.Uint16(p[2:])).Float32(),
			float16.Frombits(binary.LittleEndian.Uint16(p[4:])).Float32()
		if !finiteCoordinate(x) || !finiteCoordinate(y) || !finiteCoordinate(z) {
			return nil, ErrInvalid
		}
		points[i] = Point{x, y, z, p[6], p[7], p[8]}
	}
	return points, nil
}

// VerifySHA256 scans a bounded caller-owned immutable artifact. It does not
// authenticate the supplied digest, freeze an inode, or open any filename.
func VerifySHA256(r io.ReaderAt, size int64, want string) error {
	if r == nil || size < HeaderBytes || size > MaxBinaryBytes || len(want) != 64 || want != strings.ToLower(want) {
		return ErrInvalid
	}
	if _, err := hex.DecodeString(want); err != nil {
		return ErrInvalid
	}
	h := sha256.New()
	buffer := make([]byte, 64<<10)
	for offset := int64(0); offset < size; {
		n := int64(len(buffer))
		if size-offset < n {
			n = size - offset
		}
		if readAt(r, buffer[:n], offset) != nil {
			return ErrInvalid
		}
		_, _ = h.Write(buffer[:n])
		offset += n
	}
	if hex.EncodeToString(h.Sum(nil)) != want {
		return ErrInvalid
	}
	return nil
}

// Writer requires a new empty, caller-owned seekable destination, never a path.
// Partial/failed output is not publishable. The caller must sync, hash, enforce
// create-only publication and retain execution custody independently.
type Writer struct {
	w       io.WriteSeeker
	header  Header
	offsets []uint64
	frames  int
	size    int64
	bounds  *Bounds
	err     error
	done    bool
}

func NewWriter(w io.WriteSeeker, frames uint32, fps FPS) (*Writer, error) {
	if w == nil || frames == 0 || frames > MaxFrames || !fps.Valid() {
		return nil, ErrInvalid
	}
	end, err := w.Seek(0, io.SeekEnd)
	if err != nil || end != 0 {
		return nil, ErrInvalid
	}
	if position, err := w.Seek(0, io.SeekStart); err != nil || position != 0 {
		return nil, ErrInvalid
	}
	writer := &Writer{w: w, header: Header{FrameCount: frames, FPS: fps}, offsets: make([]uint64, int(frames)+1)}
	writer.size = int64(HeaderBytes + 8*len(writer.offsets))
	if writer.write(make([]byte, writer.size)) != nil {
		return nil, ErrInvalid
	}
	return writer, nil
}

func (w *Writer) write(data []byte) error {
	n, err := w.w.Write(data)
	if err != nil || n != len(data) {
		w.err = ErrInvalid
	}
	return w.err
}

func (w *Writer) AppendFrame(points []Point) error {
	if w.w == nil || w.done || w.err != nil || w.frames >= int(w.header.FrameCount) || len(points) > MaxFramePoints ||
		int64(len(points))*PointBytes > MaxBinaryBytes-w.size {
		w.err = ErrInvalid
		return w.err
	}
	data := make([]byte, len(points)*PointBytes)
	for i, p := range points {
		if !finiteCoordinate(p.X) || !finiteCoordinate(p.Y) || !finiteCoordinate(p.Z) {
			w.err = ErrInvalid
			return w.err
		}
		entry := data[PointBytes*i : PointBytes*(i+1)]
		binary.LittleEndian.PutUint16(entry, float16.Fromfloat32(p.X).Bits())
		binary.LittleEndian.PutUint16(entry[2:], float16.Fromfloat32(p.Y).Bits())
		binary.LittleEndian.PutUint16(entry[4:], float16.Fromfloat32(p.Z).Bits())
		entry[6], entry[7], entry[8] = p.R, p.G, p.B
	}
	if w.write(data) != nil {
		return w.err
	}
	for i := range points {
		entry := data[PointBytes*i:]
		q := [3]float32{float16.Frombits(binary.LittleEndian.Uint16(entry)).Float32(), float16.Frombits(binary.LittleEndian.Uint16(entry[2:])).Float32(), float16.Frombits(binary.LittleEndian.Uint16(entry[4:])).Float32()}
		if w.bounds == nil {
			w.bounds = &Bounds{Min: q, Max: q}
		}
		for axis, value := range q {
			if value < w.bounds.Min[axis] {
				w.bounds.Min[axis] = value
			}
			if value > w.bounds.Max[axis] {
				w.bounds.Max[axis] = value
			}
		}
	}
	w.size += int64(len(data))
	w.header.TotalPoints += uint64(len(points))
	w.frames++
	w.offsets[w.frames] = w.header.TotalPoints
	return nil
}

func (w *Writer) Finish() (Summary, error) {
	if w.w == nil || w.done || w.err != nil || w.frames != int(w.header.FrameCount) {
		w.err = ErrInvalid
		return Summary{}, w.err
	}
	w.done = true
	header := make([]byte, HeaderBytes+8*len(w.offsets))
	copy(header, magic[:])
	binary.LittleEndian.PutUint16(header[8:], 1)
	binary.LittleEndian.PutUint16(header[10:], HeaderBytes)
	binary.LittleEndian.PutUint32(header[12:], w.header.FrameCount)
	binary.LittleEndian.PutUint64(header[16:], w.header.TotalPoints)
	binary.LittleEndian.PutUint32(header[24:], w.header.FPS.Numerator)
	binary.LittleEndian.PutUint32(header[28:], w.header.FPS.Denominator)
	counts := make([]uint32, w.header.FrameCount)
	for i, offset := range w.offsets {
		binary.LittleEndian.PutUint64(header[HeaderBytes+8*i:], offset)
		if i > 0 {
			counts[i-1] = uint32(offset - w.offsets[i-1])
		}
	}
	if position, err := w.w.Seek(0, io.SeekStart); err != nil || position != 0 {
		w.err = ErrInvalid
		return Summary{}, w.err
	}
	if w.write(header) != nil {
		return Summary{}, w.err
	}
	if position, err := w.w.Seek(w.size, io.SeekStart); err != nil || position != w.size {
		w.err = ErrInvalid
		return Summary{}, w.err
	}
	var bounds *Bounds
	if w.bounds != nil {
		copy := *w.bounds
		bounds = &copy
	}
	return Summary{Header: w.header, Size: w.size, FrameCounts: counts, Bounds: bounds}, nil
}
