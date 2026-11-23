# Co-browse-only appliance

To dedicate a machine to public-web co-browse, run as the account that owns its
Citadel configuration:

```sh
citadel appliance apply
```

The command reuses the setup page's co-browse-only posture writer. Console,
Desktop, Files, Shell, Services, SSH synchronization and provisioning are off;
the meeting capability is off because its browser has no public-only egress
guard. Co-browse remains available, restricted to public addresses. The existing
posture marker prevents remote configuration from granting surfaces again.
Files and Shell remain human-only toggles; this command never grants them.

The profile enables the existing automatic-update preference and saves one
browser slot by default. Existing explicit update opt-outs, development-build
policy and busy-session deferral still apply. Optional settings:

```sh
citadel appliance apply --browser-slots 2 --window-size 1280,720
```

Slots range from 1 to 64. Window dimensions range from 320 to 8192 pixels.
Omitting the window flag preserves an existing size. Repeating the same command
is idempotent and reports unchanged settings. Unrelated device configuration
and any passcode hash are retained. Malformed permissions or device YAML are
refused, not replaced with defaults. A later write failure leaves the restrictive
posture in place and can be retried.

The command does not enroll a machine, install software, start or restart a
worker, pull an image or launch a browser. An existing worker picks up the saved
slot/window settings on its next normal start. `CITADEL_COBROWSE_MAX_SESSIONS`
retains its existing slot override; an explicit `CITADEL_COBROWSE_RESOLUTION`
retains its virtual-display override. Nodes that never apply this profile keep
their existing defaults.

## Readiness

`citadel doctor` adds a co-browse readiness section:

- Chromium/Chrome discovery and, on Linux, Xvfb discovery.
- Host OS and architecture.
- Fontconfig queries for installed fonts, CJK coverage and emoji coverage.
- Linux namespace sysctl gates and an AppArmor restriction warning.
- The stored public-only egress posture and its setup-posture marker.

The new missing-browser and missing-Xvfb checks affect the exit code only when
the appliance profile is applied. A known namespace prohibition or broken
appliance egress posture also fails that profile's readiness. Missing font
coverage, an unavailable fontconfig tool or an unknown namespace policy warn.
Existing engine, service and resource checks retain their own exit-code rules.

These are read-only preflights, not proof that a browser can successfully create
its sandbox. In particular, a restrictive AppArmor policy needs operator
validation for the installed Chromium profile. Doctor never starts a browser,
namespace, display server, updater or container to make these checks.
