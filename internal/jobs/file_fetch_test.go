package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

const fileFetchTestToken = "opaque_signed-capability.signature"

func fileFetchFixture(t *testing.T, serve http.HandlerFunc) (*FileFetchHandler, *nexus.Job) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("FILE_FETCH requires Linux O_TMPFILE")
	}
	srv := httptest.NewTLSServer(serve)
	t.Cleanup(srv.Close)
	h := NewFileFetchHandler(t.TempDir(), func() string { return "https://example.com" })
	h.lookupIP = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	}
	h.dialIP = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "8.8.8.8:443" {
			t.Errorf("dialed unpinned address: %s", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}
	h.newTransport = func() *http.Transport { return srv.Client().Transport.(*http.Transport).Clone() }
	sum := sha256.Sum256([]byte("film bytes"))
	return h, &nexus.Job{ID: "fetch", Type: "FILE_FETCH", Payload: map[string]string{
		"url":       "https://example.com" + FileFetchEndpointPrefix + fileFetchTestToken,
		"dest_path": "chapters/film.mp4", "expected_sha256": hex.EncodeToString(sum[:]), "max_bytes": "100",
	}}
}

func assertFileFetchClean(t *testing.T, workspace string) {
	t.Helper()
	entries, err := os.ReadDir(workspace)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".file-fetch-") {
			t.Fatalf("partial staging file left behind: %s", entry.Name())
		}
	}
}

