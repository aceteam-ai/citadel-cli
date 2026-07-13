// internal/jobs/meeting_transcribe_window.go
//
// Bounded rolling-window WAV clipping for during-call transcription (issue
// #5435, epic #5097). The whisper sidecar transcribes a WHOLE file (no
// offset/streaming API), so re-transcribing the entire wav-so-far every rolling
// pass makes each pass slower as the call grows — the latency wall. This clips
// only the trailing window of audio into a scratch WAV so per-pass cost is
// bounded by the window length, not the call length.
//
// WHY MANUAL BYTE-RANGE CLIP (not `ffmpeg -ss`):
//   - The recording is STILL BEING WRITTEN. ffmpeg only finalizes the RIFF/data
//     chunk sizes in the WAV header on SIGINT (end of call); mid-recording those
//     size fields hold a placeholder (0 or 0xFFFFFFFF). So the header's data size
//     is unreliable — we take the TRUE data length from os.Stat (file size minus
//     the PCM start offset) and read the fixed audio format (byte rate, frame
//     size) from the fmt chunk, which is valid from t=0.
//   - A pure byte-range copy is dependency-free and unit-testable, and cannot be
//     tripped up by ffmpeg refusing to seek a file with a placeholder header.
//
// The clip start is aligned DOWN to a whole audio frame so we never cut a sample
// in half; the returned offset (clip-start seconds) is added back to each
// segment's times by the caller to restore the absolute recording timeline,
// preserving the rolling driver's absolute-start dedup/stabilization contract.
package jobs

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// meetingWindowWavPath is the workspace-local scratch file each rolling pass
// clips the trailing window into. It sits beside the recording under meetings/
// (so it passes the transcribe handler's workspace ValidatePath) and is
// overwritten every pass (passes are serialized under transcribeMu), keyed by
// the sanitized meeting id.
func meetingWindowWavPath(workspaceDir, meetingID string) string {
	return filepath.Join(workspaceDir, "meetings", sanitizeMeetingFilename(meetingID)+"-window.wav")
}

// wavFormat holds the parsed, size-independent fields of a WAV's fmt chunk plus
// the byte offset where PCM samples begin.
type wavFormat struct {
	channels      int
	bitsPerSample int
	byteRate      int   // bytes per second of audio (sampleRate*channels*bits/8)
	dataOffset    int64 // file offset of the first PCM sample byte
}

// frameSize is the size in bytes of one audio frame (all channels, one sample).
// Clip boundaries are aligned to it so a sample is never split.
func (f wavFormat) frameSize() int64 {
	fs := int64(f.channels) * int64(f.bitsPerSample) / 8
	if fs <= 0 {
		return 1
	}
	return fs
}

// clipWavTail writes the trailing `window` of audio from srcPath into dstPath as
// a standalone WAV (reusing src's header, patched to the clip's sizes) and
// returns the clip-start offset in SECONDS on the source timeline. When the
// recording is shorter than the window the whole file is copied and the offset is
// 0 (identical to whole-file transcription). It reads the true audio length from
// the file size, never the (mid-recording, placeholder) header size.
func clipWavTail(srcPath, dstPath string, window time.Duration) (float64, error) {
	src, err := os.Open(srcPath)
	if err != nil {
		return 0, err
	}
	defer src.Close()

	info, err := src.Stat()
	if err != nil {
		return 0, err
	}
	fileSize := info.Size()

	format, header, err := readWavHeader(src)
	if err != nil {
		return 0, err
	}

	dataBytes := fileSize - format.dataOffset
	if dataBytes < 0 {
		return 0, fmt.Errorf("wav data offset %d exceeds file size %d", format.dataOffset, fileSize)
	}

	frame := format.frameSize()
	// True audio length, aligned down to a whole frame (a partially-written final
	// frame is dropped so we never feed the sidecar a split sample).
	dataBytes -= dataBytes % frame

	windowBytes := int64(window.Seconds() * float64(format.byteRate))
	windowBytes -= windowBytes % frame
	if windowBytes < frame {
		windowBytes = frame
	}

	clipBytes := dataBytes
	if clipBytes > windowBytes {
		clipBytes = windowBytes
	}
	clipStart := dataBytes - clipBytes // already frame-aligned (both operands are)
	offsetSeconds := float64(clipStart) / float64(format.byteRate)

	pcm := make([]byte, clipBytes)
	if _, err := src.ReadAt(pcm, format.dataOffset+clipStart); err != nil && err != io.EOF {
		return 0, fmt.Errorf("read wav tail: %w", err)
	}

	out := patchWavHeaderSizes(header, format, clipBytes)
	out = append(out, pcm...)
	if err := os.WriteFile(dstPath, out, 0o600); err != nil {
		return 0, err
	}
	return offsetSeconds, nil
}

