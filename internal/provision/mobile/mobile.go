// Package mobile implements operator-facing provisioning steps that prepare a
// fresh macOS node to run iOS and Android builds (citadel provision ios|android).
//
// The package is deliberately split into two layers:
//
//   - Pure builders (BuildIOSSteps, BuildAndroidSteps) that turn user-supplied
//     options into an ordered list of Step values describing the shell commands
//     and filesystem operations to perform. These are platform-agnostic and have
//     no side effects, so they are exhaustively unit-tested.
//   - A Runner that executes (or, in dry-run mode, merely prints) those steps.
//
// Real keychain/cert/profile operations require Apple secrets and modify the
// system keychain; the builders never embed secrets and accept all sensitive
// material as caller-supplied paths/identities. Dry-run mode lets the whole
// flow be exercised without certs or a real keychain.
package mobile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// StepKind distinguishes the two side-effecting operations a Step can describe.
type StepKind int

const (
	// StepExec runs an external command (Name + Args).
	StepExec StepKind = iota
	// StepCopyFile copies SrcPath to DstPath, creating DstPath's parent dir.
	StepCopyFile
)

// Step is a single provisioning operation. It is intentionally data-only so the
// builders can be tested without executing anything.
type Step struct {
	Kind StepKind
	// Desc is a human-readable description printed before the step runs.
	Desc string
	// Name and Args apply to StepExec.
	Name string
	Args []string
	// SecretArgs lists indices into Args whose values are sensitive (e.g. a
	// keychain or certificate password) and must be redacted in dry-run output.
	SecretArgs []int
	// SrcPath and DstPath apply to StepCopyFile.
	SrcPath string
	DstPath string
}

