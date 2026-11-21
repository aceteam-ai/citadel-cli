//go:build linux

package containerjobs

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"
)

func TestContainerVolumeRootOwnerPrecedesDockerRegistration(t *testing.T) {
	for _, fail := range []string{"", "mount", "chown"} {
		t.Run("failure_"+fail, func(t *testing.T) {
			n := nativeProvisionFixture(t)
			r := baseRequest()
			r.QuotaBytes = 16 << 20
			_, image, mount, _ := n.volumePaths(r.VolumeRef)
			var calls []string
			created := false
			n.command = func(ctx context.Context, bin string, args ...string) (commandResult, error) {
				step := bin + ":" + args[0]
				if bin == "docker" {
					step += " " + args[1]
				}
				calls = append(calls, step)
				if bin == fail {
					return commandResult{}, errors.New("synthetic failure")
				}
				switch bin {
				case "mkfs.ext4":
					if args[len(args)-1] != image {
						t.Fatal("formatted an unexpected path")
					}
				case "mount":
					if args[len(args)-1] != mount {
						t.Fatal("mounted an unexpected path")
					}
				case "chown":
					if !reflect.DeepEqual(args, []string{"--no-dereference", "--", "10001:10001", mount}) {
						t.Fatal("ownership was not restricted to the pinned ace volume root", args)
					}
				case "docker":
					if args[0] != "volume" {
						t.Fatal("unexpected Docker effect")
					}
					if args[1] == "create" {
						created = true
					}
					if args[1] == "ls" && created {
						return commandResult{bytes: []byte(r.VolumeRef)}, nil
					}
					if args[1] == "inspect" {
						b, _ := json.Marshal([]any{map[string]any{"Driver": "local", "Options": map[string]string{"type": "none", "o": "bind", "device": mount}, "Labels": map[string]string{"aceteam.org_id": testOrg, "aceteam.quota_bytes": "16777216"}}})
						return commandResult{bytes: b}, nil
					}
				default:
					t.Fatal("unexpected native command", bin)
				}
				return commandResult{}, nil
			}
			err := n.CreateVolume(context.Background(), r)
			mounted := slices.Index(calls, "mount:-t")
			owned := slices.Index(calls, "chown:--no-dereference")
			registered := slices.Index(calls, "docker:volume create")
			if fail == "" {
				if err != nil || mounted < 0 || owned <= mounted || registered <= owned {
					t.Fatal("ownership was not established after mount and before volume registration", err, calls)
				}
			} else {
				if err == nil {
					t.Fatal("failed volume ownership reported success")
				}
				if registered >= 0 || fail == "mount" && owned >= 0 {
					t.Fatal("failed mount/ownership proceeded toward registration")
				}
			}
		})
	}
}

func TestContainerNativeInvalidPolicyRefusesBeforeAnyNativeCommand(t *testing.T) {
	for _, p := range []HostPolicy{{Runtime: "unreviewed"}, {EgressProxyURL: "http://user:secret@proxy.invalid:3128"}} {
		n := NewNativeWithPolicy(t.TempDir(), func() (HostPolicy, error) { return p, nil })
		n.command = func(context.Context, string, ...string) (commandResult, error) {
			t.Fatal("invalid node policy reached a native command")
			return commandResult{}, nil
		}
		if err := n.CreateContainer(context.Background(), baseRequest()); err != ErrInvalid {
			t.Fatal("invalid policy accepted", err)
		}
	}
	n := NewNativeWithPolicy(t.TempDir(), func() (HostPolicy, error) { return HostPolicy{}, errors.New("synthetic unreadable config") })
	n.command = func(context.Context, string, ...string) (commandResult, error) {
		t.Fatal("unreadable node policy reached a native command")
		return commandResult{}, nil
	}
	if err := n.CreateContainer(context.Background(), baseRequest()); err != ErrUnavailable {
		t.Fatal("unreadable policy accepted", err)
	}
}
