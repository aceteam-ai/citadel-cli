// internal/jobs/meeting_audio_backup.go
//
// Sovereign meeting-audio backup — the NODE side (aceteam#5097, epic #5097).
//
// Product decision (Jason): after a call is recorded, the lossless WAV stays on
// the user's own Citadel node AND a default-on, Opus-compressed backup is
// uploaded to the AceTeam backend — a dual copy. This file implements that
// backup:
//
//  1. Transcode the WAV to Opus-in-Ogg by running ffmpeg INSIDE the already
//     running meeting container (`docker exec citadel-meeting ffmpeg …`). The
//     meeting module ships ffmpeg+libopus, so this needs no host ffmpeg (not
//     guaranteed on arbitrary nodes) and no meeting-service image change — the
//     same pattern as ollama_pull.go's `docker exec citadel-ollama`.
//  2. Upload the Opus bytes to the backend contract from aceteam #6088:
//     POST {base}/api/meetings/{meeting_id}/audio with a Bearer device key and
//     Content-Type audio/ogg;codecs=opus. The job's meeting_id IS the
//     meeting_sessions UUID the route keys on (backend sets
//     payload["meeting_id"] = session_id).
//
// The whole path is BEST-EFFORT: any failure logs and returns nil so the
// meeting job still succeeds — the transcript is already stored, the backup is
// a bonus.
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

const (
	// meetingContainerName is the fixed container_name from the meeting module's
	// compose (services/meeting-service/compose.yml). We `docker exec` ffmpeg
	// into it to transcode without a host ffmpeg dependency.
	meetingContainerName = "citadel-meeting"
	// meetingContainerWorkspaceMount is where the node workspace is bind-mounted
	// inside the meeting container (compose: ${CITADEL_WORKSPACE}:/workspace).
	// The WAV the recorder wrote and the Opus we transcode both live under it,
	// so ffmpeg's in/out paths are this + the workspace-relative path.
	meetingContainerWorkspaceMount = "/workspace"
	// meetingAudioBitrateKbps is the Opus target bitrate. 24 kbps VoIP-mode Opus
	// is transparent for speech; a 4h meeting lands ~43 MB, well under the cap.
	meetingAudioBitrateKbps = 24
	// meetingAudioMaxUploadBytes mirrors the backend's 150 MB cap
	// (MAX_MEETING_AUDIO_BYTES in the Next.js route + python storage). Enforced
	// node-side so an oversize file is skipped before we open the connection.
	meetingAudioMaxUploadBytes = 150 * 1024 * 1024
	// meetingAudioContentType is the exact upload media type the backend accepts
	// (it matches on the base type `audio/ogg`, ignoring the codecs parameter).
	meetingAudioContentType = "audio/ogg;codecs=opus"
	// meetingTranscodeTimeout bounds the in-container ffmpeg transcode. A whole
	// meeting re-encodes faster than real time, but a long call plus a busy node
	// warrants headroom.
	meetingTranscodeTimeout = 10 * time.Minute
	// meetingUploadTimeout bounds a single upload POST.
	meetingUploadTimeout = 5 * time.Minute
	// defaultBackupRetentionAge is the handler-level fallback used when the
	// AudioRetentionAge field is unset (zero). Mirrors the config default so a
	// handler constructed without wiring still prunes sanely.
	defaultBackupRetentionAge = 30 * 24 * time.Hour
)

// meetingOpusRelPath is the workspace-RELATIVE Opus path, the sibling of
// meetingWavRelPath. Kept adjacent so the two can never drift.
func meetingOpusRelPath(meetingID string) string {
	return "meetings/" + sanitizeMeetingFilename(meetingID) + ".opus"
}

// meetingOpusPath is the host-absolute Opus path, the sibling of meetingWavPath.
func meetingOpusPath(workspaceDir, meetingID string) string {
	return path.Join(workspaceDir, "meetings", sanitizeMeetingFilename(meetingID)+".opus")
}

