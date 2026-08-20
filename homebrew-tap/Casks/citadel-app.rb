# typed: false
# frozen_string_literal: true

# Homebrew cask for the Citadel.app macOS bundle (citadel#672/#670).
# Install with: brew install --cask aceteam-ai/tap/citadel-app
#
# Deliberately a separate token from the `citadel` CLI formula rather than
# reusing the same name for both: a formula and a cask sharing one token in
# the same tap is flagged by `brew audit` and forces every user to disambiguate
# with --formula/--cask. `citadel-app` sidesteps that outright.
#
# Kept in sync with the published release by scripts/update-homebrew-tap.sh
# (same automation that updates Formula/citadel.rb), reading the darwin DMG
# entries out of the release's checksums.txt. Auto-update via `brew upgrade
# --cask` is also the fix for citadel#672's other constraint: the CLI's
# in-place `citadel update` refuses to run from inside this bundle (see
# internal/update.ApplyUpdate / IsInsideAppBundle) because overwriting a file
# inside Citadel.app invalidates its code signature. A cask upgrade replaces
# the whole signed bundle instead, which is the only correct way to update it.
cask "citadel-app" do
  arch arm: "arm64", intel: "amd64"

  version "2.107.0"
  sha256 arm:   "0000000000000000000000000000000000000000000000000000000000000",
         intel: "0000000000000000000000000000000000000000000000000000000000000"

  url "https://github.com/aceteam-ai/citadel-cli/releases/download/v#{version}/citadel_v#{version}_darwin_#{arch}.dmg"
  name "Citadel"
  desc "AceTeam Sovereign Compute Fabric — macOS app bundle"
  homepage "https://aceteam.ai"

  # Matches packaging/macos/Info.plist's LSMinimumSystemVersion (raised to
  # 13.0 for this bundle only, in citadel#672 — SMAppService, the elevation
  # route citadel#670 uses, requires macOS 13. The `citadel` CLI formula has
  # no such floor.)
  depends_on macos: ">= :ventura"

  app "Citadel.app"

  # citadel#670 will turn this into a real background menu-bar app with
  # persistent state (login item, preferences, possibly a privileged helper).
  # Extend `uninstall`/`zap` here once that state exists; today's bundle is a
  # Terminal launcher with nothing of its own to clean up beyond the .app.
end
