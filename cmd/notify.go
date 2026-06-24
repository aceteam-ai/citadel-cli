// cmd/notify.go
// Send a live notification from this Citadel node to the org's phone(s).
//
// The node authenticates with its device API token (org-scoped, like every
// other node→backend call) and POSTs a notification to the AceTeam backend,
// which relays it to the org user's registered iOS device(s) via APNs. This
// powers the "agent on this machine wants your attention / HITL approval" flow
// (aceteam-ai/aceteam#4144, #4145).
package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/notify"
	"github.com/spf13/cobra"
)

var (
	notifyTitle          string
	notifyBody           string
	notifyTarget         string
	notifyConversationID string
)

var notifyCmd = &cobra.Command{
	Use:   "notify",
	Short: "Send a live notification to your org's phone(s)",
	Long: `Send a push notification from this node to the org user's registered
iOS device(s), relayed by the AceTeam backend via APNs.

The node authenticates with its device API token; the org is derived
server-side from that token (no org flag needed). Use this for the
"agent on this machine wants your attention / HITL approval" flow.

Requires a device API token (stored during 'citadel login' or 'citadel init').

Note: actual push delivery depends on the paid Apple enrollment (APNs)
clearing on the backend. Until then the backend accepts and queues the
notification but no phone will display it. This command reports
"accepted by backend", which is the most a node can observe.`,
	Example: `  # Notify the org that an agent needs attention, deep-linking to a chat
  citadel notify --title "Approval needed" \
    --body "Agent on this machine wants to deploy" \
    --target chat --conversation 123e4567-e89b-12d3-a456-426614174000

  # Minimal notification
  citadel notify -t "Heads up" -b "Long-running job finished"`,
	Run: func(cmd *cobra.Command, args []string) {
		os.Exit(runNotify())
	},
}

// runNotify loads device auth, sends the notification, and returns a process
// exit code (0 = accepted by backend, 1 = error).
func runNotify() int {
	deviceConfig := getDeviceConfigFromFile()
	if deviceConfig == nil || deviceConfig.DeviceAPIToken == "" {
		fmt.Fprintln(os.Stderr, "Error: No device API token found.")
		fmt.Fprintln(os.Stderr, "Run 'citadel login' or 'citadel init' to authenticate first.")
		return 1
	}

	apiBaseURL := deviceConfig.APIBaseURL
	if apiBaseURL == "" {
		apiBaseURL = authServiceURL
	}

	client := notify.NewClient(notify.Config{
		BaseURL: apiBaseURL,
		Token:   deviceConfig.DeviceAPIToken,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	res, err := client.Send(ctx, notify.Notification{
		Title:          notifyTitle,
		Body:           notifyBody,
		Target:         notify.Target(notifyTarget),
		ConversationID: notifyConversationID,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	// A 2xx means the backend accepted/queued the push. The node cannot observe
	// whether a phone displayed it — and real APNs delivery is gated on the
	// paid Apple enrollment clearing on the backend (aceteam#4219).
	fmt.Printf("Notification accepted by backend (HTTP %d).\n", res.StatusCode)
	fmt.Println("Delivery to the phone depends on Apple APNs enrollment clearing on the backend.")
	return 0
}

func init() {
	rootCmd.AddCommand(notifyCmd)
	notifyCmd.Flags().StringVarP(&notifyTitle, "title", "t", "", "Notification title (required)")
	notifyCmd.Flags().StringVarP(&notifyBody, "body", "b", "", "Notification body (required)")
	notifyCmd.Flags().StringVar(&notifyTarget, "target", "chat", "Deep-link surface: chat|nodes|terminal|settings")
	notifyCmd.Flags().StringVar(&notifyConversationID, "conversation", "", "Conversation ID to deep-link into (optional)")
	_ = notifyCmd.MarkFlagRequired("title")
	_ = notifyCmd.MarkFlagRequired("body")
}
