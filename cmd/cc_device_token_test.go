package cmd

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// These helpers inject only synchronous, inert fetch/save/continuation calls.
// They do not call the real token endpoint, config writer, or network connect.
func TestControlCenterStartDeadlineUsesRequestEntry(t *testing.T) {
	started := time.Unix(1000, 0)
	at := started
	clockCalls := 0
	cfg, err := ccStartDeviceAuthFlowWith(func() (*nexus.DeviceCodeResponse, error) {
		at = at.Add(90 * time.Second)
		return &nexus.DeviceCodeResponse{
			DeviceCode: "grant", UserCode: "code", VerificationURI: "https://example.invalid/device",
			ExpiresIn: 30, Interval: 5,
		}, nil
	}, func() time.Time { clockCalls++; return at })
	if err != nil || cfg == nil {
		t.Fatal("valid start response was refused")
	}
	if clockCalls != 1 || !cfg.ExpiresAt.Equal(started.Add(30*time.Second)) || !cfg.ExpiresAt.Before(at) {
		t.Fatal("slow start response extended the grant's original deadline")
	}
	if cfg.DeviceCode != "grant" || cfg.UserCode != "code" || cfg.ExpiresIn != 30 || cfg.Interval != 5 {
		t.Fatal("start response lost its original grant fields")
	}
}

func TestControlCenterStartRefusesInvalidExpiry(t *testing.T) {
	now := func() time.Time { return time.Unix(1000, 0) }
	for _, resp := range []*nexus.DeviceCodeResponse{nil, {}, {ExpiresIn: -1}} {
		cfg, err := ccStartDeviceAuthFlowWith(func() (*nexus.DeviceCodeResponse, error) { return resp, nil },
			now)
		if cfg != nil || err == nil {
			t.Fatal("invalid grant expiry reached the UI")
		}
	}
	maxInt := int(^uint(0) >> 1)
	if int64(maxInt) > int64(time.Duration(1<<63-1)/time.Second) {
		cfg, err := ccStartDeviceAuthFlowWith(func() (*nexus.DeviceCodeResponse, error) {
			return &nexus.DeviceCodeResponse{ExpiresIn: maxInt}, nil
		}, now)
		if cfg != nil || err == nil {
			t.Fatal("overflowing grant expiry was accepted")
		}
	}
	refused := errors.New("fixture start refusal")
	if cfg, err := ccStartDeviceAuthFlowWith(func() (*nexus.DeviceCodeResponse, error) { return nil, refused }, now); cfg != nil || !errors.Is(err, refused) {
		t.Fatal("start failure lost its provenance")
	}
}

func ccFixtureDeviceToken() *nexus.TokenResponse {
	return &nexus.TokenResponse{
		Authkey: "fixture-registration", ExpiresIn: 999999, NexusURL: "https://nexus.invalid",
		OrgID: "fixture-org", OrgName: "Fixture Org", RedisURL: "redis://fixture.invalid",
		DeviceAPIToken: "fixture-bearer", APIBaseURL: "https://api.invalid", UserEmail: "fixture@example.invalid",
		UserName: "Fixture User", FabricNodeID: "123", LeafPem: "fixture-leaf", ChainPem: "fixture-chain", NodeUID: "fixture-node",
	}
}

func TestControlCenterPollOnlyReturnsOwnedResponse(t *testing.T) {
	const grant = "fixture-fetch-only"
	t.Cleanup(func() { ccEndDeviceAuthFlow(grant) })
	token := ccFixtureDeviceToken()
	want := *token
	calls := 0
	got, err := ccPollDeviceAuthTokenWith(grant, func(code string) (*nexus.TokenResponse, error) {
		calls++
		if code != grant {
			t.Fatal("fetch changed the current grant")
		}
		return token, nil
	})
	if err != nil || calls != 1 || got == token || got == nil || *got != want {
		t.Fatal("fetch did not return an owned, complete current response")
	}
	token.DeviceAPIToken = "replacement"
	if *got != want {
		t.Fatal("fetch result aliases the endpoint's response")
	}
}

