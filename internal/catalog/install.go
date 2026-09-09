package catalog

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ErrNotInstallable is returned by Install when a catalog service has no
// compose.yml. Such services are host-provisioned (e.g. the Windows-only
// "wechat" microservice) and are catalogued for discoverability only -- they
// cannot be installed/run as a container by the CLI. The cmd layer detects
// this with errors.Is and prints provisioning guidance instead of a crash.
var ErrNotInstallable = errors.New("service is not installable via the catalog (host-provisioned, no compose.yml)")

// IsInstallable reports whether a catalog service can be installed/run as a
// container by the CLI, i.e. whether it has a compose.yml. Host-provisioned
// services (e.g. the Windows-only "wechat" microservice) return false. The cmd
// layer uses this to print provisioning guidance before doing any work (such as
// scaffolding node config), rather than after attempting an install.
func IsInstallable(name string) bool {
	_, err := GetComposeFile(name)
	return err == nil
}

// InstallResult holds the artifacts produced by a catalog install so the caller
// (cmd layer) can register the service in the node manifest.
type InstallResult struct {
	// Name is the canonical service name.
	Name string
	// ComposeDestPath is the absolute path where compose.yml was written.
	ComposeDestPath string
	// EnvDestPath is the absolute path where the .env file was written, or empty.
	EnvDestPath string
	// Sandboxed is true when a least-privilege hardening override was generated
	// and written (untrusted/Tier-2 installs only). When true,
	// SandboxOverridePath points at the override file.
	Sandboxed bool
	// SandboxOverridePath is the absolute path of the <name>.sandbox.yml override,
	// or empty when not sandboxed.
	SandboxOverridePath string
}

// Install copies a catalog service's compose.yml (and optional .env) into the
// node's services directory. It checks requirements and port conflicts before
// copying. Manifest registration is the caller's responsibility (cmd layer).
//
// servicesDir is the absolute path to the node's services directory
// (e.g. ~/citadel-node/services). configOverrides are key=value pairs that
// override config defaults.
func Install(name string, servicesDir string, configOverrides map[string]string) (*InstallResult, error) {
	// Load service manifest from catalog.
	manifest, err := LoadServiceManifest(name)
	if err != nil {
		return nil, err
	}

	// Resolve the compose source. A service with no compose.yml (e.g. the
	// Windows-only "wechat" microservice) is host-provisioned and not
	// installable; pass an empty composeSrcPath so InstallFromManifest returns
	// ErrNotInstallable.
	composeSrc, _ := GetComposeFile(name)

	// Catalog services are first-party (Tier 0) and have no --allow-privileged
	// flag, so the privilege gate must not apply to them (it would be an
	// un-overridable failure). Pass allowPrivileged=true. They are trusted, so
	// untrusted=false: no sandbox hardening is applied. The module-source path
	// passes the real flag + trust values.
	return InstallFromManifest(manifest, composeSrc, servicesDir, configOverrides, true, true, false, false)
}

