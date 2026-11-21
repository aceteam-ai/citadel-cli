package redis

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	goredis "github.com/redis/go-redis/v9"
)

func TestMediaNestedPayloadDecodeErrorPrivacy(t *testing.T) {
	const value = "1e9999"
	for _, kind := range []string{"", "MEDIA_GENERATE", "FILE_FETCH", "SHELL_COMMAND"} {
		t.Run("kind="+kind, func(t *testing.T) {
			job, err := (&Client{}).parseMessage(goredis.XMessage{ID: "1-0", Values: map[string]any{
				"type": kind, "payload": `{"type":"MEDIA_GENERATE","inputs":{"capability":` + value + `}}`,
			}})
			var original *json.UnmarshalTypeError
			if job != nil || err == nil || !errors.As(err, &original) || !strings.Contains(original.Error(), value) {
				t.Fatal("fixture did not reproduce original nested decoder value")
			}
			if kind == "SHELL_COMMAND" {
				if !strings.Contains(err.Error(), original.Error()) {
					t.Fatal("unrelated known-kind decode behavior changed")
				}
				return
			}
			if errors.Unwrap(err) != original {
				t.Fatal("nested decode error lost exact one-hop identity")
			}
			for _, retained := range []string{err.Error(), fmt.Sprintf("%#v", struct{ err error }{err})} {
				if strings.Contains(retained, value) {
					t.Fatal("private/pre-kind nested decoder retained input detail")
				}
			}
		})
	}
}