// transcodeDockerArgs builds the full `docker` argv (sans the leading "docker")
// that execs ffmpeg inside the meeting container to transcode the recorded WAV
// to Opus-in-Ogg. Both paths are CONTAINER paths under the /workspace mount.
//
// When uid >= 0 (Linux) we pass `-u uid:gid` so the file ffmpeg writes is owned
// by the NODE owner — the same PUID/PGID guarantee the compose entrypoint gives
// the WAV. `docker exec` bypasses the entrypoint's privilege drop and would
// otherwise write a root-owned Opus the node process may not be able to read
// back for upload. On platforms where os.Getuid() returns -1 (Windows) we omit
// -u and rely on the container's own user.
//
// Pure (no I/O) so the arg construction — the load-bearing bitrate/paths/flags —
// is unit-tested directly.
func transcodeDockerArgs(container, wavContainerPath, opusContainerPath string, bitrateKbps, uid, gid int) []string {
	args := []string{"exec"}
	if uid >= 0 && gid >= 0 {
		args = append(args, "-u", strconv.Itoa(uid)+":"+strconv.Itoa(gid))
	}
	args = append(args,
		container,
		"ffmpeg",
		"-nostdin",
		"-y",
		"-i", wavContainerPath,
		"-c:a", "libopus",
		"-b:a", strconv.Itoa(bitrateKbps)+"k",
		"-application", "voip",
		opusContainerPath,
	)
	return args
}

// runDockerExecReal is the production docker runner used when a handler does not
// inject a fake. args is the full docker argv (args[0] == "exec"). It honors the
// context so a per-job deadline or cancellation actually terminates ffmpeg.
func runDockerExecReal(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "docker", args...).CombinedOutput()
}

// SetAudioBackup configures the sovereign audio-backup path: the default-on
// toggle, the local-recording retention window, and a creds provider that
// returns the backend base URL + device bearer token at call time (read fresh
// so a rotated token is honored). The worker wires this from the persisted
// meeting config + the device-auth config; a creds provider that returns an
// empty token disables only the upload leg (retention still runs).
func (h *MeetingJoinHandler) SetAudioBackup(enabled bool, retentionAge time.Duration, creds func() (baseURL, token string)) {
	h.AudioBackupEnabled = enabled
	h.AudioRetentionAge = retentionAge
	h.backupCreds = creds
}

// backupExecUIDGID returns the node process's UID/GID so the in-container ffmpeg
// (`docker exec -u`) writes a node-owned Opus. On platforms where os.Getuid()
// returns -1 (Windows), transcodeDockerArgs omits -u.
func backupExecUIDGID() (int, int) {
	return os.Getuid(), os.Getgid()
}

// backupAndPrune is the best-effort post-recording step: upload an Opus backup
// (when enabled) and then run the retention sweep. It NEVER returns an error and
// never touches the meeting result — the transcript is the source of truth; the
// backup and prune are bonuses that must not regress the meeting job.
func (h *MeetingJoinHandler) backupAndPrune(ctx JobContext, job *nexus.Job, p meetingJoinParams, recordedWavPath string) {
	maxAge := h.AudioRetentionAge
	if maxAge <= 0 {
		maxAge = defaultBackupRetentionAge
	}

	uploadConfirmed := false
	if h.AudioBackupEnabled {
		uploadConfirmed = h.uploadAudioBackup(ctx, job, p)
	}

	// Protect the just-recorded WAV from the disk-pressure branch. Its age is ~0
	// so the age branch won't touch it, but under disk pressure an unprotected
	// sweep could — and if its backup did not confirm, losing it is data loss.
	protect := []string{}
	if !uploadConfirmed {
		protect = append(protect, recordedWavPath)
	}
	h.pruneMeetingRecordings(ctx, maxAge, protect...)
}

