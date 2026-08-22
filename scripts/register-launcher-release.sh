#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 3 ]]; then
  printf 'usage: %s VERSION PUBLISHED_AT ASSETS_DIR\n' "$0" >&2
  exit 2
fi

readonly version="${1#v}"
readonly published_at="$2"
readonly assets_dir="$3"
: "${CINEKO_CENTRAL_URL:?required}"
: "${CINEKO_RELEASE_PUBLISH_TOKEN:?required}"
: "${CINEKO_LAUNCHER_RELEASE_BASE:?required}"
readonly public_base="${CINEKO_LAUNCHER_RELEASE_BASE%/}"

if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  printf 'Launcher version must be semantic versioning: %s\n' "$version" >&2
  exit 2
fi
if [[ "$public_base" != https://* ]]; then
  printf 'Launcher release base must use HTTPS\n' >&2
  exit 2
fi

command -v go >/dev/null || {
  printf 'go is required on the release publisher runner\n' >&2
  exit 2
}

temporary_root="$(mktemp -d "${TMPDIR:-/tmp}/cineko-launcher-register.XXXXXX")"
readonly temporary_root
trap 'rm -rf "$temporary_root"' EXIT
readonly release_contract="$temporary_root/releasecontract"
GOWORK=off go build -mod=vendor -o "$release_contract" ./cmd/releasecontract
release_paths=()

append_release() {
  local platform="$1"
  local architecture="$2"
  local extension="$3"
  local executable="$4"
  local platform_key="${platform}-${architecture}"
  local filename="cineko-launcher-v${version}-${platform_key}.${extension}"
  local artifact_path="${assets_dir}/${filename}"

  if [[ ! -f "$artifact_path" ]]; then
    printf 'Launcher artifact is missing: %s\n' "$artifact_path" >&2
    return 1
  fi

  local release_path="$temporary_root/${platform_key}.json"
  "$release_contract" release "$version" "$platform/$architecture" "$artifact_path" "$executable" \
    "${public_base}/${filename}" "$published_at" >"$release_path"
  release_paths+=("$release_path")
}

append_release darwin arm64 zip 'Cineko Launcher.app/Contents/MacOS/Cineko Launcher'
append_release windows amd64 exe 'Cineko Launcher.exe'
append_release linux amd64 AppImage "cineko-launcher-v${version}-linux-amd64.AppImage"

readonly payload="$temporary_root/launcher-release-set.json"
"$release_contract" set "${release_paths[@]}" >"$payload"
"$release_contract" publish "$CINEKO_CENTRAL_URL" "$payload"

printf 'registered Launcher v%s for all supported platforms\n' "$version"
