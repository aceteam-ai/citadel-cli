//go:build linux

package mediainput

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/coordinatorfetch"
)

func acquisitionContract(t *testing.T, origins []string, data [][]byte) Contract {
	t.Helper()
	inputs := make([]map[string]any, len(origins))
	for i, origin := range origins {
		role := "source"
		if i == 1 {
			role = "mask"
		}
		sum := sha256.Sum256(data[i])
		inputs[i] = map[string]any{
			"role": role, "origin": origin, "sha256": hex.EncodeToString(sum[:]),
			"size": len(data[i]), "format": "png",
		}
		if origin == "inbox" {
			inputs[i]["capability"] = role + "-synthetic.signature"
		} else {
			inputs[i]["path"] = role + ".png"
		}
	}
	raw, err := json.Marshal(map[string]any{"schema": Schema, "inputs": inputs})
	if err != nil {
		t.Fatal(err)
	}
	task := TaskImageToImage
	if len(origins) == 2 {
		task = TaskInpaint
	}
	contract, err := Parse(task, raw)
	if err != nil {
		t.Fatal(err)
	}
	return contract
}

func inboxFixture(t *testing.T, serve http.HandlerFunc) Freezer {
	t.Helper()
	srv := httptest.NewTLSServer(serve)
	t.Cleanup(srv.Close)
	return Freezer{
		Origin:             func() string { return "https://example.com" },
		MediaInputsEnabled: func() bool { return true },
		hooks: coordinatorfetch.Hooks{
			LookupIP: func(context.Context, string) ([]net.IPAddr, error) {
				return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
			},
			DialIP: func(ctx context.Context, network, address string) (net.Conn, error) {
				if address != "8.8.8.8:443" {
					t.Error("media fetch did not use pinned numeric address")
				}
				return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
			},
			NewTransport:   func() *http.Transport { return srv.Client().Transport.(*http.Transport).Clone() },
			InterfaceAddrs: func() ([]net.Addr, error) { return nil, nil },
		},
	}
}

func TestInboxSnapshotWithoutFilesOrWorkspaceIsPrivate(t *testing.T) {
	data := encodedPNG(t, image.NewGray(image.Rect(0, 0, 2, 3)))
	contract := acquisitionContract(t, []string{"inbox"}, [][]byte{data})
	f := inboxFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != coordinatorfetch.MediaInputPath || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer source-synthetic.signature" {
			t.Error("capability route/header changed")
		}
		_, _ = w.Write(data)
	})
	f.WorkspaceDir = t.TempDir()
	f.FilesEnabled = func() bool { return false }
	snapshot, err := f.Freeze(context.Background(), contract)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		reader, err := snapshot.Reader("source")
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(reader)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatal("inbox snapshot reader changed bytes or shared its offset")
		}
		got[0] ^= 0xff
		for _, value := range []any{snapshot, reader, struct{ private Snapshot }{snapshot}, struct{ private io.Reader }{reader}} {
			for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
				text := fmt.Sprintf(format, value)
				for _, forbidden := range []string{fmt.Sprint(data), fmt.Sprintf("%#v", data), string(data), "source-synthetic.signature", f.WorkspaceDir} {
					if strings.Contains(text, forbidden) {
						t.Fatal("inbox snapshot diagnostic leaked private data")
					}
				}
			}
		}
	}
	meta := snapshot.Inputs()
	if len(meta) != 1 || meta[0].Origin != "inbox" || meta[0].Width != 2 || meta[0].Height != 3 {
		t.Fatal("inbox metadata missing verified identity")
	}
	raw, err := json.Marshal(snapshot)
	if err != nil || string(raw) != "{}" {
		t.Fatal("snapshot serialization exposed inbox content")
	}
	entries, err := os.ReadDir(f.WorkspaceDir)
	if err != nil || len(entries) != 0 {
		t.Fatal("inbox input was staged into the workspace")
	}
}

