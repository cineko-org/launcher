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

for command in curl jq sha256sum wc; do
  command -v "$command" >/dev/null || {
    printf '%s is required on the release publisher runner\n' "$command" >&2
    exit 2
  }
done

temporary_root="$(mktemp -d "${TMPDIR:-/tmp}/cineko-launcher-register.XXXXXX")"
readonly temporary_root
trap 'rm -rf "$temporary_root"' EXIT
readonly releases_file="$temporary_root/releases.jsonl"
: >"$releases_file"

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

  local size
  local sha256
  size="$(wc -c <"$artifact_path" | tr -d '[:space:]')"
  sha256="$(sha256sum "$artifact_path" | awk '{print $1}')"
	jq -cn \
	    --arg channel stable \
	    --arg platform "$platform" \
	    --arg architecture "$architecture" \
    --arg version "$version" \
    --arg url "${public_base}/${filename}" \
    --arg sha256 "$sha256" \
    --arg executable "$executable" \
    --arg publishedAt "$published_at" \
    --argjson size "$size" \
	    '{channel:$channel,platform:$platform,architecture:$architecture,version:$version,launcher:{url:$url,size:$size,sha256:$sha256,executable:$executable},publishedAt:$publishedAt}' \
    >>"$releases_file"
}

append_release darwin arm64 zip 'Cineko Launcher.app/Contents/MacOS/Cineko Launcher'
append_release windows amd64 exe 'Cineko Launcher.exe'
append_release linux amd64 AppImage "cineko-launcher-v${version}-linux-amd64.AppImage"

payload="$(jq -sc '{releases:.}' "$releases_file")"
curl --fail-with-body --retry 3 --retry-all-errors \
  --request POST \
  --header "Authorization: Bearer ${CINEKO_RELEASE_PUBLISH_TOKEN}" \
  --header 'Content-Type: application/json' \
  --data "$payload" \
  "${CINEKO_CENTRAL_URL%/}/v1/release-registry/launcher" |
  jq -e '.generation | numbers | select(. > 0)' >/dev/null

printf 'registered Launcher v%s for all supported platforms\n' "$version"
