#!/usr/bin/env bash
set -euo pipefail

profile="$(mktemp "${TMPDIR:-/tmp}/cineko-launcher-coverage.XXXXXX")"
trap 'rm -f "$profile"' EXIT

readonly cover_packages=./internal/launcher/...
GOWORK=off go test -race -covermode=atomic -coverpkg="$cover_packages" \
  -coverprofile="$profile" ./internal/launcher/...
coverage="$(GOWORK=off go tool cover -func="$profile" | awk '/^total:/ {gsub(/%/, "", $3); print $3}')"
if ! awk -v coverage="$coverage" 'BEGIN { exit !(coverage >= 55.0) }'; then
	printf 'Launcher runtime-core coverage must be at least 55.0%%; got %s%%\n' "$coverage" >&2
	exit 1
fi
printf 'Launcher runtime-core coverage: %s%%\n' "$coverage"
