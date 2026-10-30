package containerjobs

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"regexp"
	"time"
)

// Exec completion is authoritative only after inspect reports Running=false.
// A closed attach connection or an exited Docker client is not that evidence.
type execState struct {
	Running     bool
	ExitCode    int
	ContainerID string
}
type execControl interface {
	Create(context.Context, string, []string) (string, error)
	Attach(context.Context, string) (io.ReadCloser, error)
	Inspect(context.Context, string) (execState, error)
}
type dockerExecClient struct {
	client        *http.Client
	base, version string
}

var apiVersionPattern = regexp.MustCompile(`^1\.[0-9]{1,3}$`)

func newDockerExecClient() *dockerExecClient {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{DisableKeepAlives: true, ResponseHeaderTimeout: 10 * time.Second, MaxResponseHeaderBytes: 64 << 10,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", "/var/run/docker.sock")
		}}
	return &dockerExecClient{base: "http://docker", client: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrInvalid }}}
}
func (d *dockerExecClient) request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			return nil, ErrInvalid
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, d.base+path, bytes.NewReader(raw))
	if err != nil {
		return nil, ErrInvalid
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := d.client.Do(req)
	if err != nil {
		return nil, ErrUnavailable
	}
	return response, nil
}
func decodeDocker(response *http.Response, status int, target any) error {
	defer response.Body.Close()
	if response.StatusCode != status {
		return ErrUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 || json.Unmarshal(raw, target) != nil {
		return ErrUnavailable
	}
	return nil
}
func (d *dockerExecClient) Create(ctx context.Context, container string, argv []string) (string, error) {
	if !containerPattern.MatchString(container) {
		return "", ErrInvalid
	}
	response, err := d.request(ctx, http.MethodGet, "/version", nil)
	if err != nil {
		return "", err
	}
	var version struct {
		API string `json:"ApiVersion"`
	}
	if decodeDocker(response, http.StatusOK, &version) != nil || !apiVersionPattern.MatchString(version.API) {
		return "", ErrUnavailable
	}
	d.version = "/v" + version.API
	response, err = d.request(ctx, http.MethodPost, d.version+"/containers/"+container+"/exec", struct {
		AttachStdout, AttachStderr bool
		Cmd                        []string
	}{true, true, argv})
	if err != nil {
		return "", err
	}
	var result struct {
		ID string `json:"Id"`
	}
	if decodeDocker(response, http.StatusCreated, &result) != nil || !hashPattern.MatchString(result.ID) {
		return "", ErrUnavailable
	}
	return result.ID, nil
}
func (d *dockerExecClient) Attach(ctx context.Context, id string) (io.ReadCloser, error) {
	if !hashPattern.MatchString(id) || d.version == "" {
		return nil, ErrInvalid
	}
	response, err := d.request(ctx, http.MethodPost, d.version+"/exec/"+id+"/start", struct{ Detach, Tty bool }{false, false})
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, ErrUnavailable
	}
	return response.Body, nil
}
func (d *dockerExecClient) Inspect(ctx context.Context, id string) (execState, error) {
	if !hashPattern.MatchString(id) {
		return execState{}, ErrInvalid
	}
	version := d.version
	if version == "" {
		response, err := d.request(ctx, http.MethodGet, "/version", nil)
		if err != nil {
			return execState{}, err
		}
		var v struct {
			API string `json:"ApiVersion"`
		}
		if decodeDocker(response, http.StatusOK, &v) != nil || !apiVersionPattern.MatchString(v.API) {
			return execState{}, ErrUnavailable
		}
		version = "/v" + v.API
	}
	response, err := d.request(ctx, http.MethodGet, version+"/exec/"+id+"/json", nil)
	if err != nil {
		return execState{}, err
	}
	var value struct {
		ID          string
		Running     *bool
		ExitCode    *int
		ContainerID string
	}
	if decodeDocker(response, http.StatusOK, &value) != nil || value.ID != id || value.Running == nil || !hashPattern.MatchString(value.ContainerID) || !*value.Running && value.ExitCode == nil {
		return execState{}, ErrUnavailable
	}
	state := execState{Running: *value.Running, ContainerID: value.ContainerID}
	if value.ExitCode != nil {
		state.ExitCode = *value.ExitCode
	}
	return state, nil
}
func readExecOutput(ctx context.Context, reader io.Reader, out io.Writer) error {
	reader = &contextReader{ctx: ctx, r: reader}
	for {
		var header [8]byte
		_, err := io.ReadFull(reader, header[:])
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return ErrUnavailable
		}
		if (header[0] != 1 && header[0] != 2) || header[1] != 0 || header[2] != 0 || header[3] != 0 {
			return ErrInvalid
		}
		count := int64(binary.BigEndian.Uint32(header[4:]))
		if _, err = io.CopyN(out, reader, count); err != nil {
			return ErrUnavailable
		}
	}
}