// uploadAudioBackup transcodes the recorded WAV to Opus inside the meeting
// container and uploads it to the backend. Returns true only when the backend
// confirmed the upload (HTTP 201). Every failure logs and returns false — the
// caller keeps the local WAV (and the retention sweep later prunes any stray
// .opus from a failed upload).
func (h *MeetingJoinHandler) uploadAudioBackup(ctx JobContext, job *nexus.Job, p meetingJoinParams) bool {
	wavContainer := path.Join(meetingContainerWorkspaceMount, meetingWavRelPath(p.MeetingID))
	opusContainer := path.Join(meetingContainerWorkspaceMount, meetingOpusRelPath(p.MeetingID))
	opusHost := meetingOpusPath(h.WorkspaceDir, p.MeetingID)

	// 1) Transcode WAV -> Opus in the meeting container (no host ffmpeg needed).
	uid, gid := backupExecUIDGID()
	args := transcodeDockerArgs(meetingContainerName, wavContainer, opusContainer, meetingAudioBitrateKbps, uid, gid)
	run := h.runDockerExec
	if run == nil {
		run = runDockerExecReal
	}
	tctx, cancel := context.WithTimeout(ctx.Context(), meetingTranscodeTimeout)
	defer cancel()
	if out, err := run(tctx, args...); err != nil {
		ctx.Log("warn", "     - [Job %s] audio backup transcode failed (non-fatal): %v: %s", job.ID, err, strings.TrimSpace(string(out)))
		return false
	}

	// 2) Upload the Opus to the backend contract (aceteam #6088). Creds are read
	// fresh so a rotated device token is honored.
	baseURL, token := "", ""
	if h.backupCreds != nil {
		baseURL, token = h.backupCreds()
	}
	client := h.backupHTTPClient
	if client == nil {
		client = &http.Client{Timeout: meetingUploadTimeout}
	}
	uctx, ucancel := context.WithTimeout(ctx.Context(), meetingUploadTimeout)
	defer ucancel()
	docID, err := uploadMeetingAudio(uctx, client, baseURL, token, p.MeetingID, opusHost, meetingAudioMaxUploadBytes)
	if err != nil {
		ctx.Log("warn", "     - [Job %s] audio backup upload failed (non-fatal): %v", job.ID, err)
		return false
	}
	ctx.Log("info", "     - [Job %s] audio backup uploaded (audio_document_id=%s)", job.ID, docID)

	// Upload confirmed: the backend holds the compressed copy, so remove the
	// local transient .opus (the lossless WAV remains the node's kept copy).
	if rmErr := os.Remove(opusHost); rmErr != nil {
		ctx.Log("warn", "     - [Job %s] could not remove local opus after upload (non-fatal): %v", job.ID, rmErr)
	}
	return true
}

// uploadMeetingAudio POSTs the Opus file to the backend audio-backup contract
// (aceteam #6088). Returns the backend's audio_document_id on 201. Every error
// path is the CALLER's to log-and-ignore (best-effort); this function only
// reports what happened.
//
// The size guard runs before the connection is opened. Content-Length is set
// explicitly (an *os.File body does not auto-populate it) so a strict receiver
// gets a declared length instead of a chunked stream.
func uploadMeetingAudio(ctx context.Context, client *http.Client, baseURL, token, meetingID, opusPath string, maxBytes int64) (string, error) {
	if token == "" {
		return "", fmt.Errorf("no device API token; skipping audio upload")
	}
	if baseURL == "" {
		return "", fmt.Errorf("no API base URL; skipping audio upload")
	}

	info, err := os.Stat(opusPath)
	if err != nil {
		return "", fmt.Errorf("stat opus for upload: %w", err)
	}
	if info.Size() == 0 {
		return "", fmt.Errorf("opus file is empty; skipping upload")
	}
	if info.Size() > maxBytes {
		return "", fmt.Errorf("opus is %d bytes, over the %d-byte cap; skipping upload", info.Size(), maxBytes)
	}

	f, err := os.Open(opusPath)
	if err != nil {
		return "", fmt.Errorf("open opus for upload: %w", err)
	}
	defer f.Close()

	url := meetingAudioUploadURL(baseURL, meetingID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, f)
	if err != nil {
		return "", fmt.Errorf("build upload request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", meetingAudioContentType)
	req.ContentLength = info.Size()

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("upload audio: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("upload returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	var parsed struct {
		AudioDocumentID string `json:"audio_document_id"`
		BytesStored     int64  `json:"bytes_stored"`
	}
	// A 201 with an unparseable body is still a success for our purposes; only
	// the log line loses the document id.
	_ = json.Unmarshal(body, &parsed)
	return parsed.AudioDocumentID, nil
}

// meetingAudioUploadURL builds the backend audio-backup endpoint. meetingID is
// the meeting_sessions UUID (the route validates it as a UUID); it is safe as a
// path segment but we still avoid trailing/leading slash surprises.
func meetingAudioUploadURL(baseURL, meetingID string) string {
	return trimTrailingSlash(baseURL) + "/api/meetings/" + meetingID + "/audio"
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
