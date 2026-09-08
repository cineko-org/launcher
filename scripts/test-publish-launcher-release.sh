#!/usr/bin/env bash
set -euo pipefail

test_root="$(mktemp -d "${TMPDIR:-/tmp}/cineko-launcher-publisher-test.XXXXXX")"
readonly test_root
trap 'rm -rf "$test_root"' EXIT
readonly assets="$test_root/assets"
mkdir -p "$assets"

printf 'portable-darwin-arm64\n' >"$assets/cineko-launcher-v1.2.3-darwin-arm64.zip"
printf 'portable-windows-amd64\n' >"$assets/cineko-launcher-v1.2.3-windows-amd64.exe"
printf 'portable-linux-amd64\n' >"$assets/cineko-launcher-v1.2.3-linux-amd64.AppImage"

run_publisher() {
  CINEKO_LAUNCHER_RELEASE_BASE=https://github.example/releases/download/v1.2.3 \
    scripts/register-launcher-release.sh 1.2.3 2026-08-12T00:00:00Z "$assets"
}

payload="$test_root/launcher-release-set.json"
run_publisher >"$payload"
jq -e '
  reduce .releases[] as $release ({};
    .[$release.platform + "-" + $release.architecture] = $release.launcher.url
  ) == {
    "darwin-arm64": "https://github.example/releases/download/v1.2.3/cineko-launcher-v1.2.3-darwin-arm64.zip",
    "windows-amd64": "https://github.example/releases/download/v1.2.3/cineko-launcher-v1.2.3-windows-amd64.exe",
    "linux-amd64": "https://github.example/releases/download/v1.2.3/cineko-launcher-v1.2.3-linux-amd64.AppImage"
  }
' "$payload" >/dev/null

scripts/publish-launcher-release.sh "$payload" "$assets"
for platform in darwin-arm64 linux-amd64 windows-amd64; do
  jq -e --arg platform "$platform" \
    '(.platform + "-" + .architecture) == $platform and .version == "1.2.3"' \
    "$assets/launcher-$platform.json" >/dev/null
done
printf 'tampered' >>"$assets/cineko-launcher-v1.2.3-windows-amd64.exe"
if scripts/publish-launcher-release.sh "$payload" "$assets" >/dev/null 2>&1; then
  printf 'publisher accepted an artifact that no longer matches the manifest\n' >&2
  exit 1
fi

mv "$assets/cineko-launcher-v1.2.3-windows-amd64.exe" "$assets/missing.exe"
if run_publisher >/dev/null 2>&1; then
  printf 'publisher accepted an incomplete platform set\n' >&2
  exit 1
fi

printf 'Launcher release publisher checks passed\n'
