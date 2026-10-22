package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/tui/sessionview"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

const sessionViewBodyLimit = 16 * 1024

var errSessionViewUnavailable = errors.New("local session status unavailable; no node was started or stopped")

type sessionStatusRequester func(context.Context, string) (*http.Response, error)

func runSessionAttach(ctx context.Context, stateDir string, request sessionStatusRequester, run func(context.Context, *sessionview.Model) error) error {
	model := sessionview.New(ctx, func(ctx context.Context) (sessionview.Snapshot, error) {
		return loadSessionViewStatus(ctx, stateDir, request)
	})
	defer model.Close()
	return run(ctx, model)
}

// loadSessionViewStatus consumes only the existing local status response. It
// never returns peer bodies, parser details, paths or arbitrary error text.
func loadSessionViewStatus(ctx context.Context, stateDir string, request sessionStatusRequester) (sessionview.Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	response, err := request(ctx, stateDir)
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil || response == nil || response.Body == nil || response.StatusCode != http.StatusOK {
		return sessionview.Snapshot{}, errSessionViewUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, sessionViewBodyLimit+1))
	if err != nil || len(body) > sessionViewBodyLimit || !utf8.Valid(body) {
		return sessionview.Snapshot{}, errSessionViewUnavailable
	}
	return decodeSessionViewStatus(body)
}

func decodeSessionViewStatus(body []byte) (sessionview.Snapshot, error) {
	if len(body) > sessionViewBodyLimit || !utf8.Valid(body) {
		return sessionview.Snapshot{}, errSessionViewUnavailable
	}
	// Reject duplicate root members and trailing values while accepting future
	// optional status fields. No response can enlarge the bounded input buffer.
	decoder := json.NewDecoder(bytes.NewReader(body))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return sessionview.Snapshot{}, errSessionViewUnavailable
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return sessionview.Snapshot{}, errSessionViewUnavailable
		}
		name, ok := key.(string)
		if !ok {
			return sessionview.Snapshot{}, errSessionViewUnavailable
		}
		if _, duplicate := fields[name]; duplicate {
			return sessionview.Snapshot{}, errSessionViewUnavailable
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return sessionview.Snapshot{}, errSessionViewUnavailable
		}
		fields[name] = value
	}
	if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
		return sessionview.Snapshot{}, errSessionViewUnavailable
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return sessionview.Snapshot{}, errSessionViewUnavailable
	}
	var status sessionview.Snapshot
	required := []struct {
		key    string
		target any
	}{
		{"mode", &status.Mode}, {"connected", &status.Connected},
		{"version", &status.Version}, {"pid", &status.PID}, {"uptime_seconds", &status.UptimeSec},
	}
	for _, field := range required {
		raw, present := fields[field.key]
		if !present || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, field.target) != nil {
			return sessionview.Snapshot{}, errSessionViewUnavailable
		}
	}
	for key, target := range map[string]*string{"node_name": &status.NodeName, "mesh_ip": &status.MeshIP} {
		if raw, present := fields[key]; present {
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, target) != nil {
				return sessionview.Snapshot{}, errSessionViewUnavailable
			}
		}
	}
	if (status.Mode != "presence" && status.Mode != "worker") || status.PID <= 0 || status.PID > 1<<32-1 || status.UptimeSec < 0 ||
		status.Version == "" || len(status.Version) > 128 || len(status.NodeName) > 255 || len(status.MeshIP) > 128 {
		return sessionview.Snapshot{}, errSessionViewUnavailable
	}
	return status, nil
}

var sessionAttachCmd = &cobra.Command{
	Use: "attach", Short: "View an already-running local session without starting or stopping it", Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if !bareInvocationIsInteractive() {
			return errors.New("session attach requires an interactive terminal; use citadel session status for scripts")
		}
		// This deliberately does not construct ControlCenter: its collectors,
		// worker, mesh listeners, auth, settings and service actions own state.
		return runSessionAttach(cmd.Context(), network.GetStateDir(), network.SessionControlStatusRequest, func(ctx context.Context, model *sessionview.Model) error {
			_, err := tea.NewProgram(model, tea.WithContext(ctx), tea.WithAltScreen()).Run()
			return err
		})
	},
}

func init() { sessionCmd.AddCommand(sessionAttachCmd) }
