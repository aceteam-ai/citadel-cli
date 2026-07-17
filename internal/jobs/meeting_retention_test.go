package jobs

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestSelectPrunableMeetingFiles(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	maxAge := 30 * 24 * time.Hour
	old := now.Add(-40 * 24 * time.Hour)   // older than maxAge
	recent := now.Add(-1 * 24 * time.Hour) // within maxAge

	tests := []struct {
		name         string
		files        []meetingFileInfo
		diskPressure bool
		want         []string
	}{
		{
			name:  "empty input",
			files: nil,
			want:  nil,
		},
		{
			name: "age branch prunes only expired",
			files: []meetingFileInfo{
				{Path: "/m/old.wav", ModTime: old},
				{Path: "/m/recent.wav", ModTime: recent},
			},
			want: []string{"/m/old.wav"},
		},
		{
			name: "exactly at maxAge is pruned (>=)",
			files: []meetingFileInfo{
				{Path: "/m/edge.wav", ModTime: now.Add(-maxAge)},
			},
			want: []string{"/m/edge.wav"},
		},
		{
			name: "disk pressure prunes all non-protected regardless of age",
			files: []meetingFileInfo{
				{Path: "/m/old.wav", ModTime: old},
				{Path: "/m/recent.wav", ModTime: recent},
				{Path: "/m/recent.opus", ModTime: recent},
			},
			diskPressure: true,
			want:         []string{"/m/old.wav", "/m/recent.wav", "/m/recent.opus"},
		},
		{
			name: "protected never pruned by age",
			files: []meetingFileInfo{
				{Path: "/m/old.wav", ModTime: old, Protected: true},
			},
			want: nil,
		},
		{
			name: "protected never pruned under disk pressure",
			files: []meetingFileInfo{
				{Path: "/m/recent.wav", ModTime: recent, Protected: true},
				{Path: "/m/other.wav", ModTime: recent},
			},
			diskPressure: true,
			want:         []string{"/m/other.wav"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := selectPrunableMeetingFiles(tt.files, now, maxAge, tt.diskPressure)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("selectPrunableMeetingFiles = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestPruneMeetingRecordings_AgeBased exercises the filesystem sweep: only files
// older than maxAge are removed; recent ones and non-meeting files survive.
func TestPruneMeetingRecordings_AgeBased(t *testing.T) {
	ws := t.TempDir()
	meetings := filepath.Join(ws, "meetings")
	if err := os.MkdirAll(meetings, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	oldWav := writeAged(t, meetings, "old.wav", 40*24*time.Hour)
	oldOpus := writeAged(t, meetings, "stray.opus", 40*24*time.Hour)
	recentWav := writeAged(t, meetings, "recent.wav", 1*24*time.Hour)
	unrelated := writeAged(t, meetings, "notes.txt", 40*24*time.Hour)

	h := &MeetingJoinHandler{WorkspaceDir: ws, diskPressureFn: func(string) bool { return false }}
	h.pruneMeetingRecordings(JobContext{}, 30*24*time.Hour)

	assertGone(t, oldWav)
	assertGone(t, oldOpus)
	assertExists(t, recentWav)
	assertExists(t, unrelated) // non-.wav/.opus never touched
}

// TestPruneMeetingRecordings_DiskPressureProtectsCurrent verifies that under
// disk pressure a recent WAV is pruned unless it is the protected current run.
func TestPruneMeetingRecordings_DiskPressureProtectsCurrent(t *testing.T) {
	ws := t.TempDir()
	meetings := filepath.Join(ws, "meetings")
	if err := os.MkdirAll(meetings, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	current := writeAged(t, meetings, "current.wav", 0)
	otherRecent := writeAged(t, meetings, "other.wav", 1*time.Hour)

	h := &MeetingJoinHandler{WorkspaceDir: ws, diskPressureFn: func(string) bool { return true }}
	h.pruneMeetingRecordings(JobContext{}, 30*24*time.Hour, current)

	assertExists(t, current)   // protected
	assertGone(t, otherRecent) // pruned under pressure
}

func TestPruneMeetingRecordings_NoDir(t *testing.T) {
	// No meetings/ dir yet — must be a no-op, not a panic.
	h := &MeetingJoinHandler{WorkspaceDir: t.TempDir(), diskPressureFn: func(string) bool { return false }}
	h.pruneMeetingRecordings(JobContext{}, 30*24*time.Hour)
}

func writeAged(t *testing.T, dir, name string, age time.Duration) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	mt := time.Now().Add(-age)
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatalf("chtimes %s: %v", name, err)
	}
	return p
}

func assertGone(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("expected %s to be pruned, stat err = %v", p, err)
	}
}

func assertExists(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Stat(p); err != nil {
		t.Errorf("expected %s to survive, stat err = %v", p, err)
	}
}
