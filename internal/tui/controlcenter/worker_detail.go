package controlcenter

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/rivo/tview"
)

// SharedWorkerOwnerChangedMessage contains no peer-controlled identity or error.
const SharedWorkerOwnerChangedMessage = "Worker owner changed; restart the Control Center monitor to observe the new worker."

func sharedWorkerStatusText(data StatusData, now time.Time) string {
	if data.SharedWorker && data.WorkerOwnerChanged {
		return "External worker (shared status)\n" + SharedWorkerOwnerChangedMessage + "\nConsumption, swaps and lanes unknown.\n"
	}
	return sharedWorkerDetailText(data.WorkerDetail, data.WorkerDetailObservedAt, now)
}

func renderSharedWorkerStatus(data StatusData, now time.Time) string {
	if data.SharedWorker && data.WorkerOwnerChanged {
		return tview.Escape(sharedWorkerStatusText(data, now))
	}
	return renderSharedWorkerDetail(data.WorkerDetail, data.WorkerDetailObservedAt, now)
}

// renderSharedWorkerDetail is a pure projection of an already observed owner.
// Quote peer text to literal ASCII, then escape the whole result for tview:
// terminal control bytes, bidi and styling markup never become instructions.
func renderSharedWorkerDetail(detail *status.WorkerDetail, receivedAt, now time.Time) string {
	return tview.Escape(sharedWorkerDetailText(detail, receivedAt, now))
}

func sharedWorkerDetailText(detail *status.WorkerDetail, receivedAt, now time.Time) string {
	var out strings.Builder
	out.WriteString("External worker (shared status)\n")
	if detail == nil || status.ValidateWorkerDetail(*detail) != nil {
		out.WriteString("Worker detail unavailable; consumption, swaps and lanes unknown.\n")
		return out.String()
	}
	fmt.Fprintf(&out, "Owner: PID %d, mode %s, version %s\n", detail.PID, strconv.QuoteToASCII(detail.Mode), strconv.QuoteToASCII(detail.Version))
	fmt.Fprintf(&out, "Node: %s, network IP %s, connected: %t\n", strconv.QuoteToASCII(detail.NodeName), strconv.QuoteToASCII(detail.MeshIP), detail.Connected)
	if receivedAt.IsZero() || now.Before(receivedAt) {
		out.WriteString("Receipt freshness: unknown\n")
	} else {
		fmt.Fprintf(&out, "Received: %s ago\n", now.Sub(receivedAt).Round(time.Second))
	}
	fmt.Fprintf(&out, "Worker observation: %s, uptime: %ds\n", detail.ObservedAt.UTC().Format(time.RFC3339Nano), detail.UptimeSec)
	if detail.Worker == nil {
		out.WriteString("Consumption: unknown\n")
	} else {
		worker := detail.Worker
		if worker.Consuming {
			out.WriteString("Consumption: polling recently\n")
		} else {
			out.WriteString("Consumption: not polling recently (not proof of stopped work)\n")
		}
		fmt.Fprintf(&out, "Last poll: %s; last consumed job: %s\n", sharedWorkerTime(worker.LastPollAt), sharedWorkerTime(worker.LastJobConsumedAt))
		fmt.Fprintf(&out, "In flight: %d; processed: %d; failed: %d\n", worker.InFlight, worker.Processed, worker.Failed)
		fmt.Fprintf(&out, "Identity unresolved: %t; pub/sub transport: %s\n", worker.IdentityUnresolved, sharedWorkerOptionalText(worker.PubSubTransport))
	}
	if detail.Swap == nil {
		out.WriteString("Swaps: unknown\n")
	} else {
		swap := detail.Swap
		fmt.Fprintf(&out, "Swaps/hour: %d; evicting/hour: %d; evicting limit: %d\n", swap.SwapsPerHour, swap.EvictingSwapsPerHour, swap.MaxEvictingPerHour)
		if len(swap.Recent) == 0 {
			out.WriteString("Recent swaps: none\n")
		}
		for index, record := range swap.Recent {
			pulled := "unknown"
			if record.Pulled != nil {
				pulled = strconv.FormatBool(*record.Pulled)
			}
			fmt.Fprintf(&out, "Swap %d: backend %s; model %s; outcome %s\n", index+1, strconv.QuoteToASCII(record.Backend), strconv.QuoteToASCII(record.Model), strconv.QuoteToASCII(record.Outcome))
			fmt.Fprintf(&out, "  Started: %s; wait: %s; Pulled: %s\n", record.StartedAt.UTC().Format(time.RFC3339Nano), record.Wait, pulled)
			out.WriteString("  Evicted:")
			if len(record.Evicted) == 0 {
				out.WriteString(" none")
			}
			for _, backend := range record.Evicted {
				fmt.Fprintf(&out, " %s", strconv.QuoteToASCII(backend))
			}
			out.WriteByte('\n')
		}
	}
	if detail.Lanes == nil {
		out.WriteString("Lanes: unknown\n")
	} else if len(detail.Lanes) == 0 {
		out.WriteString("Lanes: none\n")
	}
	for _, lane := range detail.Lanes {
		fmt.Fprintf(&out, "Lane %s: %d queued, %d executing, capacity %d; busy since %s\n", strconv.QuoteToASCII(lane.Lane), lane.Queued, lane.Executing, lane.ExecCapacity, sharedWorkerTime(lane.BusySince))
	}
	return out.String()
}

func sharedWorkerTime(value *time.Time) string {
	if value == nil {
		return "unknown"
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func sharedWorkerOptionalText(value string) string {
	if value == "" {
		return "unknown"
	}
	return strconv.QuoteToASCII(value)
}

func (cc *ControlCenter) updateSharedWorkerDetailModal() {
	if !cc.inModal || cc.sharedWorkerDetailView == nil {
		return
	}
	data := cc.data
	if !cc.data.SharedWorker {
		data.WorkerDetail, data.WorkerDetailObservedAt, data.WorkerOwnerChanged = nil, time.Time{}, false
	}
	cc.sharedWorkerDetailView.SetText(renderSharedWorkerStatus(data, time.Now()) + "\n[gray]Esc: close this monitor view[-]")
}
