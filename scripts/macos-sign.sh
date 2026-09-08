#!/bin/bash
# macos-sign.sh — shared codesign / notarize / staple helpers for macOS
# distributable bundles (citadel#672).
#
# Sourced (not executed) by build-dmg.sh and scripts/release.sh so both share
# one signing implementation instead of two. Every entry point degrades
# gracefully when credentials are absent:
#
#   - No CITADEL_CODESIGN_IDENTITY               -> skip signing
#   - Signed but no ASC_ISSUER_ID/ASC_KEY_ID/ASC_KEY_P8_BASE64 -> skip notarization
#
# ...UNLESS CITADEL_REQUIRE_SIGNING is truthy ("1"/"true"/"yes"/"on"), in which
# case a missing prerequisite is a hard failure. scripts/release.sh sets this
# on Darwin so a real release can never silently ship an unsigned bundle;
# build-dmg.sh run standalone by a developer leaves it unset so a local build
# without credentials still works (warn, don't block).
#
# Required env for signing:
#   CITADEL_CODESIGN_IDENTITY   Exact identity name from the login keychain,
#                                e.g. "Developer ID Application: NAME (TEAMID)".
#                                No default here on purpose — the identity name
#                                is a per-machine fact, not something to bake
#                                into a public repo's scripts.
#
# Required env for notarization (an App Store Connect API key; account-wide,
# so the same key covers notarytool regardless of which product built it):
#   ASC_ISSUER_ID
#   ASC_KEY_ID
#   ASC_KEY_P8_BASE64            Base64 of the .p8 private key file. Decoded to
#                                 a 0600 temp file for the duration of one
#                                 notarytool call and removed immediately after
#                                 (trap-guarded, also on error). Never written
#                                 to a predictable path, never logged.
#
# None of the above are read from or written to the repo. Populating them in
# CI (GitHub Actions secrets already hold the ASC key; a Developer ID .p12 in
# CI is a separate, human-run piece of wiring) and on the release machine is
# out of scope for this file.

# Not `set -euo pipefail` here: this file is *sourced* into callers that
# already set their own shell options, and a sourced script changing them out
# from under the caller is a surprise. Every function below is defensive
# instead (explicit checks, `${VAR:-}` everywhere).

msign_info()  { echo "[sign] $*"; }
msign_warn()  { echo "[sign] WARN: $*" >&2; }
msign_fatal() { echo "[sign] ERROR: $*" >&2; exit 1; }

# msign_truthy VALUE — mirrors the truthy set used elsewhere in this repo
# (internal/update.IsTruthy): 1/true/yes/on, case/space-insensitive.
msign_truthy() {
  case "$(echo "${1:-}" | tr '[:upper:]' '[:lower:]' | xargs)" in
    1|true|yes|on) return 0 ;;
    *) return 1 ;;
  esac
}

msign_require_signing() { msign_truthy "${CITADEL_REQUIRE_SIGNING:-}"; }

msign_signing_configured() { [[ -n "${CITADEL_CODESIGN_IDENTITY:-}" ]]; }

msign_notarization_configured() {
  [[ -n "${ASC_ISSUER_ID:-}" && -n "${ASC_KEY_ID:-}" && -n "${ASC_KEY_P8_BASE64:-}" ]]
}

# msign_gate LABEL — call at the top of a step that needs signing configured.
# Returns 1 (skip) with a warning when unconfigured, unless
# CITADEL_REQUIRE_SIGNING is set, in which case it exits the whole script.
# Usage: msign_gate "app signing" || return 0
msign_gate() {
  local label="$1"
  if msign_signing_configured; then
    return 0
  fi
  if msign_require_signing; then
    msign_fatal "$label requires CITADEL_CODESIGN_IDENTITY, and CITADEL_REQUIRE_SIGNING is set — refusing to ship an unsigned bundle."
  fi
  msign_warn "$label skipped: CITADEL_CODESIGN_IDENTITY not set (local/dev build)."
  return 1
}

# msign_check_identity — confirms the configured identity is actually present
# in a keychain codesign can see, so a typo fails with a clear message instead
# of a cryptic codesign error deep in the app-bundle signing step.
msign_check_identity() {
  local identity="$CITADEL_CODESIGN_IDENTITY"
  if [[ "$identity" == "-" ]]; then
    return 0 # ad-hoc signing, nothing to look up
  fi
  if ! security find-identity -v -p codesigning 2>/dev/null | grep -qF "$identity"; then
    msign_fatal "codesign identity not found in any available keychain: \"$identity\""
  fi
}

# msign_sign_file PATH [ENTITLEMENTS_PLIST]
# Hardened runtime + secure timestamp. --timestamp is not optional: an
# otherwise-valid signature without one is rejected at notarization.
msign_sign_file() {
  local path="$1" entitlements="${2:-}"
  local args=(--force --options runtime --timestamp --sign "$CITADEL_CODESIGN_IDENTITY")
  if [[ -n "$entitlements" && -f "$entitlements" ]]; then
    args+=(--entitlements "$entitlements")
  fi
  msign_info "codesign: $path"
  codesign "${args[@]}" "$path"
}