// CommandString renders an exec step as a copy-pasteable shell command. Args at
// SecretArgs indices are replaced with <redacted>. Only meaningful for
// StepExec steps.
func (s Step) CommandString() string {
	secret := make(map[int]bool, len(s.SecretArgs))
	for _, i := range s.SecretArgs {
		secret[i] = true
	}
	parts := make([]string, 0, len(s.Args)+1)
	parts = append(parts, s.Name)
	for i, a := range s.Args {
		if secret[i] {
			parts = append(parts, "<redacted>")
			continue
		}
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

// shellQuote wraps an argument in single quotes when it contains characters
// that a shell would interpret, so dry-run output is copy-pasteable.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.IndexFunc(s, func(r rune) bool {
		switch r {
		case ' ', '\t', '"', '\'', '$', '`', '\\', '&', '|', ';', '<', '>', '(', ')', '*', '?':
			return true
		}
		return false
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// IOSOptions configures iOS provisioning. All sensitive material is supplied by
// the operator as paths/identities; nothing is hardcoded.
type IOSOptions struct {
	// KeychainName is the dedicated build keychain to create/configure
	// (e.g. "citadel-build.keychain-db"). Required.
	KeychainName string
	// KeychainPassword unlocks the build keychain. Required to create/unlock.
	KeychainPassword string
	// CertPath is a .p12 distribution certificate to import. Optional; when
	// empty, no certificate import step is emitted.
	CertPath string
	// CertPassword is the .p12 export password. Used only when CertPath is set.
	CertPassword string
	// ProfilePaths are .mobileprovision files to install into the user's
	// Provisioning Profiles directory. Optional.
	ProfilePaths []string
	// ProfilesDir overrides the install destination directory. When empty it
	// defaults to ~/Library/MobileDevice/Provisioning Profiles. Injectable for
	// tests.
	ProfilesDir string
}

// AndroidOptions configures Android provisioning.
type AndroidOptions struct {
	// SDKRoot is the Android SDK location (ANDROID_HOME). Required.
	SDKRoot string
	// AcceptLicenses emits the `sdkmanager --licenses` step.
	AcceptLicenses bool
	// Packages are sdkmanager package specs to install
	// (e.g. "platform-tools", "platforms;android-34"). Optional.
	Packages []string
}

// DefaultProfilesDir returns the standard macOS install location for
// provisioning profiles, rooted at the supplied home directory.
func DefaultProfilesDir(home string) string {
	return filepath.Join(home, "Library", "MobileDevice", "Provisioning Profiles")
}

// securityBin is the macOS keychain management binary.
const securityBin = "security"

// BuildIOSSteps turns IOSOptions into an ordered provisioning plan. It performs
// validation of required fields and existence checks for supplied paths, but it
// does not execute anything. Returns an error for invalid input.
//
// The emitted plan, in order:
//  1. create-keychain (if not asserted to exist)
//  2. unlock-keychain
//  3. set keychain settings (no auto-lock during build sessions)
//  4. import distribution certificate (when CertPath is set)
//  5. set-key-partition-list (allow codesign to use the imported key)
//  6. install each provisioning profile (copy into ProfilesDir)
func BuildIOSSteps(opts IOSOptions) ([]Step, error) {
	if strings.TrimSpace(opts.KeychainName) == "" {
		return nil, fmt.Errorf("keychain name is required (--keychain)")
	}
	if strings.TrimSpace(opts.KeychainPassword) == "" {
		return nil, fmt.Errorf("keychain password is required (--keychain-password)")
	}
	if opts.CertPath != "" {
		if err := mustExist(opts.CertPath, "certificate"); err != nil {
			return nil, err
		}
		if strings.TrimSpace(opts.CertPassword) == "" {
			return nil, fmt.Errorf("certificate password is required when importing a certificate (--cert-password)")
		}
	}
	for _, p := range opts.ProfilePaths {
		if err := mustExist(p, "provisioning profile"); err != nil {
			return nil, err
		}
		if !strings.EqualFold(filepath.Ext(p), ".mobileprovision") {
			return nil, fmt.Errorf("provisioning profile %q must have a .mobileprovision extension", p)
		}
	}

	kc := opts.KeychainName
	pw := opts.KeychainPassword

	steps := []Step{
		{
			Kind:       StepExec,
			Desc:       fmt.Sprintf("Create build keychain %q (ignored if it already exists)", kc),
			Name:       securityBin,
			Args:       []string{"create-keychain", "-p", pw, kc},
			SecretArgs: []int{2}, // password
		},
		{
			Kind:       StepExec,
			Desc:       fmt.Sprintf("Unlock build keychain %q", kc),
			Name:       securityBin,
			Args:       []string{"unlock-keychain", "-p", pw, kc},
			SecretArgs: []int{2}, // password
		},
		{
			Kind: StepExec,
			Desc: "Disable keychain auto-lock for build sessions",
			Name: securityBin,
			Args: []string{"set-keychain-settings", "-lut", "21600", kc},
		},
	}

	if opts.CertPath != "" {
		steps = append(steps, Step{
			Kind:       StepExec,
			Desc:       fmt.Sprintf("Import distribution certificate %q", opts.CertPath),
			Name:       securityBin,
			Args:       []string{"import", opts.CertPath, "-k", kc, "-P", opts.CertPassword, "-T", "/usr/bin/codesign", "-T", "/usr/bin/productsign"},
			SecretArgs: []int{5}, // .p12 password
		})
		steps = append(steps, Step{
			Kind:       StepExec,
			Desc:       "Allow codesign to use the imported key without prompting (partition list)",
			Name:       securityBin,
			Args:       []string{"set-key-partition-list", "-S", "apple-tool:,apple:,codesign:", "-s", "-k", pw, kc},
			SecretArgs: []int{5}, // keychain password
		})
	}

	profilesDir := opts.ProfilesDir
	for _, p := range opts.ProfilePaths {
		dst := filepath.Join(profilesDir, filepath.Base(p))
		steps = append(steps, Step{
			Kind:    StepCopyFile,
			Desc:    fmt.Sprintf("Install provisioning profile %q", filepath.Base(p)),
			SrcPath: p,
			DstPath: dst,
		})
	}

	return steps, nil
}

// BuildAndroidSteps turns AndroidOptions into an ordered provisioning plan.
//
// The emitted plan, in order:
//  1. accept SDK licenses (when AcceptLicenses) — `sdkmanager --licenses`
//  2. install each requested package — `sdkmanager "<pkg>"`
//
// The SDK root is validated and threaded into every sdkmanager invocation via
// --sdk_root so the command is hermetic regardless of ANDROID_HOME.
func BuildAndroidSteps(opts AndroidOptions) ([]Step, error) {
	root := strings.TrimSpace(opts.SDKRoot)
	if root == "" {
		return nil, fmt.Errorf("Android SDK root is required (--sdk-root or ANDROID_HOME)")
	}

	rootFlag := "--sdk_root=" + root
	var steps []Step

	if opts.AcceptLicenses {
		steps = append(steps, Step{
			Kind: StepExec,
			Desc: "Accept Android SDK licenses",
			Name: "sdkmanager",
			Args: []string{rootFlag, "--licenses"},
		})
	}

	for _, pkg := range opts.Packages {
		pkg = strings.TrimSpace(pkg)
		if pkg == "" {
			continue
		}
		steps = append(steps, Step{
			Kind: StepExec,
			Desc: fmt.Sprintf("Install SDK package %q", pkg),
			Name: "sdkmanager",
			Args: []string{rootFlag, pkg},
		})
	}

	return steps, nil
}

// mustExist returns a clear error if path does not exist or is unreadable.
func mustExist(path, label string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("%s path is empty", label)
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s not found: %s", label, path)
		}
		return fmt.Errorf("cannot access %s %q: %w", label, path, err)
	}
	return nil
}
