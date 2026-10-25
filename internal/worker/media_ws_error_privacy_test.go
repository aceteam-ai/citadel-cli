package worker

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
)

func TestMediaWSServerErrorRetentionBoundary(t *testing.T) {
	const secret = "synthetic-ws-server-error.secret"
	for _, detail := range []string{
		secret,
		`{"inputs":{"capability":"` + secret + `"}}`,
		strings.Repeat(secret, 3000),
		"",
	} {
		var logs, levels []string
		source := NewWSSource(WSSourceConfig{
			LogFn: func(level, message string) {
				levels = append(levels, level)
				logs = append(logs, message)
			},
			DebugFunc: func(format string, args ...any) {
				logs = append(logs, fmt.Sprintf(format, args...))
			},
		})
		original := redisapi.WSMessage{Type: "error", Error: detail}
		wire, err := json.Marshal(original)
		if err != nil {
			t.Fatal(err)
		}
		var decoded redisapi.WSMessage
		if json.Unmarshal(wire, &decoded) != nil || decoded.Error != detail {
			t.Fatal("fixture changed original server frame detail")
		}
		// Invoke the same callback registered by Connect. No connection, DNS,
		// listener or server is created by this boundary fixture.
		source.onServerError(decoded)
		if len(levels) != 1 || levels[0] != "error" || len(logs) != 1 || !strings.Contains(logs[0], "WebSocket server error") {
			t.Fatal("server rejection stopped being observable at error level")
		}
		if strings.Contains(logs[0], secret) {
			t.Fatal("pre-kind WS server-error callback retained private input detail")
		}
		if decoded.Error != detail {
			t.Fatal("logging mutated the original wire carrier")
		}
	}
}
