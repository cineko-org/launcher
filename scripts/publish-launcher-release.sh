#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 3 ]]; then
  printf 'usage: %s VERSION PUBLISHED_AT ASSETS_DIR\n' "$0" >&2
  exit 2
fi

version="${1#v}"
readonly published_at="$2"
readonly assets_dir="$3"
readonly bucket="cineko-releases"

if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  printf 'Launcher version must be semantic versioning: %s\n' "$version" >&2
  exit 2
fi

for name in \
  CINEKO_RELEASES_S3_ENDPOINT \
  CINEKO_RELEASES_S3_ACCESS_KEY \
  CINEKO_RELEASES_S3_SECRET_KEY \
  CINEKO_RELEASES_PUBLIC_BASE \
  CINEKO_RELEASE_PUBLISH_TOKEN \
  CINEKO_CENTRAL_URL; do
  if [[ -z "${!name:-}" ]]; then
    printf '%s is required\n' "$name" >&2
    exit 2
  fi
done
readonly public_base="${CINEKO_RELEASES_PUBLIC_BASE%/}"

for command in aws curl jq openssl sha256sum stat; do
  command -v "$command" >/dev/null || {
    printf '%s is required on the release publisher runner\n' "$command" >&2
    exit 2
  }
done

temporary_root="$(mktemp -d "${TMPDIR:-/tmp}/cineko-launcher-publish.XXXXXX")"
readonly temporary_root
trap 'rm -rf "$temporary_root"' EXIT
readonly releases_file="$temporary_root/releases.jsonl"
: >"$releases_file"

export AWS_ACCESS_KEY_ID="$CINEKO_RELEASES_S3_ACCESS_KEY"
export AWS_SECRET_ACCESS_KEY="$CINEKO_RELEASES_S3_SECRET_KEY"
export AWS_DEFAULT_REGION="${CINEKO_RELEASES_S3_REGION:-us-east-1}"

publish_artifact() {
  local platform="$1"
  local arch="$2"
  local extension="$3"
  local executable="$4"
  local platform_key="${platform}-${arch}"
  local filename="cineko-launcher-v${version}-${platform_key}.${extension}"
  local artifact_path="${assets_dir}/${filename}"
  local object_key="launcher/v${version}/${platform_key}/${filename}"
  local public_url="${public_base%/}/${object_key}"

  if [[ ! -f "$artifact_path" ]]; then
    printf 'Launcher artifact is missing: %s\n' "$artifact_path" >&2
    return 1
  fi

  local size
  local sha256
  local sha256_base64
  size="$(stat -c '%s' "$artifact_path")"
  sha256="$(sha256sum "$artifact_path" | awk '{print $1}')"
  sha256_base64="$(openssl dgst -sha256 -binary "$artifact_path" | openssl base64 -A)"

  local object_metadata
  if ! object_metadata="$(aws --endpoint-url "$CINEKO_RELEASES_S3_ENDPOINT" s3api head-object \
    --bucket "$bucket" --key "$object_key" --checksum-mode ENABLED --output json 2>/dev/null)"; then
    # The conditional write closes the stat/copy race. A concurrent identical
    # publisher is accepted only after the authoritative object metadata check.
    aws --endpoint-url "$CINEKO_RELEASES_S3_ENDPOINT" s3api put-object \
      --bucket "$bucket" --key "$object_key" --body "$artifact_path" \
      --checksum-algorithm SHA256 --checksum-sha256 "$sha256_base64" \
      --metadata "sha256=$sha256" --if-none-match '*' >/dev/null 2>&1 || true
    object_metadata="$(aws --endpoint-url "$CINEKO_RELEASES_S3_ENDPOINT" s3api head-object \
      --bucket "$bucket" --key "$object_key" --checksum-mode ENABLED --output json)"
  fi

  local remote_size
  local remote_sha256
  remote_size="$(jq -er '.ContentLength' <<<"$object_metadata")"
  remote_sha256="$(jq -r '.ChecksumSHA256 // empty' <<<"$object_metadata")"
  if [[ "$remote_size" != "$size" ]]; then
    printf 'immutable release object size mismatch: %s\n' "$object_key" >&2
    return 1
  fi
  if [[ -z "$remote_sha256" ]]; then
    # Compatibility for immutable objects published before S3 checksums were
    # enabled. New objects always use the authoritative ChecksumSHA256 field.
    remote_sha256="$(jq -r '.Metadata.sha256 // empty' <<<"$object_metadata")"
    if [[ "$remote_sha256" != "$sha256" ]]; then
      printf 'immutable legacy release object checksum mismatch: %s\n' "$object_key" >&2
      return 1
    fi
  elif [[ "$remote_sha256" != "$sha256_base64" ]]; then
    printf 'immutable release object checksum mismatch: %s\n' "$object_key" >&2
    return 1
  fi

  jq -cn \
    --arg channel stable \
    --arg platform "$platform" \
    --arg arch "$arch" \
    --arg version "$version" \
    --arg url "$public_url" \
    --arg sha256 "$sha256" \
    --arg executable "$executable" \
    --arg publishedAt "$published_at" \
    --argjson size "$size" \
    '{channel:$channel,platform:$platform,arch:$arch,version:$version,protocol:3,launcher:{url:$url,size:$size,sha256:$sha256,executable:$executable},publishedAt:$publishedAt}' \
    >>"$releases_file"

  printf 'verified Launcher v%s for %s (%s bytes, sha256 %s)\n' \
    "$version" "$platform_key" "$size" "$sha256"
}

publish_artifact darwin arm64 zip 'Cineko Launcher.app/Contents/MacOS/Cineko Launcher'
publish_artifact windows amd64 exe 'Cineko Launcher.exe'
publish_artifact linux amd64 AppImage "cineko-launcher-v${version}-linux-amd64.AppImage"

payload="$(jq -sc '{schemaVersion:2,payload:{releases:.}}' "$releases_file")"
curl --fail --silent --show-error \
  --request POST \
  --header "Authorization: Bearer ${CINEKO_RELEASE_PUBLISH_TOKEN}" \
  --header 'Content-Type: application/json' \
  --header 'X-Cineko-Protocol: 3' \
  --data "$payload" \
  "${CINEKO_CENTRAL_URL%/}/v1/release-registry/launcher" |
  jq -e '.generation | numbers | select(. > 0)' >/dev/null

printf 'activated Launcher v%s for all supported platforms\n' "$version"
