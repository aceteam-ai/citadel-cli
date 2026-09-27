package cmd

import (
	"context"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/gateway"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
	"github.com/aceteam-ai/citadel-cli/services"
)

// TestRefuseReservedAppExposeName pins the /agent/expose control-path guard
// (CRAM A1): an operator cannot expose or unexpose a reserved "app-<short_code>"
// route through the local control endpoint. This is the enforcement point the
// EXPOSE_SET/UNEXPOSE job parsers don't reach, so it needs its own pin.
func TestRefuseReservedAppExposeName(t *testing.T) {
	for _, n := range []string{"app-ac-blue-cat-fox", "app-x", "app-"} {
		if err := refuseReservedAppExposeName(n, "pick another name"); err == nil {
			t.Errorf("refuseReservedAppExposeName(%q) = nil, want an error", n)
		}
	}
	for _, n := range []string{"frigate", "grafana", "myapp", ""} {
		if err := refuseReservedAppExposeName(n, "pick another name"); err != nil {
			t.Errorf("refuseReservedAppExposeName(%q) = %v, want nil", n, err)
		}
	}
}

// TestAppTeardownRemovesDurableRecordWithNilGateway pins the Trigger-2 fix: an
// APP_DESTROY that runs while the worker is gateway-off must still remove the
// durable exposure record for the app's route. Unexpose alone cannot do that
// here (it returns "no in-process gateway", the deliberate UNEXPOSE-job retry
// signal, and tears nothing down), so teardownAppRoute must delete the record
// directly. A record that survives re-exposes on the next --gateway restart
// onto a host port a later deploy could reuse (the cross-visibility bypass).
func TestAppTeardownRemovesDurableRecordWithNilGateway(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir) // isolates platform.ConfigDir() to the temp dir
	setProvisionedServiceGateway(nil, 0, false, "", 0)
	t.Cleanup(func() { setProvisionedServiceGateway(nil, 0, false, "", 0) })

	routeName := gateway.AppExposePrefix + "ac-blue-cat-fox"
	if err := config.SaveExposure(platform.ConfigDir(), config.ExposureRecord{
		Name: routeName, Port: services.AppPodPortRangeStart, Visibility: "org",
	}); err != nil {
		t.Fatalf("seed durable app route record: %v", err)
	}

	teardownAppRoute(context.Background(), "ac-blue-cat-fox")

	if rec := config.FindExposure(platform.ConfigDir(), routeName); rec != nil {
		t.Errorf("durable app route record survived teardown with a nil gateway: %+v", rec)
	}
}

// TestAppTeardownRemovesLiveRouteAndRecordWithGateway is the positive control
// for the above: with a live in-process gateway, teardownAppRoute must drop
// BOTH the live route and the durable record (Unexpose does both; the direct
// backstop is a no-op on this path).
func TestAppTeardownRemovesLiveRouteAndRecordWithGateway(t *testing.T) {
	gw := setupExposeOpsTest(t) // wires an in-process gateway + isolates ConfigDir
	ctx := context.Background()

	routeName := gateway.AppExposePrefix + "ac-blue-cat-fox"
	if _, err := (liveExposeOps{}).Expose(ctx, worker.ExposeRequest{
		Name: routeName, Port: services.AppPodPortRangeStart, Visibility: "org",
	}); err != nil {
		t.Fatalf("expose app route: %v", err)
	}
	if !gw.HasExposure(routeName) {
		t.Fatal("test setup: gateway should have the live app route before teardown")
	}

	teardownAppRoute(ctx, "ac-blue-cat-fox")

	if gw.HasExposure(routeName) {
		t.Error("live gateway route survived teardown")
	}
	if rec := config.FindExposure(platform.ConfigDir(), routeName); rec != nil {
		t.Errorf("durable app route record survived teardown: %+v", rec)
	}
}

// TestExposedAppRoutePortsReservesDurableAppRoutePort pins the allocator-side
// fix (Trigger 1): a host port still claimed by a durable app- exposure record
// is treated as in-use, so the port allocator never hands it to a new pod. A
// non-app exposure inside the same range does NOT reserve a port here, and a
// path-only app share (Port 0) is ignored.
func TestExposedAppRoutePortsReservesDurableAppRoutePort(t *testing.T) {
	dir := t.TempDir() // exposedAppRoutePorts takes configDir as a param (pure)

	save := func(rec config.ExposureRecord) {
		t.Helper()
		if err := config.SaveExposure(dir, rec); err != nil {
			t.Fatalf("seed exposure %q: %v", rec.Name, err)
		}
	}
	save(config.ExposureRecord{Name: gateway.AppExposePrefix + "ac-blue-cat-fox", Port: services.AppPodPortRangeStart, Visibility: "org"})
	save(config.ExposureRecord{Name: "frigate", Port: services.AppPodPortRangeStart + 2, Visibility: "org"})
	save(config.ExposureRecord{Name: gateway.AppExposePrefix + "share-cat-dog-owl", Path: "/some/dir", Visibility: "org"})

	ports := exposedAppRoutePorts(dir)
	if !ports[services.AppPodPortRangeStart] {
		t.Errorf("app- route port %d should be reserved", services.AppPodPortRangeStart)
	}
	if ports[services.AppPodPortRangeStart+2] {
		t.Errorf("non-app exposure port %d must NOT be reserved as an app-route port", services.AppPodPortRangeStart+2)
	}
	if len(ports) != 1 {
		t.Errorf("exposedAppRoutePorts = %v, want only the one app- route port %d", ports, services.AppPodPortRangeStart)
	}

	// The allocator must skip the reserved app-route port and hand out the next.
	got, err := services.AllocateAppPodPort(func(p int) bool { return ports[p] })
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if got != services.AppPodPortRangeStart+1 {
		t.Errorf("allocator = %d, want %d (the reserved app- route port must be skipped)", got, services.AppPodPortRangeStart+1)
	}
}
