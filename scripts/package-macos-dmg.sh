#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 || "$1" != *.app || "$2" != *.dmg ]]; then
  printf 'usage: %s SIGNED_APP OUTPUT_DMG\n' "$0" >&2
  exit 2
fi
if [[ "${CI:-}" != true || "$(uname -s)" != Darwin ]]; then
  printf 'DMG packaging runs only on macOS CI; local build intermediates are not allowed\n' >&2
  exit 2
fi
: "${CREATE_DMG:?path to the pinned create-dmg script is required}"
: "${RUNNER_TEMP:?required on the macOS CI runner}"
readonly app_path="$1"
readonly output_path="$2"
[[ -d "$app_path" && ! -e "$output_path" ]] || {
  printf 'signed app is missing or output already exists\n' >&2
  exit 2
}
codesign --verify --deep --strict "$app_path"

work_dir="$(mktemp -d "$RUNNER_TEMP/cineko-dmg.XXXXXX")"
readonly work_dir
readonly mount_path="$work_dir/verify"
cleanup() {
  if mount | grep -Fq " on $mount_path "; then
    hdiutil detach "$mount_path" >/dev/null || true
  fi
  rm -rf "$work_dir"
}
trap cleanup EXIT

mkdir -p "$work_dir/source" "$(dirname "$output_path")" "$mount_path"
ditto "$app_path" "$work_dir/source/Cineko Launcher.app"
swift scripts/render-dmg-background.swift "$work_dir/background.png"
bash "$CREATE_DMG" \
  --volname 'Install Cineko' \
  --volicon "$app_path/Contents/Resources/iconfile.icns" \
  --background "$work_dir/background.png" \
  --window-pos 200 120 --window-size 720 440 \
  --icon-size 112 --text-size 14 \
  --icon 'Cineko Launcher.app' 180 215 \
  --hide-extension 'Cineko Launcher.app' \
  --app-drop-link 540 215 \
  "$output_path" "$work_dir/source"

hdiutil verify "$output_path"
hdiutil attach "$output_path" -readonly -nobrowse -noautoopen -mountpoint "$mount_path" >/dev/null
[[ "$(readlink "$mount_path/Applications")" == /Applications ]]
[[ -s "$mount_path/.DS_Store" ]]
[[ -s "$mount_path/.background/background.png" ]]
codesign --verify --deep --strict "$mount_path/Cineko Launcher.app"
xcrun stapler validate "$mount_path/Cineko Launcher.app"
hdiutil detach "$mount_path" >/dev/null
printf 'Verified drag-to-Applications DMG: %s\n' "$output_path"