func TestMixedOriginsAndPermissionsBeforeAnyRedemption(t *testing.T) {
	source := encodedPNG(t, image.NewNRGBA(image.Rect(0, 0, 2, 3)))
	mask := encodedPNG(t, image.NewGray(image.Rect(0, 0, 2, 3)))
	for _, origins := range [][]string{{"inbox", "workspace"}, {"workspace", "inbox"}, {"inbox", "inbox"}, {"workspace", "workspace"}} {
		t.Run(strings.Join(origins, "-"), func(t *testing.T) {
			var calls atomic.Int32
			f := inboxFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				switch r.Header.Get("Authorization") {
				case "Bearer source-synthetic.signature":
					_, _ = w.Write(source)
				case "Bearer mask-synthetic.signature":
					_, _ = w.Write(mask)
				default:
					t.Error("wrong role capability")
				}
			})
			f.WorkspaceDir = t.TempDir()
			writeOwnedInput(t, f.WorkspaceDir, "source.png", source)
			writeOwnedInput(t, f.WorkspaceDir, "mask.png", mask)
			f.FilesEnabled = func() bool { return true }
			contract := acquisitionContract(t, origins, [][]byte{source, mask})
			snapshot, err := f.Freeze(context.Background(), contract)
			if err != nil || len(snapshot.Inputs()) != 2 {
				t.Fatalf("mixed acquisition failed: %v", err)
			}
			calls.Store(0)
			for _, enabled := range []func() bool{nil, func() bool { return false }} {
				f.MediaInputsEnabled = enabled
				_, err := f.Freeze(context.Background(), contract)
				assertReason(t, err, "media_inputs_disabled")
			}
			f.MediaInputsEnabled = func() bool { return true }
			if origins[0] == "workspace" || origins[1] == "workspace" {
				f.FilesEnabled = nil
				_, err := f.Freeze(context.Background(), contract)
				assertReason(t, err, "files_disabled")
			}
			if calls.Load() != 0 {
				t.Fatal("denied mixed contract redeemed an inbox capability")
			}
		})
	}
}

func TestInboxIntegritySizeAndHeaderRefusals(t *testing.T) {
	data := encodedPNG(t, image.NewGray(image.Rect(0, 0, 2, 3)))
	for _, kind := range []string{"short", "over", "hash", "header", "wrong-format", "gzip", "redirect", "denied"} {
		t.Run(kind, func(t *testing.T) {
			body := append([]byte(nil), data...)
			want := "sha256_mismatch"
			switch kind {
			case "short":
				body = body[:len(body)-1]
				want = "size_mismatch"
			case "over":
				body = append(body, 0)
				want = "size_exceeded"
			case "hash":
				body[len(body)-1] ^= 1
			case "header":
				body[0] ^= 1
				want = "invalid_image"
			case "wrong-format":
				want = "invalid_image"
			case "gzip":
				want = "content_encoding_refused"
			case "redirect":
				want = "unexpected_status"
			case "denied":
				want = "capability_invalid_or_expired"
			}
			f := inboxFixture(t, func(w http.ResponseWriter, _ *http.Request) {
				switch kind {
				case "gzip":
					w.Header().Set("Content-Encoding", "gzip")
				case "redirect":
					w.Header().Set("Location", "https://example.com/private?token=source-synthetic.signature")
					w.WriteHeader(302)
				case "denied":
					w.WriteHeader(403)
				}
				_, _ = w.Write(body)
			})
			contract := acquisitionContract(t, []string{"inbox"}, [][]byte{data})
			if kind == "header" {
				contract = acquisitionContract(t, []string{"inbox"}, [][]byte{body})
			}
			if kind == "wrong-format" {
				contract.inputs[0].Format = "jpeg"
			}
			snapshot, err := f.Freeze(context.Background(), contract)
			assertReason(t, err, want)
			if len(snapshot.Inputs()) != 0 || strings.Contains(err.Error(), "source-synthetic.signature") || strings.Contains(err.Error(), "example.com") {
				t.Fatal("failed inbox acquisition returned a snapshot or secret diagnostic")
			}
		})
	}
}

