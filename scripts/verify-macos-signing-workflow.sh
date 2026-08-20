#!/usr/bin/env bash
set -euo pipefail

readonly workflow='.github/workflows/release.yml'
readonly signer='scripts/sign-notarize-macos-launcher.sh'
readonly release_config='release-please-config.json'

fail() {
  printf 'macOS signing workflow check failed: %s\n' "$1" >&2
  exit 1
}

for secret_name in \
  APPLE_DEVELOPER_ID_P12_BASE64 \
  APPLE_DEVELOPER_ID_P12_PASSWORD \
  APPLE_NOTARY_KEY_P8_BASE64 \
  APPLE_NOTARY_KEY_ID \
  APPLE_NOTARY_ISSUER_ID \
  APPLE_TEAM_ID; do
  grep -Fq "${secret_name}: \${{ secrets.${secret_name} }}" "$workflow" || \
    fail "${secret_name} is not sourced from a repository secret"
done

grep -Fq 'scripts/sign-notarize-macos-launcher.sh' "$workflow" || fail 'release workflow does not invoke the signer'
grep -Fq 'NOTARY_TIMEOUT: 10m' "$workflow" || fail 'notarization timeout is not 10 minutes'
jq -e '.draft == true and .["force-tag-creation"] == true' "$release_config" >/dev/null || \
  fail 'Release Please must create a tagged draft release'

required_signer_text=(
  'trap cleanup EXIT'
  'security import'
  '--options runtime'
  '--timestamp'
  'xcrun notarytool submit'
  '--wait'
  'xcrun stapler staple'
  'xcrun stapler validate'
  'spctl --assess'
  "ditto -x -k \"\$final_zip\" \"\$verification_dir\""
  "codesign --verify --deep --strict --verbose=2 \"\$verified_app\""
  "xcrun stapler validate \"\$verified_app\""
  "spctl --assess --type execute --verbose=4 \"\$verified_app\""
)
for expected in "${required_signer_text[@]}"; do
  grep -Fq -- "$expected" "$signer" || fail "signer is missing ${expected}"
done

sign_line="$(grep -n -- "--sign \"\$identity\"" "$signer" | head -n 1 | cut -d: -f1)"
notary_line="$(grep -n 'xcrun notarytool submit' "$signer" | head -n 1 | cut -d: -f1)"
staple_line="$(grep -n 'xcrun stapler staple' "$signer" | head -n 1 | cut -d: -f1)"
final_zip_line="$(grep -n "ditto -c -k --sequesterRsrc --keepParent \"\$app_path\" \"\$final_zip\"" "$signer" | head -n 1 | cut -d: -f1)"
[[ -n "$sign_line" && -n "$notary_line" && -n "$staple_line" && -n "$final_zip_line" ]] || fail 'release operation order cannot be verified'
((sign_line < notary_line && notary_line < staple_line && staple_line < final_zip_line)) || \
  fail 'final ZIP must be created only after signing, notarization, and stapling'

draft_guard_line="$(grep -n 'Require an unpublished draft release' "$workflow" | head -n 1 | cut -d: -f1)"
upload_line="$(grep -n 'Attach portable Launchers' "$workflow" | head -n 1 | cut -d: -f1)"
publish_line="$(grep -n 'Publish the complete portable release' "$workflow" | head -n 1 | cut -d: -f1)"
register_line="$(grep -n 'Register the stable Launcher set' "$workflow" | head -n 1 | cut -d: -f1)"
[[ -n "$draft_guard_line" && -n "$upload_line" && -n "$publish_line" && -n "$register_line" ]] || \
  fail 'draft publication order cannot be verified'
((draft_guard_line < upload_line && upload_line < publish_line && publish_line < register_line)) || \
  fail 'release must remain draft until all portable artifacts are attached'