// InstallFromManifest installs a service from an already-loaded manifest and a
// compose source path. It is the shared core behind both the catalog install
// (Install) and external "module source" installs. It checks arch/GPU/port
// requirements, resolves config, copies the compose file, and writes an .env.
//
// interactive controls config resolution: when true, required config vars with
// no override and no default are prompted on os.Stdin; when false (the TUI
// path), such a var is a returned error and stdin is never read.
//
// allowPrivileged is the un-bypassable privilege gate: if the resolved compose
// contains any Critical risk (privileged mode, docker-socket mount, cap_add
// ALL/SYS_ADMIN) and allowPrivileged is false, the install is REFUSED regardless
// of interactive/--yes. This guard lives in the shared core so both the CLI and
// the TUI non-interactive path are protected identically. Catalog (Tier-0)
// installs pass allowPrivileged=true (they have no override flag).
//
// untrusted marks a Tier-2 (untrusted-source) install. When true, two extra
// containment layers run in the shared core so BOTH the CLI and the TUI
// non-interactive path are protected identically (mirroring the privilege gate):
//   - bind-mount confinement: a host bind-mount outside the per-module sandbox
//     data dir is REFUSED unless allowPrivileged is set.
//   - a least-privilege hardening override (<name>.sandbox.yml) is generated and
//     written next to the compose; the run path includes it automatically.
//
// Trusted (Tier 0/1) installs pass untrusted=false and are unaffected.
//
// skipHardening is the explicit --no-harden opt-out. When true, the hardening
// override is NOT generated even for an untrusted install -- but bind-mount
// confinement still applies. This keeps the two containment layers independent:
// opting out of the override does not silently disable bind-mount confinement
// (the operator must use --allow-privileged for that). skipHardening is ignored
// for trusted installs (which never generate an override anyway).
//
// An empty composeSrcPath means the service is host-provisioned (no container)
// and InstallFromManifest returns ErrNotInstallable.
func InstallFromManifest(manifest *ServiceManifest, composeSrcPath, servicesDir string, configOverrides map[string]string, interactive, allowPrivileged, untrusted, skipHardening bool) (*InstallResult, error) {
	name := manifest.Name

	// 1. Reject host-provisioned services up front (no compose.yml).
	if composeSrcPath == "" {
		return nil, ErrNotInstallable
	}

	// 2. Check architecture compatibility.
	if !CheckArchCompatible(manifest.Requires.Arch) {
		return nil, fmt.Errorf("service '%s' requires architecture %v, but this machine is %s",
			name, manifest.Requires.Arch, runtime.GOARCH)
	}

	// 3. Check GPU requirements.
	if manifest.Requires.GPU {
		hasGPU, vramGB, err := CheckGPU()
		if err != nil {
			return nil, fmt.Errorf("failed to check GPU: %w", err)
		}
		if !hasGPU {
			return nil, fmt.Errorf("service '%s' requires a GPU, but none was detected", name)
		}
		if manifest.Requires.VRAMMinGB > 0 && vramGB < manifest.Requires.VRAMMinGB {
			return nil, fmt.Errorf("service '%s' requires %.1f GB VRAM, but only %.1f GB available",
				name, manifest.Requires.VRAMMinGB, vramGB)
		}
	}

	// 4. Check port conflicts.
	var conflicts []int
	for _, p := range manifest.Ports {
		if CheckPortConflict(p.Host) {
			conflicts = append(conflicts, p.Host)
		}
	}
	if len(conflicts) > 0 {
		return nil, fmt.Errorf("port conflict: port(s) %v already in use", conflicts)
	}

	// 4b/4c. Read the resolved compose once for the container-name collision
	// check and the privilege gate. A read failure here is fatal when a gate
	// could apply: a security gate must not fail open. (The compose is copied
	// from this same path moments later, so a real read failure would fail the
	// install anyway -- we just refuse earlier and explicitly.)
	{
		data, rerr := os.ReadFile(composeSrcPath)
		if rerr != nil {
			if !allowPrivileged {
				return nil, fmt.Errorf("cannot read compose for '%s' to run the safety scan: %w", name, rerr)
			}
			return nil, fmt.Errorf("failed to read compose file for '%s': %w", name, rerr)
		}
		composeText := string(data)

		// 4b. Container-name collision. A reinstall of the same module is already
		// blocked upstream (by the manifest's hasService check), so any existing
		// container matching this compose's container_name is a foreign collision
		// -- refuse with a clear escape hatch. Best-effort (skipped if docker is
		// unavailable).
		if cn := ParseComposeContainerName(composeText); cn != "" && ContainerNameConflict(cn) {
			return nil, fmt.Errorf("container name '%s' is already in use by another container; "+
				"remove it (docker rm -f %s) or override the name via the module's compose before installing '%s'",
				cn, cn, name)
		}

		// 4c. Privilege gate (un-bypassable). Any Critical compose risk requires
		// an explicit opt-in; without it, refuse -- even under --yes. This is the
		// shared-core guard that protects the TUI non-interactive path too.
		if !allowPrivileged {
			if crit := criticalRisks(ScanComposeRisks(composeText)); len(crit) > 0 {
				return nil, fmt.Errorf("refusing to install '%s': its compose contains privileged/root-equivalent "+
					"directives (%s).\n   This grants the module Docker-level (host root) access on this node. "+
					"If you trust this source, re-run with --allow-privileged to override.",
					name, strings.Join(criticalDirectives(crit), ", "))
			}
		}

		// 4d. Bind-mount confinement (untrusted/Tier-2 only). Enforce the #342
		// risk-scan warning: an untrusted module may only bind-mount host paths
		// within its per-module sandbox data dir (<servicesDir>/<name>-data).
		// Anything outside is refused unless the operator opted into privileged
		// installs. Trusted (Tier 0/1) installs skip this entirely.
		if untrusted && !allowPrivileged {
			if v := BindMountViolations(composeText, servicesDir, name); len(v) > 0 {
				return nil, fmt.Errorf("refusing to install untrusted module '%s': its compose bind-mounts host "+
					"path(s) outside the module sandbox dir (%s): %s.\n   "+
					"Untrusted modules may only mount within their sandbox data dir. "+
					"If you trust this source, re-run with --allow-privileged to override.",
					name, SandboxDataDir(servicesDir, name), strings.Join(v, ", "))
			}
		}
	}

	// 5. Resolve config values (prompt for required ones without defaults only
	//    when interactive). Any already-persisted values are passed in so a
	//    `generate:` secret is minted ONCE and then reused: reconciles, module
	//    updates and re-runs must not rotate a credential the module's running
	//    containers were started with.
	envDest := filepath.Join(servicesDir, name+".env")
	configValues, err := resolveConfig(manifest.Config, configOverrides, readEnvFile(envDest), interactive)
	if err != nil {
		return nil, err
	}

	// 6. Copy compose.yml to services directory.
	if err := os.MkdirAll(servicesDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create services directory: %w", err)
	}

	composeDest := filepath.Join(servicesDir, name+".yml")
	if err := copyFile(composeSrcPath, composeDest); err != nil {
		return nil, fmt.Errorf("failed to copy compose file: %w", err)
	}

	result := &InstallResult{
		Name:            name,
		ComposeDestPath: composeDest,
	}

	// 6b. Least-privilege sandbox (untrusted/Tier-2 only, unless --no-harden).
	// Generate a hardening override from the manifest's declared needs and write
	// it next to the compose as <name>.sandbox.yml; the run path includes it
	// automatically when present. Also create the per-module sandbox data dir so
	// the one allowed bind-mount root exists. Trusted (Tier 0/1) installs skip all
	// of this; skipHardening opts an untrusted install out of the override only
	// (bind-mount confinement above still ran).
	if untrusted && !skipHardening {
		baseData, rerr := os.ReadFile(composeDest)
		if rerr != nil {
			return nil, fmt.Errorf("failed to read copied compose for sandbox hardening: %w", rerr)
		}
		// Detect real host GPU presence to gate the per-service GPU exemption
		// (#377). This MUST be a fresh CheckGPU() here, not the step-3 result:
		// step 3 only runs when manifest.Requires.GPU is set, but the exemption is
		// driven by author-controlled compose GPU signals a module can declare with
		// Requires.GPU=false. CheckGPU() reports hasGPU=false on any detection error
		// (nvidia-smi missing/failing), which is the fail-safe direction (harden),
		// so dropping the error here is intentional -- unlike step 3, this is not a
		// hard requirement, just an exemption gate.
		hostHasGPU, _, _ := CheckGPU()
		override, gerr := GenerateHardeningOverride(string(baseData), manifest, hostHasGPU)
		if gerr != nil {
			return nil, fmt.Errorf("failed to generate sandbox override for '%s': %w", name, gerr)
		}
		overridePath := SandboxOverridePath(servicesDir, name)
		if werr := os.WriteFile(overridePath, []byte(override), 0600); werr != nil {
			return nil, fmt.Errorf("failed to write sandbox override: %w", werr)
		}
		// Best-effort: create the per-module sandbox data dir (the only host path
		// an untrusted module is allowed to bind-mount). A failure here is not
		// fatal -- the module may not use a bind mount at all.
		_ = os.MkdirAll(SandboxDataDir(servicesDir, name), 0755)
		result.Sandboxed = true
		result.SandboxOverridePath = overridePath
	}

	// 7. Write .env file if there are config values.
	if len(configValues) > 0 {
		if err := writeEnvFile(envDest, configValues); err != nil {
			return nil, fmt.Errorf("failed to write env file: %w", err)
		}
		result.EnvDestPath = envDest
	}

	return result, nil
}

