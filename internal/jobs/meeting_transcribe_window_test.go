package jobs

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTestWav writes a canonical 44-byte-header mono 16 kHz s16 WAV carrying
// `seconds` of PCM. Byte i of the PCM is set to byte(i) so a clip's contents can
// be checked against the expected source offset. If placeholderSizes is true the
// RIFF/data size fields are written as 0xFFFFFFFF, emulating a recording that
// ffmpeg has not yet finalized — clipWavTail must ignore those and use the file
// size instead.
func writeTestWav(t *testing.T, path string, seconds int, placeholderSizes bool) (byteRate int, dataBytes int) {
	t.Helper()
	const sampleRate = 16000
	const channels = 1
	const bits = 16
	byteRate = sampleRate * channels * bits / 8 // 32000
	dataBytes = byteRate * seconds

	buf := make([]byte, 44+dataBytes)
	copy(buf[0:4], "RIFF")
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16) // PCM fmt chunk size
	binary.LittleEndian.PutUint16(buf[20:22], 1)  // audio format PCM
	binary.LittleEndian.PutUint16(buf[22:24], channels)
	binary.LittleEndian.PutUint32(buf[24:28], sampleRate)
	binary.LittleEndian.PutUint32(buf[28:32], uint32(byteRate))
	binary.LittleEndian.PutUint16(buf[32:34], channels*bits/8) // block align
	binary.LittleEndian.PutUint16(buf[34:36], bits)
	copy(buf[36:40], "data")
	if placeholderSizes {
		binary.LittleEndian.PutUint32(buf[4:8], 0xFFFFFFFF)
		binary.LittleEndian.PutUint32(buf[40:44], 0xFFFFFFFF)
	} else {
		binary.LittleEndian.PutUint32(buf[4:8], uint32(36+dataBytes))
		binary.LittleEndian.PutUint32(buf[40:44], uint32(dataBytes))
	}
	for i := 0; i < dataBytes; i++ {
		buf[44+i] = byte(i)
	}
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatalf("write test wav: %v", err)
	}
	return byteRate, dataBytes
}

func TestMeetingWindowWavPath_UsesSeparateScratchDir(t *testing.T) {
	// The rolling-window scratch clip is a churny per-pass temp; it must live in a
	// separate scratch subdir, NOT alongside the durable recordings in meetings/,
	// and still inside the workspace so the transcribe handler's ValidatePath
	// accepts it.
	got := meetingWindowWavPath("/ws", "mtg-1")

	meetingsDir := filepath.Join("/ws", "meetings") + string(filepath.Separator)
	if strings.HasPrefix(got, meetingsDir) {
		t.Errorf("window scratch clip %q must NOT live under the meetings/ recordings dir", got)
	}
	want := filepath.Join("/ws", meetingScratchDirName, "mtg-1-window.wav")
	if got != want {
		t.Errorf("meetingWindowWavPath = %q, want %q", got, want)
	}
	// The recording itself stays under meetings/ (the shared, node-readable dir).
	if recDir := filepath.Dir(meetingWavPath("/ws", "mtg-1")); recDir != filepath.Join("/ws", "meetings") {
		t.Errorf("recording dir = %q, want it to remain under meetings/", recDir)
	}
	// The scratch clip must still resolve inside the workspace (ValidatePath, so
	// the transcribe handler accepts it). Use a real temp workspace since
	// ValidatePath resolves symlinks on the workspace root.
	ws := t.TempDir()
	if _, err := ValidatePath(ws, meetingWindowWavPath(ws, "mtg-1")); err != nil {
		t.Errorf("scratch clip path must validate inside the workspace: %v", err)
	}
}

func TestClipWavTail_ClipsTrailingWindow(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "rec.wav")
	dst := filepath.Join(dir, "clip.wav")
	// 100s recording, placeholder header sizes (mid-recording), clip last 30s.
	byteRate, _ := writeTestWav(t, src, 100, true)

	offset, err := clipWavTail(src, dst, 30*time.Second)
	if err != nil {
		t.Fatalf("clipWavTail: %v", err)
	}
	// Clip start = (100 - 30)s = 70s.
	if offset < 69.99 || offset > 70.01 {
		t.Errorf("offset = %v, want ~70", offset)
	}

	format, _, err := readWavHeaderPath(t, dst)
	if err != nil {
		t.Fatalf("read clip header: %v", err)
	}
	info, _ := os.Stat(dst)
	clipData := info.Size() - format.dataOffset
	if clipData != int64(30*byteRate) {
		t.Errorf("clip data bytes = %d, want %d (30s)", clipData, 30*byteRate)
	}
	// The clip's data size field must be self-consistent (not the placeholder).
	if sz := dataSizeField(t, dst, format.dataOffset); sz != uint32(clipData) {
		t.Errorf("clip data-size field = %d, want %d", sz, clipData)
	}
	// The first clip sample must equal source byte at offset 70s (byte value wraps
	// mod 256): source PCM index = 70*32000 = 2_240_000.
	first := firstPCMByte(t, dst, format.dataOffset)
	if want := byte(70 * byteRate); first != want {
		t.Errorf("first clip byte = %d, want %d (source offset preserved)", first, want)
	}
}

func TestClipWavTail_ShorterThanWindowCopiesWhole(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "rec.wav")
	dst := filepath.Join(dir, "clip.wav")
	byteRate, dataBytes := writeTestWav(t, src, 10, false)

	offset, err := clipWavTail(src, dst, 90*time.Second)
	if err != nil {
		t.Fatalf("clipWavTail: %v", err)
	}
	if offset != 0 {
		t.Errorf("offset = %v, want 0 when recording is shorter than the window", offset)
	}
	format, _, _ := readWavHeaderPath(t, dst)
	info, _ := os.Stat(dst)
	if got := info.Size() - format.dataOffset; got != int64(dataBytes) {
		t.Errorf("clip data bytes = %d, want the whole %d", got, dataBytes)
	}
	_ = byteRate
}

func TestClipWavTail_RejectsNonWav(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "bad.wav")
	if err := os.WriteFile(src, []byte("not a wav file at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := clipWavTail(src, filepath.Join(dir, "clip.wav"), 30*time.Second); err == nil {
		t.Error("expected an error clipping a non-WAV file")
	}
}

// readWavHeaderPath is a test helper that opens a file and parses its WAV header.
func readWavHeaderPath(t *testing.T, path string) (wavFormat, []byte, error) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		return wavFormat{}, nil, err
	}
	defer f.Close()
	return readWavHeader(f)
}

func dataSizeField(t *testing.T, path string, dataOffset int64) uint32 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var b [4]byte
	if _, err := f.ReadAt(b[:], dataOffset-4); err != nil {
		t.Fatal(err)
	}
	return binary.LittleEndian.Uint32(b[:])
}

func firstPCMByte(t *testing.T, path string, dataOffset int64) byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var b [1]byte
	if _, err := f.ReadAt(b[:], dataOffset); err != nil {
		t.Fatal(err)
	}
	return b[0]
}