// readWavHeader parses a canonical RIFF/WAVE header off r, returning the fmt
// fields, the PCM start offset, and the raw header bytes (everything before the
// first sample) so the clip can reuse the source's exact fmt chunk. It walks the
// chunk list rather than assuming a fixed 44-byte layout, so extra chunks (e.g.
// a LIST/INFO chunk) before "data" are tolerated.
func readWavHeader(r io.ReaderAt) (wavFormat, []byte, error) {
	var riff [12]byte
	if _, err := r.ReadAt(riff[:], 0); err != nil {
		return wavFormat{}, nil, fmt.Errorf("read RIFF header: %w", err)
	}
	if string(riff[0:4]) != "RIFF" || string(riff[8:12]) != "WAVE" {
		return wavFormat{}, nil, fmt.Errorf("not a RIFF/WAVE file")
	}

	var format wavFormat
	haveFmt := false
	offset := int64(12)
	var chunkHdr [8]byte
	for {
		if _, err := r.ReadAt(chunkHdr[:], offset); err != nil {
			return wavFormat{}, nil, fmt.Errorf("read chunk header at %d: %w", offset, err)
		}
		id := string(chunkHdr[0:4])
		size := int64(binary.LittleEndian.Uint32(chunkHdr[4:8]))
		body := offset + 8

		switch id {
		case "fmt ":
			fmtBody := make([]byte, 16)
			if _, err := r.ReadAt(fmtBody, body); err != nil {
				return wavFormat{}, nil, fmt.Errorf("read fmt chunk: %w", err)
			}
			format.channels = int(binary.LittleEndian.Uint16(fmtBody[2:4]))
			format.byteRate = int(binary.LittleEndian.Uint32(fmtBody[8:12]))
			format.bitsPerSample = int(binary.LittleEndian.Uint16(fmtBody[14:16]))
			if format.channels <= 0 || format.byteRate <= 0 || format.bitsPerSample <= 0 {
				return wavFormat{}, nil, fmt.Errorf("invalid fmt chunk (channels=%d byteRate=%d bits=%d)", format.channels, format.byteRate, format.bitsPerSample)
			}
			haveFmt = true
		case "data":
			if !haveFmt {
				return wavFormat{}, nil, fmt.Errorf("data chunk precedes fmt chunk")
			}
			format.dataOffset = body
			header := make([]byte, body)
			if _, err := r.ReadAt(header, 0); err != nil {
				return wavFormat{}, nil, fmt.Errorf("read header prefix: %w", err)
			}
			return format, header, nil
		}

		// Advance to the next chunk (chunks are word-aligned: odd sizes pad by 1).
		offset = body + size
		if size%2 == 1 {
			offset++
		}
	}
}

// patchWavHeaderSizes returns a copy of the source header with the RIFF chunk
// size and the data chunk size rewritten to match a clip carrying clipBytes of
// PCM, so the scratch WAV is self-consistent and finite (the source header's
// sizes are the mid-recording placeholders we must not copy).
func patchWavHeaderSizes(header []byte, format wavFormat, clipBytes int64) []byte {
	out := make([]byte, len(header))
	copy(out, header)
	// RIFF chunk size lives at bytes 4:8 and covers everything after it:
	// (headerLen - 8) + clipBytes.
	binary.LittleEndian.PutUint32(out[4:8], uint32(int64(len(header))-8+clipBytes))
	// The data chunk size is the 4 bytes immediately preceding the PCM start.
	binary.LittleEndian.PutUint32(out[format.dataOffset-4:format.dataOffset], uint32(clipBytes))
	return out
}