func TestInboxRevocationAndCancellationDoNotReturnPartialSnapshot(t *testing.T) {
	data := encodedPNG(t, image.NewGray(image.Rect(0, 0, 2, 3)))
	for _, origins := range [][]string{{"inbox"}, {"inbox", "workspace"}, {"workspace", "inbox"}} {
		for _, cause := range []string{"media", "files", "cancel"} {
			if cause == "files" && len(origins) == 1 {
				continue
			}
			t.Run(strings.Join(origins, "-")+"-"+cause, func(t *testing.T) {
				var calls atomic.Int32
				f := inboxFixture(t, func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); _, _ = w.Write(data) })
				f.WorkspaceDir = t.TempDir()
				writeOwnedInput(t, f.WorkspaceDir, "source.png", data)
				writeOwnedInput(t, f.WorkspaceDir, "mask.png", data)
				media, files := true, true
				f.MediaInputsEnabled = func() bool { return media }
				f.FilesEnabled = func() bool { return files }
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				f.afterRead = func() {
					switch cause {
					case "media":
						media = false
					case "files":
						files = false
					case "cancel":
						cancel()
					}
				}
				inputs := [][]byte{data}
				if len(origins) == 2 {
					inputs = append(inputs, data)
				}
				snapshot, err := f.Freeze(ctx, acquisitionContract(t, origins, inputs))
				want := "media_inputs_disabled"
				if cause == "files" {
					want = "files_disabled"
				} else if cause == "cancel" {
					want = "cancelled_or_timeout"
				}
				assertReason(t, err, want)
				if len(snapshot.Inputs()) != 0 || calls.Load() > 1 || (origins[0] == "workspace" && calls.Load() != 0) {
					t.Fatal("revocation returned a partial snapshot or redeemed the next input")
				}
			})
		}
	}
}

func TestInboxStaticConfigurationRefusalBeforeNetwork(t *testing.T) {
	data := encodedPNG(t, image.NewGray(image.Rect(0, 0, 1, 1)))
	contract := acquisitionContract(t, []string{"inbox"}, [][]byte{data})
	f := Freezer{MediaInputsEnabled: func() bool { return true }, hooks: coordinatorfetch.Hooks{
		LookupIP: func(context.Context, string) ([]net.IPAddr, error) { t.Fatal("static refusal reached DNS"); return nil, nil },
	}}
	_, err := f.Freeze(context.Background(), contract)
	assertReason(t, err, "inbox_unavailable")
	for _, origin := range []string{"http://example.com", "https://example.com/path", "https://example.com?capability=source-synthetic.signature"} {
		f.Origin = func() string { return origin }
		_, err := f.Freeze(context.Background(), contract)
		assertReason(t, err, "untrusted_origin")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = f.Freeze(ctx, contract)
	assertReason(t, err, "cancelled_or_timeout")
}

func TestInboxMasksUseSharedDimensionAndModeGuards(t *testing.T) {
	source := encodedPNG(t, image.NewGray(image.Rect(0, 0, 2, 3)))
	for _, mask := range []image.Image{
		image.NewGray(image.Rect(0, 0, 3, 3)),
		image.NewGray16(image.Rect(0, 0, 2, 3)),
		image.NewNRGBA(image.Rect(0, 0, 2, 3)),
	} {
		data := encodedPNG(t, mask)
		f := inboxFixture(t, func(w http.ResponseWriter, r *http.Request) {
			body := source
			if r.Header.Get("Authorization") == "Bearer mask-synthetic.signature" {
				body = data
			}
			_, _ = w.Write(body)
		})
		snapshot, err := f.Freeze(context.Background(), acquisitionContract(t, []string{"inbox", "inbox"}, [][]byte{source, data}))
		assertReason(t, err, "mask_invalid")
		if len(snapshot.Inputs()) != 0 {
			t.Fatal("invalid inbox mask returned a partial snapshot")
		}
	}
}