func TestControlCenterPollPreservesPendingAndEndFlowCleanup(t *testing.T) {
	const grant = "fixture-retry-cleanup"
	t.Cleanup(func() { ccEndDeviceAuthFlow(grant) })
	for _, token := range []*nexus.TokenResponse{nil, {}, {DeviceAPIToken: "bearer-without-registration"}} {
		got, err := ccPollDeviceAuthTokenWith(grant, func(string) (*nexus.TokenResponse, error) { return token, nil })
		var pending *nexus.TokenError
		if got != nil || !errors.As(err, &pending) || pending == nil || pending.ErrorCode != "authorization_pending" {
			t.Fatal("missing registration key lost the typed pending response")
		}
	}
	got, err := ccPollDeviceAuthTokenWith(grant, func(string) (*nexus.TokenResponse, error) {
		return nil, &nexus.TokenServerError{StatusCode: 503}
	})
	var pending *nexus.TokenError
	if got != nil || !errors.As(err, &pending) || pending == nil || pending.ErrorCode != "authorization_pending" {
		t.Fatal("transient server response lost bounded retry authority")
	}
	if count, ok := ccTokenServerErrors.Load(grant); !ok || count != 1 {
		t.Fatal("fixture did not establish a retry entry before EndFlow")
	}
	// EndFlow is called only after the synchronous fetch above returns.
	ccEndDeviceAuthFlow(grant)
	ccEndDeviceAuthFlow(grant)
	if _, ok := ccTokenServerErrors.Load(grant); ok {
		t.Fatal("finished modal retained a retry entry")
	}
	refused := &nexus.TokenError{ErrorCode: "access_denied"}
	if got, err := ccPollDeviceAuthTokenWith(grant, func(string) (*nexus.TokenResponse, error) { return nil, refused }); got != nil || err != refused {
		t.Fatal("terminal token response changed or fabricated a token")
	}
}

func TestControlCenterAcceptSavesBeforeRegistrationAndKeepsExactResponse(t *testing.T) {
	for _, saveFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "saved", true: "save failure remains nonfatal"}[saveFails], func(t *testing.T) {
			token := ccFixtureDeviceToken()
			want := *token
			var events []string
			connectErr := errors.New("fixture registration refusal")
			err := ccAcceptDeviceTokenWith(token, "current-grant", func(saved *nexus.TokenResponse) error {
				events = append(events, "save")
				if saved == token || *saved != want {
					t.Fatal("save did not receive the complete owned response")
				}
				// Neither a callback alias nor a caller mutation may substitute
				// another credential into this admitted registration/link.
				saved.Authkey = "saver-mutation"
				token.DeviceAPIToken = "caller-mutation"
				if saveFails {
					return errors.New("fixture write failure")
				}
				return nil
			}, func(current *nexus.TokenResponse, code string) error {
				events = append(events, "register")
				if current == token || *current != want || code != "current-grant" {
					t.Fatal("registration lost the exact admitted response")
				}
				return connectErr
			})
			if !errors.Is(err, connectErr) || !reflect.DeepEqual(events, []string{"save", "register"}) {
				t.Fatal("persistence ordering or nonfatal save policy changed")
			}
		})
	}
}

func TestControlCenterAcceptRetainsExistingConditionalPersistence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		token    nexus.TokenResponse
		wantSave bool
	}{
		{name: "registration only", token: nexus.TokenResponse{Authkey: "key"}},
		{name: "user metadata", token: nexus.TokenResponse{Authkey: "key", UserEmail: "user@example.invalid"}, wantSave: true},
		{name: "device bearer", token: nexus.TokenResponse{Authkey: "key", DeviceAPIToken: "bearer"}, wantSave: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saves, connects := 0, 0
			err := ccAcceptDeviceTokenWith(&tc.token, "grant", func(*nexus.TokenResponse) error { saves++; return nil },
				func(current *nexus.TokenResponse, code string) error {
					connects++
					if *current != tc.token || code != "grant" {
						t.Fatal("conditional save changed the response")
					}
					return nil
				})
			if err != nil || connects != 1 || (saves == 1) != tc.wantSave || saves > 1 {
				t.Fatal("conditional persistence changed")
			}
		})
	}
}

func TestControlCenterAcceptRefusesMissingAuthorityBeforeAnyEffect(t *testing.T) {
	for _, tc := range []struct {
		token *nexus.TokenResponse
		code  string
	}{
		{code: "grant"},
		{token: &nexus.TokenResponse{}, code: "grant"},
		{token: &nexus.TokenResponse{DeviceAPIToken: "saved-bearer"}, code: "grant"},
		{token: ccFixtureDeviceToken()},
	} {
		err := ccAcceptDeviceTokenWith(tc.token, tc.code, func(*nexus.TokenResponse) error {
			t.Fatal("invalid authorization reached persistence")
			return nil
		}, func(*nexus.TokenResponse, string) error {
			t.Fatal("invalid authorization reached registration")
			return nil
		})
		if err == nil {
			t.Fatal("missing current authorization was accepted")
		}
	}
	if err := ccAcceptDeviceTokenWith(ccFixtureDeviceToken(), "grant", func(*nexus.TokenResponse) error {
		t.Fatal("missing registration continuation must fail before saving")
		return nil
	}, nil); err == nil {
		t.Fatal("missing continuation was accepted")
	}
}
