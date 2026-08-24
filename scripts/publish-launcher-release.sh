#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 ]]; then
  printf 'usage: %s LAUNCHER_RELEASE_SET ASSETS_DIR\n' "$0" >&2
  exit 2
fi

readonly release_set="$1"
readonly assets_dir="$2"
: "${CINEKO_RELEASES_S3_ENDPOINT:?required}"
: "${CINEKO_RELEASES_S3_ACCESS_KEY:?required}"
: "${CINEKO_RELEASES_S3_SECRET_KEY:?required}"
: "${CINEKO_RELEASES_PUBLIC_BASE_URL:?required}"
readonly bucket="${CINEKO_RELEASES_S3_BUCKET:-cineko-releases}"
readonly public_base_url="${CINEKO_RELEASES_PUBLIC_BASE_URL%/}"
export AWS_ACCESS_KEY_ID="$CINEKO_RELEASES_S3_ACCESS_KEY"
export AWS_SECRET_ACCESS_KEY="$CINEKO_RELEASES_S3_SECRET_KEY"
export AWS_DEFAULT_REGION="${CINEKO_RELEASES_S3_REGION:-us-east-1}"

for command in aws go jq openssl; do
  command -v "$command" >/dev/null || {
    printf '%s is required on the release publisher runner\n' "$command" >&2
    exit 2
  }
done

temporary_directory="$(mktemp -d "${TMPDIR:-/tmp}/cineko-launcher-publish.XXXXXX")"
readonly temporary_directory
trap 'rm -rf "$temporary_directory"' EXIT
readonly release_contract="$temporary_directory/releasecontract"
GOWORK=off go build -mod=vendor -o "$release_contract" ./cmd/releasecontract
"$release_contract" verify-set "$release_set"

publish_immutable() {
  local artifact="$1"
  local object_key="$2"
  local expected_size="$3"
  local expected_sha256="$4"
  local expected_sha256_base64
  expected_sha256_base64="$(openssl dgst -sha256 -binary "$artifact" | openssl base64 -A)"
  local object_metadata
  if ! object_metadata="$(aws --endpoint-url "$CINEKO_RELEASES_S3_ENDPOINT" s3api head-object \
    --bucket "$bucket" --key "$object_key" --checksum-mode ENABLED --output json 2>/dev/null)"; then
    aws --endpoint-url "$CINEKO_RELEASES_S3_ENDPOINT" s3api put-object \
      --bucket "$bucket" --key "$object_key" --body "$artifact" \
      --checksum-algorithm SHA256 --checksum-sha256 "$expected_sha256_base64" \
      --metadata "sha256=$expected_sha256" --if-none-match '*' >/dev/null 2>&1 || true
    object_metadata="$(aws --endpoint-url "$CINEKO_RELEASES_S3_ENDPOINT" s3api head-object \
      --bucket "$bucket" --key "$object_key" --checksum-mode ENABLED --output json)"
  fi
  local remote_size remote_sha256
  remote_size="$(jq -er '.ContentLength' <<<"$object_metadata")"
  remote_sha256="$(jq -r '.ChecksumSHA256 // .Metadata.sha256 // empty' <<<"$object_metadata")"
  if [[ "$remote_size" != "$expected_size" ]] ||
    [[ "$remote_sha256" != "$expected_sha256_base64" && "$remote_sha256" != "$expected_sha256" ]]; then
    printf 'immutable Launcher object mismatch: %s\n' "$object_key" >&2
    exit 1
  fi
}

manifest_paths=()
while IFS= read -r release; do
  platform="$(jq -er '.platform' <<<"$release")"
  architecture="$(jq -er '.architecture' <<<"$release")"
  public_url="$(jq -er '.launcher.url' <<<"$release")"
  expected_size="$(jq -er '.launcher.size' <<<"$release")"
  expected_sha256="$(jq -er '.launcher.sha256' <<<"$release")"
  prefix="${public_base_url}/"
  [[ "$public_url" == "$prefix"* ]] || {
    printf 'Launcher URL is outside the public release base: %s\n' "$public_url" >&2
    exit 1
  }
  object_key="${public_url#"$prefix"}"
  artifact="${assets_dir%/}/$(basename "$public_url")"
  [[ -f "$artifact" ]] || { printf 'Launcher artifact is missing: %s\n' "$artifact" >&2; exit 1; }
  [[ "$(wc -c <"$artifact" | tr -d '[:space:]')" == "$expected_size" ]] || {
    printf 'Launcher artifact size mismatch: %s\n' "$artifact" >&2
    exit 1
  }
  [[ "$(openssl dgst -sha256 "$artifact" | awk '{print $NF}')" == "$expected_sha256" ]] || {
    printf 'Launcher artifact checksum mismatch: %s\n' "$artifact" >&2
    exit 1
  }
  publish_immutable "$artifact" "$object_key" "$expected_size" "$expected_sha256"
  manifest_path="$temporary_directory/${platform}-${architecture}.json"
  jq '.' <<<"$release" >"$manifest_path"
  manifest_paths+=("${platform}-${architecture}:$manifest_path")
done < <(jq -c '.releases[]' "$release_set")

[[ "${#manifest_paths[@]}" -eq 3 ]] || { printf 'expected three Launcher channel manifests\n' >&2; exit 1; }
for manifest_entry in "${manifest_paths[@]}"; do
  platform_key="${manifest_entry%%:*}"
  manifest_path="${manifest_entry#*:}"
  aws --endpoint-url "$CINEKO_RELEASES_S3_ENDPOINT" s3api put-object \
    --bucket "$bucket" --key "channels/stable/${platform_key}/launcher.json" \
    --body "$manifest_path" --content-type 'application/json; charset=utf-8' \
    --cache-control no-store >/dev/null
done

printf 'published immutable Launcher artifacts and stable channel manifests\n'
