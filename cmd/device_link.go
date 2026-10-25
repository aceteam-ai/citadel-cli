package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	goversion "github.com/hashicorp/go-version"
	"gopkg.in/yaml.v3"
)

// deviceLinkEnforcedFrom is the first aceteam release whose link route creates
// or adopts the device configuration (aceteam#11743). An older server answers a
// grant approved without the onboarding wizard with 404, so its refusal says
// nothing about this enrollment and must not fail it.
var deviceLinkEnforcedFrom = goversion.Must(goversion.NewVersion("1.520.0"))

// enrollmentRollbackFn removes the device credentials this enrollment saved
// before linking, so a fatal refusal does not leave a half-enrolled node that
// the next `citadel init` treats as configured. A seam so tests never touch
// the machine's real config.
var enrollmentRollbackFn = func(token *nexus.TokenResponse) error {
	return rollbackEnrollmentCredentials(filepath.Join(nodeConfigDirFn(), "config.yaml"), token)
}

// deviceLinkRetryDelay spaces the single retry after a transport error or 5xx.
var deviceLinkRetryDelay = 2 * time.Second

// deviceLinkWarnFn reports an unverified link that was not fatal.
var deviceLinkWarnFn = func(msg string) { fmt.Fprintln(os.Stderr, msg) }

// deviceLinker is the slice of the device-auth client enrollment linking uses.
type deviceLinker interface {
	LinkRegisteredNode(ctx context.Context, deviceCode, nodeID string) error
	ServerVersion(ctx context.Context) (string, error)
}

// linkDeviceEnrollment uses only the grant just approved and the identity the
// network actually registered. Never fall back to saved credentials/hostname/IP:
// a stale reservation or a previously connected node must fail at the server.
func linkDeviceEnrollment(result *DeviceAuthResult) error {
	return linkDeviceEnrollmentGated(result, enrollmentNodeID, nexus.NewDeviceAuthClient(authServiceURL))
}

// registeredNodeID holds the Headscale node ID that this process's most recent
// connectToNetwork registration returned. connectToNetwork disconnects before
// it returns so tsnet flushes its state, and after that the global server can
// no longer report the ID; without this, init and enroll never linked.
var registeredNodeID struct {
	sync.Mutex
	id string
}

func setRegisteredNodeID(id string) {
	registeredNodeID.Lock()
	defer registeredNodeID.Unlock()
	registeredNodeID.id = id
}

// enrollmentNodeID is the identity the network actually registered: the live
// connection's node ID, else the one this process's registration just
// returned. Never a saved credential, hostname or IP.
func enrollmentNodeID(ctx context.Context) string {
	if id := network.GetGlobalNodeID(ctx); id != "" {
		return id
	}
	registeredNodeID.Lock()
	defer registeredNodeID.Unlock()
	return registeredNodeID.id
}

// linkDeviceEnrollmentGated links the enrollment and decides whether a failure
// is fatal. A server reporting v1.520.0 or later must confirm the link before
// enrollment reports success. Transport errors and 5xx are retried once; if
// confirmation remains unavailable, credentials are retained because the server
// may already have committed the link. Only a definite refusal with no earlier
// uncertain attempt removes this enrollment's saved credentials. A retry's
// refusal cannot disprove an earlier commit. Older or unknown versions retain the
// compatibility path, with an explicit unverified-link recovery warning. No
// grant is persisted or replayed for later recovery.
func linkDeviceEnrollmentGated(result *DeviceAuthResult, nodeID func(context.Context) string, client deviceLinker) error {
	attempt := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return linkDeviceEnrollmentWith(ctx, result, nodeID, client.LinkRegisteredNode)
	}
	linkErr := attempt()
	uncertainAttempt := linkErr != nil && !isDeviceLinkRefusal(linkErr)
	if uncertainAttempt {
		time.Sleep(deviceLinkRetryDelay)
		linkErr = attempt()
	}
	if linkErr == nil {
		return nil
	}
	// A fresh budget: a slow link attempt must not starve the version read.
	vctx, vcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer vcancel()
	version, versionErr := client.ServerVersion(vctx)
	if versionErr == nil {
		enforced, ok := deviceLinkEnforced(version)
		if ok && enforced {
			fatal := deviceEnrollmentRecoveryError(linkErr)
			if !uncertainAttempt && isDeviceLinkRefusal(linkErr) && result != nil {
				if err := enrollmentRollbackFn(result.Token); err != nil {
					return fmt.Errorf("%w (removing this enrollment's saved credentials also failed)", fatal)
				}
			}
			return fatal
		}
		if ok {
			deviceLinkWarnFn("⚠️  Device link is unverified on this older server; continuing for compatibility. Saved credentials do not prove completed device binding. Run 'citadel logout && citadel init' with fresh browser approval to register again.")
			return nil
		}
	}
	deviceLinkWarnFn("⚠️  Device link is unverified and the server version could not be verified; continuing for compatibility. Saved credentials do not prove completed device binding. Run 'citadel logout && citadel init' with fresh browser approval to register again.")
	return nil
}