func TestFileFetchSuccess(t *testing.T) {
	h, job := fileFetchFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != FileFetchEndpointPrefix+fileFetchTestToken ||
			r.Header.Get("Accept-Encoding") != "identity" || r.Header.Get("Authorization") != "" || r.ContentLength != 0 {
			t.Error("request did not preserve fixed capability GET contract")
		}
		fmt.Fprint(w, "film bytes")
	})
	job.Payload["max_bytes"] = "10" // Exactly at the cap is valid.
	job.Payload["method"] = "POST"
	job.Payload["headers"] = `{"Authorization":"injected"}`
	job.Payload["body"] = "ignored"
	job.Payload["allow_private"] = "true"
	job.Payload["follow_redirects"] = "true"
	var logs []string
	out, err := h.Execute(JobContext{LogFn: func(_ string, msg string) { logs = append(logs, msg) }}, job)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Path   string `json:"path"`
		Bytes  int64  `json:"bytes"`
		SHA256 string `json:"sha256"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(h.WorkspaceDir, "chapters/film.mp4")
	if result.Path != want || result.Bytes != 10 || result.SHA256 != job.Payload["expected_sha256"] {
		t.Fatalf("unexpected result: %s", out)
	}
	if body, err := os.ReadFile(want); err != nil || string(body) != "film bytes" {
		t.Fatalf("bad staged content %q: %v", body, err)
	}
	if info, err := os.Stat(want); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("published mode: %v %v", info, err)
	}
	for _, log := range logs {
		if strings.Contains(log, fileFetchTestToken) || strings.Contains(log, "https://") || strings.Contains(log, "film.mp4") {
			t.Fatalf("sensitive log: %s", log)
		}
	}
	assertFileFetchClean(t, h.WorkspaceDir)
}

func TestFileFetchCreateOnly(t *testing.T) {
	for _, when := range []string{"before_transfer", "before_publish"} {
		t.Run(when, func(t *testing.T) {
			h, job := fileFetchFixture(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "film bytes") })
			job.Payload["dest_path"] = "film.mp4"
			path := filepath.Join(h.WorkspaceDir, "film.mp4")
			create := func() {
				if err := os.WriteFile(path, []byte("other writer"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if when == "before_transfer" {
				create()
			} else {
				calls := 0
				h.Enabled = func() bool {
					calls++
					if calls == 2 {
						create()
					}
					return true
				}
			}
			out, err := h.Execute(JobContext{}, job)
			if err == nil || !strings.Contains(err.Error(), "destination_exists") || len(out) != 0 {
				t.Fatalf("existing destination accepted: %s %v", out, err)
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != "other writer" {
				t.Fatalf("existing destination changed: %q %v", data, err)
			}
			assertFileFetchClean(t, h.WorkspaceDir)
		})
	}
}

func TestFileFetchConcurrentCreateOnly(t *testing.T) {
	h, job := fileFetchFixture(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "film bytes") })
	done := make(chan error, 8)
	for range cap(done) {
		go func() { _, err := h.Execute(JobContext{}, job); done <- err }()
	}
	successes := 0
	for range cap(done) {
		if err := <-done; err == nil {
			successes++
		} else if !strings.Contains(err.Error(), "destination_exists") {
			t.Errorf("unexpected competing-writer refusal: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("create-only publication had %d winners", successes)
	}
	if data, err := os.ReadFile(filepath.Join(h.WorkspaceDir, job.Payload["dest_path"])); err != nil || string(data) != "film bytes" {
		t.Fatalf("published content changed: %q %v", data, err)
	}
	assertFileFetchClean(t, h.WorkspaceDir)
}

// The permission callback runs after hashing/sync, exactly where replacing a
// visible temporary entry previously allowed unverified bytes to be published.
func TestFileFetchAnonymousStagingCannotBeSubstitutedByName(t *testing.T) {
	h, job := fileFetchFixture(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "film bytes") })
	job.Payload["dest_path"] = "film.mp4"
	calls := 0
	h.Enabled = func() bool {
		calls++
		if calls == 2 {
			entries, err := os.ReadDir(h.WorkspaceDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Errorf("staging inode has a visible name before publication: %v", entries)
			}
			// A substitution attack would discover and replace the named temp.
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".file-fetch-") {
					path := filepath.Join(h.WorkspaceDir, entry.Name())
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte("unverified bytes"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
		return true
	}
	if _, err := h.Execute(JobContext{}, job); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("did not exercise post-verification boundary: %d callbacks", calls)
	}
	if data, err := os.ReadFile(filepath.Join(h.WorkspaceDir, "film.mp4")); err != nil || string(data) != "film bytes" {
		t.Fatalf("published unverified data: %q %v", data, err)
	}
	assertFileFetchClean(t, h.WorkspaceDir)
}

func TestFileFetchUnsupportedFilesystemRefusesBeforeHTTP(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs fixture requires Linux")
	}
	h, job := fileFetchFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("unsupported storage reached HTTP") })
	h.WorkspaceDir = "/proc"
	job.Payload["dest_path"] = "file-fetch-test-unsupported"
	if h.Configured() {
		t.Fatal("procfs advertised anonymous staging")
	}
	out, err := h.Execute(JobContext{}, job)
	if err == nil || !strings.Contains(err.Error(), "staging_unavailable") || len(out) != 0 {
		t.Fatalf("unsupported storage accepted: %s %v", out, err)
	}
}

func TestFileFetchRefusalsPreserveOldFile(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		chunked  bool
		encoding string
		change   func(*nexus.Job)
		want     string
	}{
		{name: "declared_size", body: "film bytes", change: func(j *nexus.Job) { j.Payload["max_bytes"] = "3" }, want: "size_exceeded"},
		{name: "stream_size", body: "film bytes", chunked: true, change: func(j *nexus.Job) { j.Payload["max_bytes"] = "3" }, want: "size_exceeded"},
		{name: "hash", body: "wrong bytes", want: "sha256_mismatch"},
		{name: "expired", status: 410, body: fileFetchTestToken, want: "capability_invalid_or_expired"},
		{name: "invalid", status: 403, body: fileFetchTestToken, want: "capability_invalid_or_expired"},
		{name: "auth", status: 401, body: fileFetchTestToken, want: "capability_invalid_or_expired"},
		{name: "server_error", status: 500, body: fileFetchTestToken, want: "unexpected_status"},
		{name: "partial_content", status: 206, body: "film bytes", want: "unexpected_status"},
		{name: "gzip", encoding: "gzip", body: "film bytes", want: "content_encoding_refused"},
		{name: "redirect", status: 302, body: fileFetchTestToken, want: "unexpected_status"},
		{name: "missing_hash", change: func(j *nexus.Job) { delete(j.Payload, "expected_sha256") }, want: "invalid_sha256"},
		{name: "bad_hash", change: func(j *nexus.Job) { j.Payload["expected_sha256"] = fileFetchTestToken }, want: "invalid_sha256"},
		{name: "missing_size", change: func(j *nexus.Job) { delete(j.Payload, "max_bytes") }, want: "invalid_max_bytes"},
		{name: "unbounded_size", change: func(j *nexus.Job) { j.Payload["max_bytes"] = "9223372036854775807" }, want: "invalid_max_bytes"},
		{name: "negative_size", change: func(j *nexus.Job) { j.Payload["max_bytes"] = "-1" }, want: "invalid_max_bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			h, job := fileFetchFixture(t, func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.Header().Set("Location", "http://169.254.169.254/"+fileFetchTestToken)
				if tc.encoding != "" {
					w.Header().Set("Content-Encoding", tc.encoding)
				}
				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}
				if tc.chunked {
					w.(http.Flusher).Flush()
				}
				fmt.Fprint(w, tc.body)
			})
			job.Payload["dest_path"] = "film.mp4"
			if tc.change != nil {
				tc.change(job)
			}
			oldPath := filepath.Join(h.WorkspaceDir, "film.mp4")
			if err := os.WriteFile(oldPath, []byte("old film"), 0600); err != nil {
				t.Fatal(err)
			}
			out, err := h.Execute(JobContext{}, job)
			if err == nil || !strings.Contains(err.Error(), tc.want) || len(out) != 0 {
				t.Fatalf("out=%s err=%v", out, err)
			}
			if strings.Contains(err.Error(), fileFetchTestToken) || strings.Contains(err.Error(), h.WorkspaceDir) {
				t.Fatalf("sensitive error: %v", err)
			}
			if data, readErr := os.ReadFile(oldPath); readErr != nil || string(data) != "old film" {
				t.Fatalf("old file lost: %q %v", data, readErr)
			}
			if hits.Load() > 1 {
				t.Fatal("followed a redirect")
			}
			assertFileFetchClean(t, h.WorkspaceDir)
		})
	}
}

func TestFileFetchURLPolicy(t *testing.T) {
	h := NewFileFetchHandler(t.TempDir(), func() string { return "https://example.com" })
	base := "https://example.com" + FileFetchEndpointPrefix
	for _, raw := range []string{
		"", "http://example.com" + FileFetchEndpointPrefix + "token", "https://evil.example" + FileFetchEndpointPrefix + "token",
		"https://example.com.evil" + FileFetchEndpointPrefix + "token", "https://example.com:443" + FileFetchEndpointPrefix + "token",
		"https://user@example.com" + FileFetchEndpointPrefix + "token", "https://example.com./api/shared/file-fetch/token",
		base, base + ".", base + "..", base + "token/extra", base + "token?x=y", base + "token?", base + "token#", base + "token#x",
		base + "%74oken", base + "token%2fextra", base + "token\\extra", base + strings.Repeat("a", fileFetchMaxTokenBytes+1),
		"https://example.com/api/shared/file-fetch-other/token", "https://example.com/api/shared/../shared/file-fetch/token",
	} {
		if _, err := h.validateURL(raw); err == nil {
			t.Errorf("accepted invalid capability URL %q", raw)
		}
	}
	for _, raw := range []string{"", "http://example.com", "https://example.com:443", "https://user@example.com", "https://example.com/path", "https://example.com#", "https://example.com?", "https://example.com:"} {
		h.Origin = func() string { return raw }
		if h.Configured() {
			t.Errorf("accepted origin %q", raw)
		}
	}
}

func TestFileFetchDestinations(t *testing.T) {
	for _, dest := range []string{"", ".", "..", "../film", "x/../../film", "/tmp/film", "a/../film", "a//film", "a/./film", "C:\\film", "x\\film", "x\x00film", "file:stream"} {
		t.Run(fmt.Sprintf("%q", dest), func(t *testing.T) {
			h, job := fileFetchFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid destination reached HTTP") })
			job.Payload["dest_path"] = dest
			if _, err := h.Execute(JobContext{}, job); err == nil || !strings.Contains(err.Error(), "invalid_dest_path") {
				t.Fatalf("destination accepted: %v", err)
			}
			assertFileFetchClean(t, h.WorkspaceDir)
		})
	}
	for _, leaf := range []bool{false, true} {
		t.Run(fmt.Sprintf("symlink_leaf_%t", leaf), func(t *testing.T) {
			h, job := fileFetchFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("symlink destination reached HTTP") })
			outside := t.TempDir()
			target := outside
			link := filepath.Join(h.WorkspaceDir, "chapters")
			if leaf {
				target = filepath.Join(outside, "film.mp4")
				link = filepath.Join(h.WorkspaceDir, "film.mp4")
				job.Payload["dest_path"] = "film.mp4"
			}
			if err := os.Symlink(target, link); err != nil {
				t.Skip(err)
			}
			if _, err := h.Execute(JobContext{}, job); err == nil {
				t.Fatal("symlink escape accepted")
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 0 {
				t.Fatalf("outside root changed: %v %v", entries, err)
			}
		})
	}
}

func TestFileFetchAncestorSymlinkRace(t *testing.T) {
	outside := t.TempDir()
	var workspace string
	h, job := fileFetchFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if err := os.Rename(filepath.Join(workspace, "chapters"), filepath.Join(workspace, "original")); err != nil {
			t.Error(err)
		}
		if err := os.Symlink(outside, filepath.Join(workspace, "chapters")); err != nil {
			t.Error(err)
		}
		fmt.Fprint(w, "film bytes")
	})
	workspace = h.WorkspaceDir
	if err := os.Mkdir(filepath.Join(workspace, "chapters"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "chapters/film.mp4"), []byte("old film"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Execute(JobContext{}, job); err == nil || !strings.Contains(err.Error(), "publish_failed") {
		t.Fatalf("race not refused: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(workspace, "original/film.mp4")); err != nil || string(data) != "old film" {
		t.Fatalf("old file changed: %q %v", data, err)
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("escaped workspace: %v %v", entries, err)
	}
	assertFileFetchClean(t, workspace)
}

func TestFileFetchDNSAndConnectPolicy(t *testing.T) {
	for _, addresses := range [][]string{
		{}, {"127.0.0.1"}, {"10.0.0.1"}, {"169.254.169.254"}, {"100.64.0.1"}, {"0.0.0.0"},
		{"224.0.0.1"}, {"192.0.2.1"}, {"198.18.0.1"}, {"240.0.0.1"}, {"::1"}, {"fe80::1"}, {"fd00::1"},
		{"::ffff:127.0.0.1"}, {"64:ff9b::7f00:1"}, {"2002:7f00:1::1"}, {"2001:db8::1"},
		{"8.8.8.8", "127.0.0.1"},
	} {
		t.Run(strings.Join(addresses, ","), func(t *testing.T) {
			h := NewFileFetchHandler(t.TempDir(), nil)
			h.lookupIP = func(context.Context, string) ([]net.IPAddr, error) {
				var ips []net.IPAddr
				for _, address := range addresses {
					ips = append(ips, net.IPAddr{IP: net.ParseIP(address)})
				}
				return ips, nil
			}
			h.dialIP = func(context.Context, string, string) (net.Conn, error) {
				t.Error("dialed refused DNS answer")
				return nil, errors.New("unexpected")
			}
			tr := h.transport("example.com")
			if _, err := tr.DialContext(context.Background(), "tcp", "example.com:443"); err == nil {
				t.Fatal("allowed refused destination")
			}
		})
	}
	h := NewFileFetchHandler(t.TempDir(), nil)
	lookups := 0
	h.lookupIP = func(context.Context, string) ([]net.IPAddr, error) {
		lookups++
		if lookups == 1 {
			return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
		}
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}
	dials := 0
	h.dialIP = func(_ context.Context, _ string, address string) (net.Conn, error) {
		dials++
		if address != "8.8.8.8:443" {
			t.Error("numeric address changed")
		}
		a, b := net.Pipe()
		b.Close()
		return a, nil
	}
	tr := h.transport("example.com")
	conn, err := tr.DialContext(context.Background(), "tcp", "example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if _, err := tr.DialContext(context.Background(), "tcp", "example.com:443"); err == nil {
		t.Fatal("reconnect bypassed DNS policy")
	}
	if lookups != 2 || dials != 1 {
		t.Fatalf("lookups=%d dials=%d", lookups, dials)
	}
	if _, err := tr.DialContext(context.Background(), "tcp", "evil.example:443"); err == nil {
		t.Fatal("different hostname accepted")
	}
	if _, err := tr.DialContext(context.Background(), "tcp", "example.com:80"); err == nil {
		t.Fatal("different port accepted")
	}
}

func TestFileFetchProxyIgnored(t *testing.T) {
	var proxyHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { proxyHits.Add(1) }))
	defer proxy.Close()
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(key, proxy.URL)
	}
	h, job := fileFetchFixture(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "film bytes") })
	if _, err := h.Execute(JobContext{}, job); err != nil {
		t.Fatal(err)
	}
	if proxyHits.Load() != 0 {
		t.Fatal("ambient proxy used")
	}
}

func TestFileFetchFailedNewDestinationLeavesNoFile(t *testing.T) {
	for _, failure := range []string{"hash", "truncated", "dns", "connect"} {
		t.Run(failure, func(t *testing.T) {
			h, job := fileFetchFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if failure == "truncated" {
					w.Header().Set("Content-Length", "40")
				}
				fmt.Fprint(w, "wrong bytes")
			})
			if failure == "dns" {
				h.lookupIP = func(context.Context, string) ([]net.IPAddr, error) { return nil, errors.New(fileFetchTestToken) }
			}
			if failure == "connect" {
				h.dialIP = func(context.Context, string, string) (net.Conn, error) { return nil, errors.New(fileFetchTestToken) }
			}
			var logs []string
			out, err := h.Execute(JobContext{LogFn: func(_ string, msg string) { logs = append(logs, msg) }}, job)
			if err == nil || len(out) != 0 {
				t.Fatalf("fetch unexpectedly succeeded: %s", out)
			}
			if strings.Contains(err.Error(), fileFetchTestToken) || strings.Contains(strings.Join(logs, "\n"), fileFetchTestToken) {
				t.Fatal("transport error leaked capability")
			}
			var refusal map[string]string
			if decodeErr := json.Unmarshal([]byte(err.Error()), &refusal); decodeErr != nil || refusal["reason"] == "" {
				t.Fatalf("unstructured refusal: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(h.WorkspaceDir, job.Payload["dest_path"])); !os.IsNotExist(err) {
				t.Fatalf("failed download published destination: %v", err)
			}
			assertFileFetchClean(t, h.WorkspaceDir)
		})
	}
}

func TestFileFetchOwnAddressPolicyFailsClosed(t *testing.T) {
	original := interfaceAddrsFn
	t.Cleanup(func() { interfaceAddrsFn = original })
	for _, cannotEnumerate := range []bool{false, true} {
		interfaceAddrsFn = func() ([]net.Addr, error) {
			if cannotEnumerate {
				return nil, errors.New("unavailable")
			}
			return []net.Addr{&net.IPNet{IP: net.ParseIP("8.8.8.8"), Mask: net.CIDRMask(32, 32)}}, nil
		}
		h, job := fileFetchFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("connected to own address") })
		h.dialIP = func(context.Context, string, string) (net.Conn, error) {
			t.Error("dialed without own-address policy")
			return nil, errors.New("unexpected")
		}
		if _, err := h.Execute(JobContext{}, job); err == nil || !strings.Contains(err.Error(), "destination_blocked") {
			t.Fatalf("own-address policy: %v", err)
		}
		assertFileFetchClean(t, h.WorkspaceDir)
	}
}

func TestFileFetchCancellationAndPermissionRevocation(t *testing.T) {
	for _, kind := range []string{"context", "deadline", "cooperative", "permission"} {
		t.Run(kind, func(t *testing.T) {
			started := make(chan struct{})
			var revoked atomic.Bool
			h, job := fileFetchFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.(http.Flusher).Flush()
				fmt.Fprint(w, "film")
				w.(http.Flusher).Flush()
				close(started)
				if kind == "permission" {
					revoked.Store(true)
					fmt.Fprint(w, " bytes")
					return
				}
				<-r.Context().Done()
			})
			timeout := 3 * time.Second
			if kind == "deadline" {
				timeout = 100 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			var requested atomic.Bool
			jc := JobContext{Ctx: ctx}
			if kind == "cooperative" {
				jc.CancelRequested = requested.Load
			}
			if kind == "permission" {
				h.Enabled = func() bool { return !revoked.Load() }
			}
			job.Payload["dest_path"] = "film.mp4"
			path := filepath.Join(h.WorkspaceDir, "film.mp4")
			if err := os.WriteFile(path, []byte("old film"), 0600); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { _, err := h.Execute(jc, job); done <- err }()
			select {
			case <-started:
			case err := <-done:
				t.Fatalf("fetch did not start: %v", err)
			case <-time.After(time.Second):
				t.Fatal("fetch did not start")
			}
			if kind == "context" {
				cancel()
			} else if kind == "cooperative" {
				requested.Store(true)
			}
			select {
			case err := <-done:
				want := "cancelled_or_timeout"
				if kind == "permission" {
					want = "files_disabled"
				}
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("err=%v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("cancellation did not abort streaming")
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != "old film" {
				t.Fatalf("old file changed: %q %v", data, err)
			}
			assertFileFetchClean(t, h.WorkspaceDir)
		})
	}
}
