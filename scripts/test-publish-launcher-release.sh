#!/usr/bin/env bash
set -euo pipefail

test_root="$(mktemp -d "${TMPDIR:-/tmp}/cineko-launcher-publisher-test.XXXXXX")"
readonly test_root
trap 'rm -rf "$test_root"' EXIT
readonly assets="$test_root/assets"
readonly objects="$test_root/objects"
readonly payloads="$test_root/payloads.jsonl"
readonly race_marker="$test_root/put-race"
fake_bin="$(pwd)/scripts/testdata/release-publisher"
readonly fake_bin
mkdir -p "$assets" "$objects"

printf 'portable-darwin-arm64\n' >"$assets/cineko-launcher-v1.2.3-darwin-arm64.zip"
printf 'portable-windows-amd64\n' >"$assets/cineko-launcher-v1.2.3-windows-amd64.exe"
printf 'portable-linux-amd64\n' >"$assets/cineko-launcher-v1.2.3-linux-amd64.AppImage"

run_publisher() {
  PATH="$fake_bin:$PATH" \
  FAKE_ASSET_ROOT="$assets" \
  FAKE_OBJECT_ROOT="$objects" \
  FAKE_PAYLOADS="$payloads" \
  FAKE_PUT_RACE_MARKER="$race_marker" \
  CINEKO_RELEASES_S3_ENDPOINT=https://minio.example \
  CINEKO_RELEASES_S3_ACCESS_KEY=access \
  CINEKO_RELEASES_S3_SECRET_KEY=secret \
  CINEKO_RELEASES_PUBLIC_BASE=https://releases.example.com/cineko \
  CINEKO_RELEASE_PUBLISH_TOKEN=publisher \
  CINEKO_CENTRAL_URL=https://central.example \
    scripts/publish-launcher-release.sh 1.2.3 2026-08-12T00:00:00Z "$assets"
}

run_publisher >/dev/null
if [[ "$(find "$objects" -type f | wc -l | tr -d ' ')" != 3 ]]; then
  printf 'publisher did not upload all platform artifacts\n' >&2
  exit 1
fi
jq -se '
  (length == 1) and
  (.[0] | .schemaVersion == 2 and (.payload.releases |
    (length == 3) and
    all(.[]; .version == "1.2.3" and .protocol == 3 and .launcher.size > 0 and (.launcher.sha256 | length) == 64) and
    any(.[]; .platform == "linux" and .launcher.executable == "cineko-launcher-v1.2.3-linux-amd64.AppImage" and (.launcher.url | endswith(".AppImage")))
  ))
' "$payloads" >/dev/null

# Re-running an identical release is idempotent and reuses the immutable objects.
run_publisher >/dev/null
if [[ "$(wc -l <"$payloads" | tr -d ' ')" != 2 ]]; then
  printf 'publisher did not register exactly one atomic set per successful run\n' >&2
  exit 1
fi

# A versioned object can never be overwritten with different bytes. A partial
# platform failure must happen before Central sees any part of the release set.
printf 'changed\n' >>"$assets/cineko-launcher-v1.2.3-windows-amd64.exe"
if run_publisher >/dev/null 2>&1; then
  printf 'publisher overwrote an immutable release object\n' >&2
  exit 1
fi
if [[ "$(wc -l <"$payloads" | tr -d ' ')" != 2 ]]; then
  printf 'publisher registered a partial platform set\n' >&2
  exit 1
fi

printf 'Launcher release publisher checks passed\n'