func isDeviceLinkRefusal(err error) bool {
	var refused *nexus.DeviceLinkRefusedError
	return errors.As(err, &refused)
}

// rollbackEnrollmentCredentials undoes saveDeviceConfigToFile /
// saveRedisURLToConfig for one enrollment, in the node config file only. It
// acts only when the file still holds this enrollment's own credential (proof
// that its save landed and nothing replaced it), and removes only the fields
// whose current values this enrollment wrote. Operator and node keys
// (aceteam_api_key, login_node_uid, nexus_url, node_config_dir, ...) and the
// legacy config location are never touched. Values the save itself overwrote
// cannot be restored; that is the save-before-link order #1350 tracks.
func rollbackEnrollmentCredentials(configFile string, token *nexus.TokenResponse) error {
	if token == nil {
		return nil
	}
	return withNodePointerLock(configFile, func() error {
		data, err := os.ReadFile(configFile)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		var config map[string]interface{}
		if err := yaml.Unmarshal(data, &config); err != nil || config == nil {
			return nil // never rewrite what cannot be read
		}
		holds := func(key, want string) bool {
			got, ok := config[key].(string)
			return ok && want != "" && got == want
		}
		var remove []string
		switch {
		case holds("device_api_token", token.DeviceAPIToken):
			remove = append(remove, "device_api_token")
			for key, want := range map[string]string{
				"api_base_url":   token.APIBaseURL,
				"org_id":         token.OrgID,
				"org_name":       token.OrgName,
				"user_email":     token.UserEmail,
				"user_name":      token.UserName,
				"fabric_node_id": token.FabricNodeID,
			} {
				if holds(key, want) {
					remove = append(remove, key)
				}
			}
		case token.DeviceAPIToken == "" && holds("redis_url", token.RedisURL):
			remove = append(remove, "redis_url")
		default:
			return nil
		}
		for _, key := range remove {
			delete(config, key)
		}
		if len(config) == 0 {
			return os.Remove(configFile)
		}
		out, err := yaml.Marshal(config)
		if err != nil {
			return err
		}
		return writeConfigFileAtomic(configFile, out)
	})
}

// writeConfigFileAtomic replaces path with data (mode 0600) via a temp file in
// the same directory and a rename, so a crash never leaves a truncated file.
func writeConfigFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config.yaml.tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// deviceLinkEnforced reports whether the server version is at or above
// deviceLinkEnforcedFrom. ok is false when the version does not parse.
func deviceLinkEnforced(version string) (enforced, ok bool) {
	v, err := goversion.NewVersion(strings.TrimPrefix(strings.TrimSpace(version), "v"))
	if err != nil {
		return false, false
	}
	return v.GreaterThanOrEqual(deviceLinkEnforcedFrom), true
}

// deviceEnrollmentRecoveryFailure keeps a cause available to typed callers
// without rendering arbitrary transport, server, or grant-bearing error text.
type deviceEnrollmentRecoveryFailure struct{ cause error }

func (*deviceEnrollmentRecoveryFailure) Error() string {
	return "device enrollment link was not confirmed; saved credentials do not complete device binding. Run 'citadel logout && citadel init' with fresh browser approval to register again"
}

func (e *deviceEnrollmentRecoveryFailure) Unwrap() error { return e.cause }

func deviceEnrollmentRecoveryError(err error) error {
	return &deviceEnrollmentRecoveryFailure{cause: err}
}

func linkDeviceEnrollmentWith(ctx context.Context, result *DeviceAuthResult, nodeID func(context.Context) string, link func(context.Context, string, string) error) error {
	if result == nil || result.Token == nil || result.DeviceCode == "" || result.Token.Authkey == "" {
		return fmt.Errorf("device enrollment cannot be linked without its current authorization")
	}
	id := nodeID(ctx)
	if id == "" {
		return fmt.Errorf("device enrollment cannot be linked before network registration")
	}
	return link(ctx, result.DeviceCode, id)
}

// ccConnectDeviceEnrollmentWith keeps the modal's current grant across network
// registration. A refused link disconnects and reaches the UI as an error,
// preventing its success callback from starting a worker for an unlinked grant.
func ccConnectDeviceEnrollmentWith(token *nexus.TokenResponse, deviceCode string, connect func() error, link func(*DeviceAuthResult) error, disconnect func() error) error {
	if token == nil || token.Authkey == "" || deviceCode == "" {
		return deviceEnrollmentRecoveryError(fmt.Errorf("current device authorization is missing"))
	}
	current := *token
	// A prior enroll in this process is not this modal's current registration.
	// Reset at the injected connect boundary so ordering is directly testable.
	setRegisteredNodeID("")
	if err := connect(); err != nil {
		return err
	}
	if err := link(&DeviceAuthResult{DeviceCode: deviceCode, Token: &current}); err != nil {
		_ = disconnect()
		return err
	}
	return nil
}
