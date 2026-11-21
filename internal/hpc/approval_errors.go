package hpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxApprovalErrorBytes = 240

// approvalResponseError displays only the structured error, never the raw body.
// The poll secret is redacted before truncation, including if the server echoes
// it in the message. A create response can also supply a poll_secret to redact.
func approvalResponseError(fallback string, data []byte, pollSecret string) error {
	var detail struct {
		Error      string `json:"error"`
		Code       string `json:"code"`
		Limit      *int64 `json:"limit"`
		Current    *int64 `json:"current"`
		PollSecret string `json:"poll_secret"`
	}
	message := ""
	if len(data) <= maxResponseBytes && json.Unmarshal(data, &detail) == nil {
		switch detail.Code {
		case "fabric_node_limit":
			message = "fabric node limit reached"
			if detail.Limit != nil && detail.Current != nil && *detail.Limit >= 0 && *detail.Current >= 0 {
				message += fmt.Sprintf(" (%d of %d)", *detail.Current, *detail.Limit)
			}
		case "approver_not_manager":
			message = "approver is no longer an organization manager"
		default:
			message = detail.Error
		}
	}
	message = normalizeApprovalError(message)
	if message != "" {
		fallback += ": " + message
	}
	fallback = normalizeApprovalError(fallback)
	for _, secret := range []string{pollSecret, detail.PollSecret} {
		// Normalize both sides so stripping a control character cannot restore
		// a previously split secret, then redact before applying the output cap.
		if secret = normalizeApprovalError(secret); secret != "" {
			fallback = strings.ReplaceAll(fallback, secret, "[redacted]")
		}
	}
	if len(fallback) > maxApprovalErrorBytes {
		end := maxApprovalErrorBytes - len("...")
		for !utf8.RuneStart(fallback[end]) {
			end--
		}
		fallback = fallback[:end] + "..."
	}
	return errors.New(fallback)
}

func normalizeApprovalError(message string) string {
	message = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsSpace(r):
			return ' '
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r):
			return -1
		default:
			return r
		}
	}, message)
	return strings.Join(strings.Fields(message), " ")
}
