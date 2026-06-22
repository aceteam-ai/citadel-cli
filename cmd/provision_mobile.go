// cmd/provision_mobile.go
//
// Operator-facing `citadel provision ios` and `citadel provision android`
// subcommands (#140 Phase 3). They prepare a fresh macOS node to run the
// IOS_BUILD / ANDROID_BUILD job handlers added in Phase 2.
//
// iOS provisioning configures the code-signing keychain, imports a distribution
// certificate, installs provisioning profiles, and checks for Xcode CLI tools.
// Android provisioning ensures the SDK location, accepts SDK licenses, and
// installs build components. All sensitive material (certs, passwords) is
// supplied by the operator as paths/params; nothing is hardcoded. The
// --dry-run flag prints every command without executing it, so the flow is
// exercisable without real certs or a system keychain.
package cmd

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/provision/mobile"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var (
	provisionDryRun bool

	provisionIOSKeychain     string
	provisionIOSKeychainPass string
	provisionIOSCertPath     string
	provisionIOSCertPass     string
	provisionIOSProfiles     []string
	provisionIOSProfilesDir  string

	provisionAndroidSDKRoot  string
	provisionAndroidLicenses bool
	provisionAndroidPackages []string
)

var provisionIOSCmd = &cobra.Command{
	Use:          "ios",
	Short:        "Prepare this macOS node to run iOS builds",
	SilenceUsage: true,
	Long: `Configure code-signing and tooling so this node can run IOS_BUILD jobs.

Steps performed:
  - Create/unlock a dedicated build keychain and set its partition list.
  - Import a distribution certificate (.p12) when --cert is given.
  - Install provisioning profiles into the user's Provisioning Profiles dir.
  - Verify Xcode command-line tools are installed (hint to install if missing).

Sensitive material is supplied as paths/params and never hardcoded. Use
--dry-run to print every command without touching the keychain.

Examples:
  # Print the plan without executing anything (no certs needed)
  citadel provision ios --dry-run \
    --keychain citadel-build.keychain-db --keychain-password "$KC_PW"

  # Real run: import a cert and install a profile
  citadel provision ios \
    --keychain citadel-build.keychain-db --keychain-password "$KC_PW" \
    --cert ./dist.p12 --cert-password "$CERT_PW" \
    --profile ./app.mobileprovision`,
	RunE: runProvisionIOS,
}

var provisionAndroidCmd = &cobra.Command{
	Use:          "android",
	Short:        "Prepare this node to run Android builds",
	SilenceUsage: true,
	Long: `Configure the Android SDK so this node can run ANDROID_BUILD jobs.

Steps performed:
  - Resolve the SDK location (--sdk-root, else ANDROID_HOME).
  - Accept SDK licenses via sdkmanager --licenses (with --accept-licenses).
  - Install requested SDK packages via sdkmanager (with --package).

Use --dry-run to print every command without executing it.

Examples:
  # Accept licenses and install build-tools/platform
  citadel provision android --accept-licenses \
    --package "platform-tools" --package "platforms;android-34" \
    --package "build-tools;34.0.0"

  # Print the plan only
  citadel provision android --dry-run --sdk-root ~/Library/Android/sdk --accept-licenses`,
	RunE: runProvisionAndroid,
}

func runProvisionIOS(cmd *cobra.Command, _ []string) error {
	if !provisionDryRun && !platform.IsDarwin() {
		return fmt.Errorf("provision ios requires macOS; this node is %s (use --dry-run to preview the plan anywhere)", platform.OS())
	}

	profilesDir := provisionIOSProfilesDir
	if profilesDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolving home directory: %w", err)
		}
		profilesDir = mobile.DefaultProfilesDir(home)
	}

	steps, err := mobile.BuildIOSSteps(mobile.IOSOptions{
		KeychainName:     provisionIOSKeychain,
		KeychainPassword: provisionIOSKeychainPass,
		CertPath:         provisionIOSCertPath,
		CertPassword:     provisionIOSCertPass,
		ProfilePaths:     provisionIOSProfiles,
		ProfilesDir:      profilesDir,
	})
	if err != nil {
		return err
	}

	runner := mobile.NewRunner(provisionDryRun, cmd.OutOrStdout())
	if err := runner.Run(steps); err != nil {
		return err
	}

	checkXcodeCLITools(cmd, provisionDryRun)

	if provisionDryRun {
		color.Yellow("Dry run complete; no changes were made.")
	} else {
		color.Green("iOS provisioning complete.")
	}
	return nil
}

