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

## Linux helper staging source

The same sidecar staging script also maps the exact `x86_64-unknown-linux-gnu`
and `aarch64-unknown-linux-gnu` targets to Go `linux/amd64` and `linux/arm64`.
The two macOS target mappings remain supported. With no argument, it uses the
Rust host tuple; explicit empty, unsupported or extra arguments refuse. The
helper path remains `src-tauri/binaries/citadel-<target-triple>`, with no output
override. A checked, nonempty Git version is embedded before reporting success.

This is the first #618 staging source slice in the shared Tauri codebase, not
a Linux application/package readiness claim. It adds no Linux launcher,
installer or service behavior. Debian/AppImage packaging, actual Tauri/native
builds, Secret Service availability and credential behavior, service supervision,
installation and both architecture acceptance remain gated. Existing Cargo
Secret Service features alone do not establish native acceptance.

`scripts/stage-sidecar-test.sh` prepares the actual script in a disposable fake
repository with recording Go/Git/Rust shims; it requires no real compiler,
Tauri or Mac tool. The existing DMG source test suite invokes it. These fixtures
check source command construction and failure paths; they do not prove a native
binary or distribution artifact. No local candidate script/test/build execution
accompanied this source change.

## Windows helper staging source

The shared script accepts exactly `x86_64-pc-windows-msvc` for the first #619
build-input slice, selecting Go `windows/amd64` and the fixed output
`src-tauri/binaries/citadel-x86_64-pc-windows-msvc.exe`. This follows Tauri's
[target-suffixed external binary naming](https://v2.tauri.app/develop/sidecar/),
including the Windows `.exe` extension. Darwin and Linux output names stay
suffixless. Other Windows targets, including GNU, 32-bit and ARM64, refuse.

The existing inert fixture suite covers explicit and default-host Windows
selection, exact Go argv/environment/output naming, rejected inputs and
provenance/build/chmod failures. It runs the shared Bash stager with fake tools
on Unix; it does not prove Windows shell compatibility, a real PE binary or
native Tauri bundling. This slice builds no Windows app or installer and adds
no new workflow, runtime, permission, authentication or service behavior.

Windows native build and onboarding/credential acceptance, user-level
supervision without elevation, signed installer/winget packaging and
distribution remain open under #619. The existing Administrator/LocalSystem
SCM service path is not the accepted user-level v1 supervisor. Do not infer
full Windows support from a staged helper or from generic CLI cross-compiles.

## Build an unsigned DMG

The distribution entrypoint accepts one exact macOS target at a time. It stages the matching Go helper, asks Tauri to create the DMG, and verifies that both the desktop executable and bundled helper are thin Mach-O binaries for the requested architecture. It also requires exactly one nonempty DMG in the target-specific Tauri output directory.

```sh
cd desktop
npm ci
./scripts/build-macos-dmg.sh aarch64-apple-darwin
./scripts/build-macos-dmg.sh x86_64-apple-darwin
```

From the repository root, `./build-dmg.sh` is a compatibility dispatcher to that exact Tauri builder. With no arguments it selects the host architecture; `--arch` accepts `arm64`/`aarch64` or `amd64`/`x86_64` aliases and the two canonical target triples. The retired `--binary` and `--version` options fail closed because Tauri owns the bundled helper and application version.

The two artifacts remain separate. Universal binaries and mixed-architecture bundles are rejected. Both entrypoints intentionally pass `--no-sign`; their output is not distributable until the Developer ID signing, Apple notarization, stapling, native installer acceptance, and publishing gates are implemented and accepted. There is no legacy Terminal-launcher packaging path.

## Native acceptance gate

Pull requests run the frontend build on a hosted runner. Before this workflow lands on the default branch, GitHub cannot dispatch its manual native job. A maintainer must review the exact pull request head and run `desktop/scripts/native-head-gate.sh <PR number> <full head SHA>` on a trusted Mac with no signing identities or production credentials. The script checks the current same-repository PR head, builds an isolated checkout at that exact SHA, and leaves the unsigned bundle for inspection. After the workflow lands on the default branch, maintainers may dispatch `Citadel desktop macOS` with the PR number and full head SHA. Configure its `desktop-macos-native` environment with required reviewers.

Before accepting a build for distribution, install and open the app from a DMG, eject the DMG, move the app, and launch it under Gatekeeper App Translocation. On each path, enroll a test node and confirm launchd starts from the Application Support helper path. On an app upgrade, open the app without repeating setup and confirm launchd repoints to the new helper and restarts once, while the prior helper and enrollment identity remain intact. Confirm a second app open does not restart the already-current service, and that a manual launchd job is never adopted. Also check cancellation, timeout, and app quit during setup; none should leave a setup child running. This manual macOS gate is required until those platform behaviors are exercised in native automation.

The repository-root `build-dmg.sh` builds only through the reviewed Tauri entrypoint. The desktop app remains an unsigned, non-distributable artifact until its native macOS build, onboarding, installer lifecycle, Developer ID, notarization, stapling, and publishing checks pass. Do not distribute an unsigned debug bundle as the production app.
