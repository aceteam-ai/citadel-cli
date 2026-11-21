package heartbeat

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/status"
)

func TestCapacityHeartbeatTransports(t *testing.T) {
	const fixture = `{"load1":0,"load5":35,"load15":12.5,"ncpu":8,"psi_cpu_some_avg60":0,"psi_io_some_avg60":2,"psi_io_full_avg60":1,"psi_mem_some_avg60":3}`
	var expected map[string]float64
	if err := json.Unmarshal([]byte(fixture), &expected); err != nil {
		t.Fatal(err)
	}
	for _, observed := range []bool{false, true} {
		for _, transport := range []string{"api", "redis"} {
			t.Run(transport+map[bool]string{false: "-unknown", true: "-observed"}[observed], func(t *testing.T) {
				msg := testStatusMessage()
				msg.Status.System = status.SystemMetrics{}
				if observed {
					if err := json.Unmarshal([]byte(fixture), &msg.Status.System); err != nil {
						t.Fatal(err)
					}
				}
				var raw []byte
				if transport == "api" {
					stub := &redisAPIStub{}
					pub, _ := newTestAPIPublisher(t, stub)
					if err := pub.publishMessage(context.Background(), msg, msg.Timestamp); err != nil {
						t.Fatal(err)
					}
					landed := stub.landed()
					if len(landed) != 1 {
						t.Fatal("durable API heartbeat missing")
					}
					var err error
					raw, err = json.Marshal(landed[0])
					if err != nil {
						t.Fatal(err)
					}
				} else {
					pub, mr, _ := newTestRedisPublisher(t)
					if err := pub.publishMessage(context.Background(), msg, ""); err != nil {
						t.Fatal(err)
					}
					entries := streamEntries(t, mr)
					if len(entries) != 1 {
						t.Fatal("durable direct heartbeat missing")
					}
					raw = []byte(entries[0]["payload"])
				}
				var wire struct {
					Status struct {
						System map[string]float64 `json:"system"`
					} `json:"status"`
				}
				if err := json.Unmarshal(raw, &wire); err != nil {
					t.Fatal(err)
				}
				for key, value := range expected {
					got, present := wire.Status.System[key]
					if present != observed || observed && got != value {
						t.Fatalf("%s lost value or unknown semantics", key)
					}
				}
			})
		}
	}
}
