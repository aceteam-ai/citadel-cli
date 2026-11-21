package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/status"
)

func resolveWorkerRole(dc *DeviceConfig, flag string) (config.WorkerRole, error) {
	value := config.WorkerRole(flag)
	if flag == "" && dc != nil {
		value = dc.Role
	}
	return config.ResolveWorkerRole(value)
}

// Control mode must resolve its enrolled identity before source construction:
// a nil queue list would activate the constructors' legacy shared defaults.
func controlWorkerIdentity(ctx context.Context, role config.WorkerRole, dc *DeviceConfig) (string, error) {
	if role != config.WorkerRoleControl {
		return "", nil
	}
	if dc == nil {
		return "", fmt.Errorf("control worker requires enrolled node configuration")
	}
	if dc.OrgID == "" {
		if manifest, _, err := findAndReadManifest(); err == nil && manifest != nil {
			dc.OrgID = manifest.Node.OrgID
		}
	}
	return controlWorkerIdentityFrom(ctx, dc.OrgID, network.GetGlobalNodeID, sleepCtx)
}

// Explicit I/O seams keep identity retry tests away from the live network.
func controlWorkerIdentityFrom(ctx context.Context, orgID string, lookup func(context.Context) string, pause func(context.Context, time.Duration) bool) (string, error) {
	if strings.TrimSpace(orgID) == "" {
		return "", fmt.Errorf("control worker requires an enrolled organization; refusing shared queue fallback")
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for attempt := 0; attempt < 5; attempt++ {
		if lookupCtx.Err() != nil {
			break
		}
		if id := lookup(lookupCtx); id != "" {
			if result := controlWorkerQueues(workerQueueParams{OrgID: orgID, NodeID: id}); result.Err != nil {
				return "", result.Err
			}
			return id, nil
		}
		if !pause(lookupCtx, time.Second) {
			break
		}
	}
	return "", fmt.Errorf("control worker node ID unavailable; refusing shared queue fallback")
}

func controlPinnedQueue(role config.WorkerRole, queues []string) string {
	if role == config.WorkerRoleControl && len(queues) == 1 {
		return queues[0]
	}
	return ""
}

func printConfiguredWorkerRole(w io.Writer, dc *DeviceConfig) {
	role, err := resolveWorkerRole(dc, "")
	if err != nil {
		fmt.Fprintln(w, "  Configured role:\tinvalid (worker refuses startup)")
		return
	}
	fmt.Fprintf(w, "  Configured role:\t%s (changes apply on worker restart)\n", role)
}

func probeActiveWorkerRole() (config.WorkerRole, bool) {
	port := resolveStatusPort()
	if port <= 0 {
		return "", false
	}
	client := &http.Client{Timeout: pubSubProbeCheapTimeout, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	return probeActiveWorkerRoleAt(client, fmt.Sprintf("http://127.0.0.1:%d/worker", port))
}

func probeActiveWorkerRoleAt(client *http.Client, url string) (config.WorkerRole, bool) {
	body, err := httpGetBodyErr(client, url)
	if err != nil {
		return "", false
	}
	var envelope status.WorkerEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Worker == nil {
		return "", false
	}
	role, err := config.ResolveWorkerRole(config.WorkerRole(envelope.Role))
	if err != nil {
		return "", false
	}
	return role, true
}

func printActiveWorkerRole(w io.Writer) {
	if role, known := probeActiveWorkerRole(); known {
		fmt.Fprintf(w, "  Active role:\t%s\n", role)
	} else {
		fmt.Fprintln(w, "  Active role:\tunknown (worker status unavailable)")
	}
}