func runProvisionAndroid(cmd *cobra.Command, _ []string) error {
	sdkRoot := provisionAndroidSDKRoot
	if sdkRoot == "" {
		sdkRoot = androidSDKFromEnv()
	}
	if sdkRoot == "" {
		return fmt.Errorf("Android SDK location not found: pass --sdk-root or set ANDROID_HOME")
	}

	if !provisionDryRun {
		if _, err := exec.LookPath("sdkmanager"); err != nil {
			return fmt.Errorf("sdkmanager not found on PATH; install the Android command-line tools, or use --dry-run to preview")
		}
	}

	steps, err := mobile.BuildAndroidSteps(mobile.AndroidOptions{
		SDKRoot:        sdkRoot,
		AcceptLicenses: provisionAndroidLicenses,
		Packages:       provisionAndroidPackages,
	})
	if err != nil {
		return err
	}

	runner := mobile.NewRunner(provisionDryRun, cmd.OutOrStdout())
	if err := runner.Run(steps); err != nil {
		return err
	}

	if provisionDryRun {
		color.Yellow("Dry run complete; no changes were made.")
	} else {
		color.Green("Android provisioning complete.")
	}
	return nil
}

// androidSDKFromEnv returns the SDK root from the standard environment
// variables, ANDROID_HOME taking precedence over the older ANDROID_SDK_ROOT.
func androidSDKFromEnv() string {
	if home := os.Getenv("ANDROID_HOME"); home != "" {
		return home
	}
	return os.Getenv("ANDROID_SDK_ROOT")
}

// checkXcodeCLITools reports whether the Xcode command-line tools are present
// and prints an install hint if not. In dry-run mode on a non-macOS host the
// check is skipped (xcode-select is unavailable there).
func checkXcodeCLITools(cmd *cobra.Command, dryRun bool) {
	out := cmd.OutOrStdout()
	if dryRun && !platform.IsDarwin() {
		fmt.Fprintln(out, "Skipping Xcode CLI tools check (not on macOS; dry-run).")
		return
	}
	if path, err := exec.Command("xcode-select", "-p").Output(); err == nil && len(path) > 0 {
		fmt.Fprintf(out, "Xcode command-line tools present at %s", string(path))
		return
	}
	color.Yellow("Xcode command-line tools not found.")
	fmt.Fprintln(out, "Install them with: xcode-select --install")
}

func init() {
	provisionCmd.AddCommand(provisionIOSCmd)
	provisionCmd.AddCommand(provisionAndroidCmd)

	provisionIOSCmd.Flags().BoolVar(&provisionDryRun, "dry-run", false, "Print the provisioning commands without executing them")
	provisionIOSCmd.Flags().StringVar(&provisionIOSKeychain, "keychain", "citadel-build.keychain-db", "Dedicated build keychain to create/unlock")
	provisionIOSCmd.Flags().StringVar(&provisionIOSKeychainPass, "keychain-password", "", "Password for the build keychain (required)")
	provisionIOSCmd.Flags().StringVar(&provisionIOSCertPath, "cert", "", "Path to a distribution certificate (.p12) to import")
	provisionIOSCmd.Flags().StringVar(&provisionIOSCertPass, "cert-password", "", "Export password for the .p12 certificate")
	provisionIOSCmd.Flags().StringSliceVar(&provisionIOSProfiles, "profile", nil, "Path to a .mobileprovision profile to install (repeatable)")
	provisionIOSCmd.Flags().StringVar(&provisionIOSProfilesDir, "profiles-dir", "", "Override the provisioning profiles install directory")

	provisionAndroidCmd.Flags().BoolVar(&provisionDryRun, "dry-run", false, "Print the provisioning commands without executing them")
	provisionAndroidCmd.Flags().StringVar(&provisionAndroidSDKRoot, "sdk-root", "", "Android SDK location (defaults to ANDROID_HOME)")
	provisionAndroidCmd.Flags().BoolVar(&provisionAndroidLicenses, "accept-licenses", false, "Accept Android SDK licenses via sdkmanager --licenses")
	provisionAndroidCmd.Flags().StringSliceVar(&provisionAndroidPackages, "package", nil, "SDK package to install, e.g. 'platform-tools' (repeatable)")
}
