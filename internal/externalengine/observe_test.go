package externalengine

import (
	"context"
	"errors"
	"testing"

	fabricpb "github.com/aceteam-ai/fabric-protocol/gen/go/aceteam/fabric/v1"
)

func TestObservedExternalStateRequiresFreshExactModel(t *testing.T) {
	c, err := Validate(Config{Version: 1, Mode: "adopted", Endpoint: Endpoint{Host: "127.0.0.1", Port: 58000}, Model: "vendor/model", Revision: "4", RequestID: "00000000-0000-4000-8000-000000000004", NodeID: "12"}, true)
	if err != nil {
		t.Fatal(err)
	}
	good := observedModule(context.Background(), c, func(_ context.Context, e Endpoint, m string) error {
		if e != c.Endpoint || m != c.Model {
			t.Fatalf("wrong probe target %v %q", e, m)
		}
		return nil
	})
	if good.Status != fabricpb.ModuleStatus_MODULE_STATUS_RUNNING || good.Health != fabricpb.ModuleHealth_MODULE_HEALTH_HEALTHY || good.ConfigRef != c.ConfigRef || good.InstalledVersion != c.Model {
		t.Fatalf("healthy state: %+v", good)
	}
	bad := observedModule(context.Background(), c, func(context.Context, Endpoint, string) error { return errors.New("model absent") })
	if bad.Health != fabricpb.ModuleHealth_MODULE_HEALTH_UNHEALTHY || bad.InstalledVersion != "" || bad.ConfigRef != c.ConfigRef {
		t.Fatalf("unhealthy state: %+v", bad)
	}
	c.Mode = "detached"
	c.ConfigRef = Ref(c)
	detached := observedModule(context.Background(), c, func(context.Context, Endpoint, string) error { t.Fatal("detached probed"); return nil })
	if detached.Status != fabricpb.ModuleStatus_MODULE_STATUS_STOPPED || detached.Health != fabricpb.ModuleHealth_MODULE_HEALTH_HEALTHY || detached.ConfigRef != c.ConfigRef {
		t.Fatalf("detached state: %+v", detached)
	}
}