// resolveConfig merges overrides with defaults. When interactive is true it
// prompts the user (os.Stdin) for any required config vars that have no default
// and no override. When interactive is false, such a var is a returned error and
// stdin is never read (the TUI path collects all config up front as overrides).
//
// existing holds the values already persisted in the module's .env (nil on a
// first install). It is consulted ONLY for `generate:` vars, so that a minted
// secret survives reconciles/updates; every other var still resolves purely from
// the override/default/prompt chain, keeping the operator's input authoritative.
func resolveConfig(configVars []ConfigVar, overrides, existing map[string]string, interactive bool) (map[string]string, error) {
	values := make(map[string]string)

	for _, cv := range configVars {
		// Check override first.
		if v, ok := overrides[cv.Name]; ok {
			values[cv.Name] = v
			continue
		}

		// A generated OR carry var reuses whatever a previous install persisted, so
		// the credential a running container holds is never rotated underneath it
		// (citadel#624 sub-collision 2). This reuse is what makes carry survive the
		// update-in-place delete-then-reinstall AND the FIRST module install on a
		// node whose bespoke stack already wrote the .env (the migration path,
		// where CarryGeneratedConfig never ran because the service was not yet in
		// the manifest). The difference between the two kinds is only what happens
		// when nothing is persisted: a generate var mints a fresh value; a carry
		// var (minted out of band, e.g. by the bridge admin API) cannot, so it is
		// simply left unset.
		if cv.Generate != "" || cv.Carry {
			if v := existing[cv.Name]; v != "" {
				values[cv.Name] = v
				continue
			}
			if cv.Generate != "" {
				v, err := generateConfigValue(cv.Generate)
				if err != nil {
					return nil, fmt.Errorf("config '%s': %w", cv.Name, err)
				}
				values[cv.Name] = v
				continue
			}
			// A carry var with nothing persisted cannot be minted here (it comes
			// from out of band); fall through so a default/required/prompt still
			// applies if the manifest declares one.
		}

		// Use default if available.
		if cv.Default != "" {
			values[cv.Name] = cv.Default
			continue
		}

		// Required without default.
		if cv.Required {
			// Non-interactive (TUI) path: never read stdin. The caller must
			// supply every required value as an override.
			if !interactive {
				return nil, fmt.Errorf("required config '%s' has no value (provide it via --set %s=...)", cv.Name, cv.Name)
			}

			// Interactive: prompt the user.
			fmt.Printf("  %s", cv.Name)
			if cv.Description != "" {
				fmt.Printf(" (%s)", cv.Description)
			}
			fmt.Print(": ")

			scanner := bufio.NewScanner(os.Stdin)
			if !scanner.Scan() {
				return nil, fmt.Errorf("aborted: no value provided for required config '%s'", cv.Name)
			}
			val := strings.TrimSpace(scanner.Text())
			if val == "" {
				return nil, fmt.Errorf("required config '%s' cannot be empty", cv.Name)
			}
			values[cv.Name] = val
		}
	}

	return values, nil
}

