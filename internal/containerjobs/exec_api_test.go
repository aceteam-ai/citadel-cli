package containerjobs

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestContainerDockerExecProtocolNegotiatesAndKeepsArgvTyped(t *testing.T) {
	id := strings.Repeat("e", 64)
	containerID := strings.Repeat("c", 64)
	var seen []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/version":
			io.WriteString(w, `{"ApiVersion":"1.51"}`)
		case "/v1.51/containers/" + testContainer + "/exec":
			var value struct {
				Cmd                        []string
				AttachStdout, AttachStderr bool
			}
			if json.NewDecoder(r.Body).Decode(&value) != nil || len(value.Cmd) != 2 || value.Cmd[1] != "a;echo fixture" || !value.AttachStdout || !value.AttachStderr {
				t.Error("untyped argv or missing attachment")
			}
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{"Id": id})
		case "/v1.51/exec/" + id + "/start":
			frame := make([]byte, 8)
			frame[0] = 1
			binary.BigEndian.PutUint32(frame[4:], 5)
			w.Write(frame)
			io.WriteString(w, "hello")
		case "/v1.51/exec/" + id + "/json":
			json.NewEncoder(w).Encode(map[string]any{"ID": id, "ContainerID": containerID, "Running": false, "ExitCode": 7})
		default:
			t.Error("unexpected engine endpoint")
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	d := &dockerExecClient{base: srv.URL, client: srv.Client()}
	got, err := d.Create(context.Background(), testContainer, []string{"python", "a;echo fixture"})
	if err != nil || got != id {
		t.Fatal(err)
	}
	stream, err := d.Attach(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	out := &boundedOutput{}
	if readExecOutput(context.Background(), stream, out) != nil || string(out.bytes) != "hello" {
		t.Fatal("mux output lost")
	}
	state, err := d.Inspect(context.Background(), id)
	mu.Lock()
	defer mu.Unlock()
	if err != nil || state.Running || state.ExitCode != 7 || state.ContainerID != containerID || len(seen) != 4 {
		t.Fatal(state, err)
	}
}
func TestContainerExecMuxBoundsAndMalformedFraming(t *testing.T) {
	var wire bytes.Buffer
	for _, kind := range []byte{1, 2} {
		var frame [8]byte
		frame[0] = kind
		binary.BigEndian.PutUint32(frame[4:], OutputLimit)
		wire.Write(frame[:])
		wire.Write(bytes.Repeat([]byte{kind}, OutputLimit))
	}
	out := &boundedOutput{}
	if readExecOutput(context.Background(), &wire, out) != nil || len(out.bytes) != OutputLimit || !out.truncated {
		t.Fatal("output cap or drain failed")
	}
	bad := []byte{3, 0, 0, 0, 0, 0, 0, 0}
	if readExecOutput(context.Background(), bytes.NewReader(bad), &boundedOutput{}) == nil {
		t.Fatal("invalid frame accepted")
	}
}
func TestContainerExecInspectMissingStateCannotProveExit(t *testing.T) {
	id := strings.Repeat("e", 64)
	for _, body := range []string{`{}`, `{"ID":"` + id + `","ContainerID":"` + strings.Repeat("c", 64) + `","ExitCode":0}`, `{"ID":"` + id + `","ContainerID":"` + strings.Repeat("c", 64) + `","Running":false}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
		d := &dockerExecClient{base: srv.URL, version: "/v1.51", client: srv.Client()}
		if _, err := d.Inspect(context.Background(), id); err == nil {
			t.Fatal("missing state proved exit")
		}
		srv.Close()
	}
}
