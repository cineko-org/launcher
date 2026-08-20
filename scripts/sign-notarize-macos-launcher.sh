#!/usr/bin/env bash
set -euo pipefail

# Signs a portable macOS app, submits it to Apple, staples the ticket, and emits
# the only ZIP that downstream release steps are allowed to publish.

readonly app_path="${1:-}"
readonly output_path="${2:-}"
readonly notary_timeout="${NOTARY_TIMEOUT:-10m}"

required_environment=(
  APPLE_DEVELOPER_ID_P12_BASE64
  APPLE_DEVELOPER_ID_P12_PASSWORD
  APPLE_NOTARY_KEY_P8_BASE64
  APPLE_NOTARY_KEY_ID
  APPLE_NOTARY_ISSUER_ID
  APPLE_TEAM_ID
)
required_commands=(base64 codesign curl ditto jq openssl security shasum spctl unzip xcrun)

readonly developer_id_g2_url='https://www.apple.com/certificateauthority/DeveloperIDG2CA.cer'
readonly developer_id_g2_sha256='f16cd3c54c7f83cea4bf1a3e6a0819c8aaa8e4a1528fd144715f350643d2df3a'

fail() {
  printf 'macOS release signing failed: %s\n' "$1" >&2
  exit 1
}

for name in "${required_environment[@]}"; do
  [[ -n "${!name:-}" ]] || fail "required environment variable ${name} is empty"
done
for command_name in "${required_commands[@]}"; do
  command -v "$command_name" >/dev/null 2>&1 || fail "required command ${command_name} is unavailable"
done

[[ -d "$app_path" ]] || fail "app bundle does not exist"
[[ "$app_path" == *.app ]] || fail "input must be an app bundle"
[[ -n "$output_path" && "$output_path" == *.zip ]] || fail "output must be a ZIP path"

work_dir="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/cineko-signing.XXXXXX")"
keychain_path="$work_dir/cineko-release.keychain-db"
keychain_password="$(openssl rand -hex 32)"
p12_path="$work_dir/developer-id.p12"
p8_path="$work_dir/AuthKey.p8"
developer_id_g2_path="$work_dir/DeveloperIDG2CA.cer"
submission_zip="$work_dir/submission.zip"
final_zip="$work_dir/final.zip"
verification_dir="$work_dir/verification"
notary_result="$work_dir/notary-result.json"
original_keychains=()

while IFS= read -r keychain; do
  original_keychains+=("${keychain//\"/}")
done < <(security list-keychains -d user | sed 's/^[[:space:]]*//')

cleanup() {
  if ((${#original_keychains[@]} > 0)); then
    security list-keychains -d user -s "${original_keychains[@]}" >/dev/null 2>&1 || true
  fi
  security delete-keychain "$keychain_path" >/dev/null 2>&1 || true
  rm -rf "$work_dir"
}
trap cleanup EXIT

umask 077
printf '%s' "$APPLE_DEVELOPER_ID_P12_BASE64" | base64 -D >"$p12_path"
printf '%s' "$APPLE_NOTARY_KEY_P8_BASE64" | base64 -D >"$p8_path"
curl --fail --silent --show-error --location \
  "$developer_id_g2_url" \
  --output "$developer_id_g2_path"
printf '%s  %s\n' "$developer_id_g2_sha256" "$developer_id_g2_path" | shasum -a 256 -c - >/dev/null

security create-keychain -p "$keychain_password" "$keychain_path"
security set-keychain-settings -lut 21600 "$keychain_path"
security unlock-keychain -p "$keychain_password" "$keychain_path"
security list-keychains -d user -s "$keychain_path" "${original_keychains[@]}"
security import "$developer_id_g2_path" \
  -k "$keychain_path" \
  -T /usr/bin/codesign \
  -T /usr/bin/security >/dev/null
security import "$p12_path" \
  -k "$keychain_path" \
  -P "$APPLE_DEVELOPER_ID_P12_PASSWORD" \
  -T /usr/bin/codesign \
  -T /usr/bin/security >/dev/null
security find-key -a "$keychain_path" >/dev/null 2>&1 || fail "P12 does not contain an importable private key"
security set-key-partition-list \
  -S apple-tool:,apple:,codesign: \
  -s -k "$keychain_password" "$keychain_path" >/dev/null

identities="$(security find-identity -v -p codesigning "$keychain_path")"
identity="$(printf '%s\n' "$identities" | sed -n 's/.*"\(Developer ID Application:.*\)"/\1/p' | head -n 1)"
[[ -n "$identity" ]] || fail "Developer ID Application identity was not imported"
[[ "$identity" == *"(${APPLE_TEAM_ID})"* ]] || fail "signing identity does not match APPLE_TEAM_ID"

codesign \
  --force \
  --options runtime \
  --timestamp \
  --keychain "$keychain_path" \
  --sign "$identity" \
  "$app_path"
codesign --verify --deep --strict --verbose=2 "$app_path"

ditto -c -k --sequesterRsrc --keepParent "$app_path" "$submission_zip"
if ! xcrun notarytool submit "$submission_zip" \
  --key "$p8_path" \
  --key-id "$APPLE_NOTARY_KEY_ID" \
  --issuer "$APPLE_NOTARY_ISSUER_ID" \
  --wait \
  --timeout "$notary_timeout" \
  --output-format json >"$notary_result"; then
  submission_id="$(jq -r '.id // empty' "$notary_result" 2>/dev/null || true)"
  if [[ -n "$submission_id" ]]; then
    xcrun notarytool log "$submission_id" \
      --key "$p8_path" \
      --key-id "$APPLE_NOTARY_KEY_ID" \
      --issuer "$APPLE_NOTARY_ISSUER_ID" >&2 || true
  fi
  fail "Apple notarization submission failed"
fi

status="$(jq -r '.status // empty' "$notary_result")"
if [[ "$status" != 'Accepted' ]]; then
  submission_id="$(jq -r '.id // empty' "$notary_result")"
  if [[ -n "$submission_id" ]]; then
    xcrun notarytool log "$submission_id" \
      --key "$p8_path" \
      --key-id "$APPLE_NOTARY_KEY_ID" \
      --issuer "$APPLE_NOTARY_ISSUER_ID" >&2 || true
  fi
  fail "Apple notarization did not return Accepted"
fi

xcrun stapler staple "$app_path"
xcrun stapler validate "$app_path"
codesign --verify --deep --strict --verbose=2 "$app_path"
spctl --assess --type execute --verbose=4 "$app_path"

mkdir -p "$(dirname "$output_path")"
ditto -c -k --sequesterRsrc --keepParent "$app_path" "$final_zip"
unzip -t "$final_zip" >/dev/null
mkdir -p "$verification_dir"
ditto -x -k "$final_zip" "$verification_dir"
verified_app="$verification_dir/$(basename "$app_path")"
[[ -d "$verified_app" ]] || fail "final ZIP does not contain the app bundle"
codesign --verify --deep --strict --verbose=2 "$verified_app"
xcrun stapler validate "$verified_app"
spctl --assess --type execute --verbose=4 "$verified_app"
mv -f "$final_zip" "$output_path"

printf 'Signed and notarized macOS Launcher: %s\n' "$output_path"