// CarryGeneratedConfig returns assigned augmented with any `generate:` OR
// `carry:` values already persisted in the module's .env, for callers that TEAR
// DOWN a module before re-installing it.
//
// The fabric MODULE_SET update-in-place path uninstalls before it installs, and
// uninstall deletes <name>.env -- so by the time InstallFromManifest reads the
// persisted values they are gone, and the node would mint a NEW secret on every
// re-assignment. That is not merely churn: compose recreates only the containers
// whose env changed (the broker), leaving the consumer running with the OLD
// credential in memory until someone restarts it by hand.
//
// `carry:` vars (citadel#624 sub-collision 2) are the same delete-then-restore
// concern for a credential the node CANNOT re-mint at all (it is minted out of
// band -- e.g. the WhatsApp bridge's TENANT_* keys, issued by the bridge admin
// API). Dropping one on update-in-place would rotate the platform-stored
// credential to nothing, the 401 class #624 exists to kill; there is no
// fallback like generate's mint, so carrying it forward is the only preservation
// path.
//
// Call this BEFORE the teardown. Values the caller already supplies win, so an
// operator can still rotate a credential by assigning one explicitly.
func CarryGeneratedConfig(manifest *ServiceManifest, servicesDir string, assigned map[string]string) map[string]string {
	if manifest == nil {
		return assigned
	}
	persisted := readEnvFile(filepath.Join(servicesDir, manifest.Name+".env"))
	if len(persisted) == 0 {
		return assigned
	}

	merged := make(map[string]string, len(assigned)+len(manifest.Config))
	for k, v := range assigned {
		merged[k] = v
	}
	for _, cv := range manifest.Config {
		if cv.Generate == "" && !cv.Carry {
			continue
		}
		if _, ok := merged[cv.Name]; ok {
			continue
		}
		if v := persisted[cv.Name]; v != "" {
			merged[cv.Name] = v
		}
	}
	return merged
}

// generateConfigValue mints a value for a ConfigVar declaring `generate:`.
// An unknown kind is an error, not a silent empty string: a typo in a manifest
// must not hand a module a blank credential.
func generateConfigValue(kind string) (string, error) {
	switch kind {
	case GenerateSecret:
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("generate secret: %w", err)
		}
		// URL-safe, unpadded: the value is interpolated into compose env and
		// shell-quoted entrypoints, so it must contain no '=', quote or shell
		// metacharacter.
		return base64.RawURLEncoding.EncodeToString(buf), nil
	default:
		return "", fmt.Errorf("unknown generate kind %q (only %q is supported)", kind, GenerateSecret)
	}
}

// readEnvFile parses a previously written module .env into a map. A missing or
// unreadable file yields an empty map -- callers use it to REUSE prior values,
// so "nothing persisted yet" and "cannot read it" both correctly mean "mint a
// fresh one" rather than failing the install.
func readEnvFile(path string) map[string]string {
	values := make(map[string]string)
	data, err := os.ReadFile(path)
	if err != nil {
		return values
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		values[strings.TrimSpace(k)] = v
	}
	return values
}

// copyFile copies src to dst, preserving content but using 0600 permissions.
func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0600)
}

// writeEnvFile writes key=value pairs to a file, one per line.
func writeEnvFile(path string, values map[string]string) error {
	var lines []string
	for k, v := range values {
		lines = append(lines, fmt.Sprintf("%s=%s", k, v))
	}
	content := strings.Join(lines, "\n") + "\n"
	return os.WriteFile(path, []byte(content), 0600)
}