# msign_sign_app APP_DIR [ENTITLEMENTS_PLIST]
# Signs inside-out (every nested executable, then the bundle itself) and
# deliberately never uses --deep: --deep signs nested code with the *outer*
# bundle's defaults, which is exactly how a nested binary's own entitlements
# get silently dropped. Explicit inside-out order is the Apple-recommended
# approach and the only one that composes correctly as the bundle grows
# (citadel#670 will add a helper tool + XPC service under here).
msign_sign_app() {
  local app_dir="$1" entitlements="${2:-}"
  [[ -d "$app_dir" ]] || msign_fatal "msign_sign_app: not a directory: $app_dir"

  local f
  while IFS= read -r -d '' f; do
    msign_sign_file "$f"
  done < <(find "$app_dir/Contents/MacOS" -type f -perm +111 -print0 2>/dev/null)

  # Any frameworks/dylibs (none today, but future-proof for #670's helper).
  if [[ -d "$app_dir/Contents/Frameworks" ]]; then
    while IFS= read -r -d '' f; do
      msign_sign_file "$f"
    done < <(find "$app_dir/Contents/Frameworks" \( -name '*.dylib' -o -name '*.framework' \) -print0 2>/dev/null)
  fi

  msign_sign_file "$app_dir" "$entitlements"
}

# msign_notarize PATH — submits PATH (a zip, dmg, or pkg — notarytool accepts
# all three directly) using the ASC API key and waits for a result. Fails loud
# with the notarization log on rejection so a bad submission is diagnosable
# without re-running by hand.
msign_notarize() {
  local path="$1"
  [[ -e "$path" ]] || msign_fatal "msign_notarize: no such file: $path"

  local keyfile
  keyfile=$(mktemp)
  # Belt and suspenders: mktemp is already 0600 on macOS, but don't rely on a
  # platform default for key material.
  chmod 600 "$keyfile"
  trap 'rm -f "$keyfile"' RETURN

  # BSD base64 (macOS, the only platform this ever runs on) takes -D for
  # decode; -d is the GNU spelling. Try both so a GNU coreutils shadowing
  # /usr/bin (common with a Homebrew PATH) still works.
  if ! base64 -D <<<"$ASC_KEY_P8_BASE64" >"$keyfile" 2>/dev/null; then
    base64 -d <<<"$ASC_KEY_P8_BASE64" >"$keyfile"
  fi
  [[ -s "$keyfile" ]] || msign_fatal "failed to decode ASC_KEY_P8_BASE64 (empty result)."

  msign_info "notarytool submit --wait: $path"
  if ! xcrun notarytool submit "$path" \
        --issuer "$ASC_ISSUER_ID" \
        --key-id "$ASC_KEY_ID" \
        --key "$keyfile" \
        --wait \
        --output-format json >/tmp/citadel-notarize-result.json 2>&1; then
    cat /tmp/citadel-notarize-result.json >&2
    msign_fatal "notarization submission failed for $path (see output above)."
  fi

  local status
  status=$(grep -o '"status"[[:space:]]*:[[:space:]]*"[^"]*"' /tmp/citadel-notarize-result.json | tail -1 | cut -d'"' -f4)
  rm -f /tmp/citadel-notarize-result.json
  if [[ "$status" != "Accepted" ]]; then
    msign_fatal "notarization did not succeed for $path (status: ${status:-unknown})."
  fi
  msign_info "notarization accepted: $path"
}

msign_staple() {
  local path="$1"
  msign_info "stapler staple: $path"
  xcrun stapler staple "$path"
}

# msign_app_bundle APP_DIR [ENTITLEMENTS_PLIST]
# Full sign -> notarize -> staple pipeline for a .app. Notarization requires a
# zip (notarytool cannot take a bare .app directory), built with
# `ditto -c -k --keepParent` so the archive round-trips the bundle exactly —
# a plain `zip -r` does not preserve resource forks / extended attributes the
# same way and has bitten other Apple notarization pipelines.
msign_app_bundle() {
  local app_dir="$1" entitlements="${2:-}"
  msign_gate "app bundle signing ($app_dir)" || return 0
  msign_check_identity
  msign_sign_app "$app_dir" "$entitlements"
  msign_info "codesign --verify: $app_dir"
  codesign --verify --strict --deep --verbose=2 "$app_dir"

  if ! msign_gate "app bundle notarization ($app_dir)"; then
    return 0
  fi
  if ! msign_notarization_configured; then
    if msign_require_signing; then
      msign_fatal "notarization requires ASC_ISSUER_ID/ASC_KEY_ID/ASC_KEY_P8_BASE64, and CITADEL_REQUIRE_SIGNING is set."
    fi
    msign_warn "app bundle notarization skipped: ASC_* credentials not set (bundle is signed but NOT notarized; Gatekeeper will still warn on first launch)."
    return 0
  fi

  local zip_path="${app_dir%.app}.zip"
  rm -f "$zip_path"
  ditto -c -k --keepParent "$app_dir" "$zip_path"
  msign_notarize "$zip_path"
  rm -f "$zip_path"
  # Staple the .app itself (not the zip) — a cask copies the app out of the
  # DMG, and only a staple on the app survives that copy without a network
  # Gatekeeper check.
  msign_staple "$app_dir"
}

# msign_dmg DMG_PATH — sign -> notarize -> staple a already-built DMG. Call
# this AFTER msign_app_bundle has stapled the .app that went into the DMG, so
# both the app and the disk image carry an offline-verifiable staple.
msign_dmg() {
  local dmg_path="$1"
  msign_gate "DMG signing ($dmg_path)" || return 0
  msign_check_identity
  msign_sign_file "$dmg_path"

  if ! msign_gate "DMG notarization ($dmg_path)"; then
    return 0
  fi
  if ! msign_notarization_configured; then
    if msign_require_signing; then
      msign_fatal "notarization requires ASC_ISSUER_ID/ASC_KEY_ID/ASC_KEY_P8_BASE64, and CITADEL_REQUIRE_SIGNING is set."
    fi
    msign_warn "DMG notarization skipped: ASC_* credentials not set (DMG is signed but NOT notarized)."
    return 0
  fi

  msign_notarize "$dmg_path"
  msign_staple "$dmg_path"
}
