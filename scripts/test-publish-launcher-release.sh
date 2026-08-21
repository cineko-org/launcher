#!/usr/bin/env bash
set -euo pipefail

test_root="$(mktemp -d "${TMPDIR:-/tmp}/cineko-launcher-publisher-test.XXXXXX")"
readonly test_root
trap 'rm -rf "$test_root"' EXIT
readonly assets="$test_root/assets"
readonly payloads="$test_root/payloads.jsonl"
fake_bin="$(pwd)/scripts/testdata/release-publisher"
readonly fake_bin
mkdir -p "$assets"

printf 'portable-darwin-arm64\n' >"$assets/cineko-launcher-v1.2.3-darwin-arm64.zip"
printf 'portable-windows-amd64\n' >"$assets/cineko-launcher-v1.2.3-windows-amd64.exe"
printf 'portable-linux-amd64\n' >"$assets/cineko-launcher-v1.2.3-linux-amd64.AppImage"

run_publisher() {
  PATH="$fake_bin:$PATH" \
  FAKE_PAYLOADS="$payloads" \
  CINEKO_LAUNCHER_RELEASE_BASE=https://github.example/releases/download/v1.2.3 \
  CINEKO_RELEASE_PUBLISH_TOKEN=publisher \
  CINEKO_CENTRAL_URL=https://central.example \
    scripts/register-launcher-release.sh 1.2.3 2026-08-12T00:00:00Z "$assets"
}

run_publisher >/dev/null
jq -se '
  (length == 1) and
  (.[0] | (.releases |
    (length == 3) and
    all(.[]; .version == "1.2.3" and .architecture != null and .launcher.size > 0 and (.launcher.sha256 | length) == 64 and (.launcher.url | startswith("https://github.example/releases/download/v1.2.3/"))) and
    any(.[]; .platform == "linux" and .architecture == "amd64" and .launcher.executable == "cineko-launcher-v1.2.3-linux-amd64.AppImage" and (.launcher.url | endswith(".AppImage")))
  ))
' "$payloads" >/dev/null

mv "$assets/cineko-launcher-v1.2.3-windows-amd64.exe" "$assets/missing.exe"
if run_publisher >/dev/null 2>&1; then
  printf 'publisher accepted an incomplete platform set\n' >&2
  exit 1
fi
if [[ "$(wc -l <"$payloads" | tr -d ' ')" != 1 ]]; then
  printf 'publisher registered a partial platform set\n' >&2
  exit 1
fi

printf 'Launcher release publisher checks passed\n'
