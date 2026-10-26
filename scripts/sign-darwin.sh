#!/usr/bin/env bash
# Signs and notarizes one darwin binary. GoReleaser runs it after each build.
#
#   scripts/sign-darwin.sh <binary> <goos>
#
# Needs APPLE_SIGNING_IDENTITY (a "Developer ID Application" identity in an
# unlocked keychain), NOTARY_API_KEY_PATH, APPLE_API_KEY and APPLE_API_ISSUER;
# the release workflow sets them. Without them the binary stays unsigned,
# unless BROUT_REQUIRE_SIGNING=1, which makes that an error.
set -euo pipefail

binary="$1"
goos="$2"
[ "$goos" = darwin ] || exit 0

missing=()
for var in APPLE_SIGNING_IDENTITY NOTARY_API_KEY_PATH APPLE_API_KEY APPLE_API_ISSUER; do
  [ -n "${!var:-}" ] || missing+=("$var")
done
if [ ${#missing[@]} -gt 0 ]; then
  if [ "${BROUT_REQUIRE_SIGNING:-}" = 1 ]; then
    echo "sign-darwin: cannot sign $binary; missing ${missing[*]}" >&2
    exit 1
  fi
  echo "sign-darwin: leaving $binary unsigned; missing ${missing[*]}" >&2
  exit 0
fi

codesign --force --sign "$APPLE_SIGNING_IDENTITY" --options runtime --timestamp "$binary"
codesign --verify --strict --verbose=2 "$binary"

# A bare binary cannot be stapled, so Gatekeeper checks the notarization
# ticket online on first run.
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
zip="$work/$(basename "$(dirname "$binary")").zip"
ditto -c -k --keepParent "$binary" "$zip"
result="$(xcrun notarytool submit "$zip" \
  --key "$NOTARY_API_KEY_PATH" --key-id "$APPLE_API_KEY" --issuer "$APPLE_API_ISSUER" \
  --wait --output-format json)"
status="$(jq -r '.status' <<<"$result")"
id="$(jq -r '.id' <<<"$result")"
if [ "$status" != Accepted ]; then
  xcrun notarytool log "$id" \
    --key "$NOTARY_API_KEY_PATH" --key-id "$APPLE_API_KEY" --issuer "$APPLE_API_ISSUER" >&2 || true
  echo "sign-darwin: notarization of $binary is $status (submission $id); Apple's log is above" >&2
  exit 1
fi
echo "sign-darwin: $binary signed and notarized (submission $id)"
