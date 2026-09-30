package cmd

import (
	"context"
	"net"
	"os"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/ingress"
)

// fakeIngressRoutesSource is a RoutesSource that never touches the network, so
// ingressServe's happy path runs entirely under `go test`.
type fakeIngressRoutesSource struct{}

func (fakeIngressRoutesSource) Fetch(_ context.Context, _ string) (int, string, []byte, error) {
	return 200, "e1", []byte(`{"routes":{}}`), nil
}
func (fakeIngressRoutesSource) FetchOne(_ context.Context, _ string) (int, []byte, error) {
	return 404, nil, nil
}

func TestIngressServe_NotLoggedIn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 1)

	err := ingressServe(
		ctx, cancel,
		func() bool { return false }, // hasState: not logged in
		func(context.Context) (bool, error) { return false, nil },
		sigs,
		ingressOptions{}, // no authkey -> must fail fast with a clear error
	)
	if err == nil {
		t.Fatal("expected an error when the node is not logged in and no authkey is given")
	}
}

func TestIngressServe_StartThenShutdown(t *testing.T) {
	routes := ingress.NewClient(ingress.ClientConfig{Source: fakeIngressRoutesSource{}})
	opts := ingressOptions{
		appsDomain:  "apps.example.com",
		listenAddr:  "127.0.0.1:0",
		routes:      routes,
		cert:        &ingress.SelfSignedCertProvider{AppsDomain: "apps.example.com"},
		dialContext: nil,
		isConnected: func() bool { return true },
		listen:      func(string) (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") },
		logf:        func(string, ...any) {},
	}

	sigs := make(chan os.Signal, 1)
	sigs <- syscall.SIGTERM // pre-buffered: shut down as soon as we begin serving

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- ingressServe(
			ctx, cancel,
			func() bool { return true },                              // hasState
			func(context.Context) (bool, error) { return true, nil }, // verify -> connected (never ErrStaleState in tests)
			sigs,
			opts,
		)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ingressServe returned error on clean shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ingressServe did not shut down within 10s")
	}
}

func TestResolveIngressOptions_RequiresRoutesURLAndDomain(t *testing.T) {
	// No flags/env set: must error on the missing routes URL.
	t.Setenv("CITADEL_INGRESS_ROUTES_URL", "")
	t.Setenv("CITADEL_INGRESS_APPS_DOMAIN", "")
	if _, err := resolveIngressOptions(context.Background()); err == nil {
		t.Fatal("expected error when routes URL is missing")
	}

	// Routes URL present, domain missing.
	t.Setenv("CITADEL_INGRESS_ROUTES_URL", "https://cp.example.com/ingress/routes")
	if _, err := resolveIngressOptions(context.Background()); err == nil {
		t.Fatal("expected error when apps domain is missing")
	}

	// Both present, plus a mounted static cert -> resolves cleanly.
	t.Setenv("CITADEL_INGRESS_APPS_DOMAIN", "apps.example.com")
	t.Setenv("CITADEL_INGRESS_CERT_FILE", "/tmp/does-not-need-to-exist.crt")
	t.Setenv("CITADEL_INGRESS_KEY_FILE", "/tmp/does-not-need-to-exist.key")
	opts, err := resolveIngressOptions(context.Background())
	if err != nil {
		t.Fatalf("expected clean resolution: %v", err)
	}
	if opts.appsDomain != "apps.example.com" {
		t.Fatalf("appsDomain = %q", opts.appsDomain)
	}
	if _, ok := opts.cert.(*ingress.StaticFileCertProvider); !ok {
		t.Fatalf("expected StaticFileCertProvider, got %T", opts.cert)
	}
	if opts.exchanger == nil {
		t.Fatal("expected production credential exchanger wiring")
	}
	if want := []string{ingress.AppCredentialCookieName, ingress.AppNonceCookieName}; !reflect.DeepEqual(opts.cookieNames, want) {
		t.Fatalf("default cookie names = %q, want %q", opts.cookieNames, want)
	}
}

func TestIngressSessionCookieNames(t *testing.T) {
	defaults := []string{ingress.AppCredentialCookieName, ingress.AppNonceCookieName}
	tests := []struct {
		name  string
		value string
		want  []string
	}{
		{name: "unset keeps secure defaults", want: defaults},
		{name: "blank entries", value: " ,\t, ", want: defaults},
		{
			name:  "trim comma list and deduplicate",
			value: " custom_session, second ,custom_session ,, third ",
			want:  append(append([]string(nil), defaults...), "custom_session", "second", "third"),
		},
		{
			name:  "operator cannot remove or duplicate defaults",
			value: ingress.AppNonceCookieName + ", custom_session, " + ingress.AppCredentialCookieName,
			want:  append(append([]string(nil), defaults...), "custom_session"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ingressSessionCookieNames(tt.value); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ingressSessionCookieNames(%q) = %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}

func TestResolveIngressOptions_ParsesSessionCookieEnvironment(t *testing.T) {
	t.Setenv("CITADEL_INGRESS_ROUTES_URL", "https://cp.example.com/ingress/routes")
	t.Setenv("CITADEL_INGRESS_APPS_DOMAIN", "apps.example.com")
	t.Setenv("CITADEL_INGRESS_CERT_FILE", "/tmp/does-not-need-to-exist.crt")
	t.Setenv("CITADEL_INGRESS_KEY_FILE", "/tmp/does-not-need-to-exist.key")
	t.Setenv("CITADEL_INGRESS_SESSION_COOKIE", " custom_session, second, custom_session ")

	opts, err := resolveIngressOptions(context.Background())
	if err != nil {
		t.Fatalf("resolveIngressOptions() error: %v", err)
	}
	want := []string{ingress.AppCredentialCookieName, ingress.AppNonceCookieName, "custom_session", "second"}
	if !reflect.DeepEqual(opts.cookieNames, want) {
		t.Fatalf("cookieNames = %q, want %q", opts.cookieNames, want)
	}
}

func TestResolveIngressOptions_RefusesLargePollInterval(t *testing.T) {
	// resolveIngressOptions reads the ingressPollInterval flag var first; keep the
	// env-driven path hermetic regardless of test ordering.
	prevPoll := ingressPollInterval
	ingressPollInterval = 0
	t.Cleanup(func() { ingressPollInterval = prevPoll })

	t.Setenv("CITADEL_INGRESS_ROUTES_URL", "https://cp.example.com/ingress/routes")
	t.Setenv("CITADEL_INGRESS_APPS_DOMAIN", "apps.example.com")
	t.Setenv("CITADEL_INGRESS_CERT_FILE", "/tmp/does-not-need-to-exist.crt")
	t.Setenv("CITADEL_INGRESS_KEY_FILE", "/tmp/does-not-need-to-exist.key")

	// A poll interval at or above half the route max-age lets the map expire
	// between polls: refuse loudly rather than ship a silent config-dependent
	// outage.
	t.Setenv("CITADEL_INGRESS_POLL_INTERVAL", "60s")
	if _, err := resolveIngressOptions(context.Background()); err == nil {
		t.Fatal("expected an error for a poll interval >= RouteMaxAge/2")
	}

	// A small interval resolves cleanly.
	t.Setenv("CITADEL_INGRESS_POLL_INTERVAL", "5s")
	if _, err := resolveIngressOptions(context.Background()); err != nil {
		t.Fatalf("a small poll interval should resolve cleanly: %v", err)
	}
}
