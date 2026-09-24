# Citadel desktop

One Tauri codebase for the Citadel node app. The first build target is macOS. Windows and Linux use this same shell after #570 lands; their packaging and node-service details are tracked by #619 and #618.

This app uses the bundled `citadel` Go binary for all node work. The UI has fixed commands for `init`, `status --json`, service start/stop/status, and doctor. It does not expose a general shell. Hosted login follows the desktop PKCE and MFA contract in aceteam#10269 with the exact `citadel://auth/callback` route. Session tokens are stored in the operating system credential store, and only a signed-in or MFA summary reaches the web view.

## Build on a Mac

Install the Go toolchain from `go.mod`, Rust, Node, Xcode command line tools, and npm. From the repository root:

```sh
cd desktop
npm ci
./scripts/stage-sidecar.sh
npm run tauri -- build --bundles app --no-sign
```

The stage script builds the Go helper into `src-tauri/binaries/citadel-<target-triple>` as required by Tauri's `externalBin`. The artifact stays ignored by Git. For Intel macOS, pass `x86_64-apple-darwin` to the script and to `tauri build -- --target`.

The bundled helper is copied to a private, content-versioned directory under `~/Library/Application Support/ai.aceteam.citadel/helpers/`; launchd points at that copy. At every app start, the current helper is staged and an existing desktop-managed user LaunchAgent is repointed and restarted once if its helper changed. No enrollment is repeated. An unrelated or manually installed service is left untouched. Reconciliation failures are shown in the app; the node helper never replaces itself.

## Native acceptance gate

Pull requests run the frontend build on a hosted runner. Before this workflow lands on the default branch, GitHub cannot dispatch its manual native job. A maintainer must review the exact pull request head and run `desktop/scripts/native-head-gate.sh <PR number> <full head SHA>` on a trusted Mac with no signing identities or production credentials. The script checks the current same-repository PR head, builds an isolated checkout at that exact SHA, and leaves the unsigned bundle for inspection. After the workflow lands on the default branch, maintainers may dispatch `Citadel desktop macOS` with the PR number and full head SHA. Configure its `desktop-macos-native` environment with required reviewers.

Before accepting a build for distribution, install and open the app from a DMG, eject the DMG, move the app, and launch it under Gatekeeper App Translocation. On each path, enroll a test node and confirm launchd starts from the Application Support helper path. On an app upgrade, open the app without repeating setup and confirm launchd repoints to the new helper and restarts once, while the prior helper and enrollment identity remain intact. Confirm a second app open does not restart the already-current service, and that a manual launchd job is never adopted. Also check cancellation, timeout, and app quit during setup; none should leave a setup child running. This manual macOS gate is required until those platform behaviors are exercised in native automation.

The existing `build-dmg.sh` still builds the old command-line wrapper. The desktop app is a separate draft artifact until its macOS build, onboarding, and installer acceptance checks pass. Do not distribute an unsigned debug bundle as the production app.
