# Citadel desktop

One Tauri codebase for the Citadel node app. The first build target is macOS. Windows and Linux use this same shell after #570 lands; their packaging and node-service details are tracked by #619 and #618.

This app uses the bundled `citadel` Go binary for all node work. The UI has fixed commands for `init`, `status --json`, service start/stop/status, and doctor. It does not expose a general shell. Hosted login uses the existing AceTeam one-time-code exchange and the exact `citadel://auth/callback` contract in aceteam#10268. Session tokens are stored in the operating system credential store, and only a signed-in summary reaches the web view.

## Build on a Mac

Install the Go toolchain from `go.mod`, Rust, Node, Xcode command line tools, and npm. From the repository root:

```sh
cd desktop
npm ci
./scripts/stage-sidecar.sh
npm run tauri build
```

The stage script builds the Go helper into `src-tauri/binaries/citadel-<target-triple>` as required by Tauri's `externalBin`. The artifact stays ignored by Git. For Intel macOS, pass `x86_64-apple-darwin` to the script and to `tauri build -- --target`.

The existing `build-dmg.sh` still builds the old command-line wrapper. The desktop app is a separate draft artifact until its macOS build, onboarding, and installer acceptance checks pass. Do not distribute an unsigned debug bundle as the production app.
