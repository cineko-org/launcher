#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 ]]; then
  printf 'usage: %s LAUNCHER_RELEASE_SET ASSETS_DIR\n' "$0" >&2
  exit 2
fi

readonly release_set="$1"
readonly assets_dir="$2"
readonly release_contract="$assets_dir/releasecontract"
GOWORK=off go build -mod=vendor -o "$release_contract" ./cmd/releasecontract
trap 'rm -f "$release_contract"' EXIT
"$release_contract" verify-set "$release_set"

while IFS= read -r release; do
  platform="$(jq -er '.platform' <<<"$release")"
  architecture="$(jq -er '.architecture' <<<"$release")"
  public_url="$(jq -er '.launcher.url' <<<"$release")"
  artifact="$assets_dir/$(basename "$public_url")"
  [[ -f "$artifact" ]] || { printf 'Launcher artifact is missing: %s\n' "$artifact" >&2; exit 1; }
  [[ "$(wc -c <"$artifact" | tr -d '[:space:]')" == "$(jq -er '.launcher.size' <<<"$release")" ]] || {
    printf 'Launcher artifact size mismatch: %s\n' "$artifact" >&2
    exit 1
  }
  [[ "$(openssl dgst -sha256 "$artifact" | awk '{print $NF}')" == "$(jq -er '.launcher.sha256' <<<"$release")" ]] || {
    printf 'Launcher artifact checksum mismatch: %s\n' "$artifact" >&2
    exit 1
  }
  jq '.' <<<"$release" >"$assets_dir/launcher-$platform-$architecture.json"
done < <(jq -c '.releases[]' "$release_set")

printf 'verified Launcher artifacts and generated GitHub release manifests\n'
