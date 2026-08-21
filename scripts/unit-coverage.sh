#!/usr/bin/env bash
set -euo pipefail

profile="$(mktemp "${TMPDIR:-/tmp}/cineko-launcher-coverage.XXXXXX")"
trap 'rm -f "$profile"' EXIT

GOWORK=off go test -race -coverprofile="$profile" ./internal/keys
coverage="$(GOWORK=off go tool cover -func="$profile" | awk '/^total:/ {gsub(/%/, "", $3); print $3}')"
if [[ "$coverage" != "100.0" ]]; then
  printf 'Launcher security-core coverage must be 100.0%%; got %s%%\n' "$coverage" >&2
  exit 1
fi
printf 'Launcher security-core coverage: %s%%\n' "$coverage"
