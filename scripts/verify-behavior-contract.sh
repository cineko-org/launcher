#!/usr/bin/env bash
set -euo pipefail

readonly document="docs/behavior-contract.md"
sources=()
while IFS= read -r source; do
	sources+=("$source")
done < <(rg --files -g '*.go' -g '*.ts' -g '*.tsx' -g '!vendor/**' -g '!**/assets/**')

while IFS= read -r value; do
	if [[ "$value" == "/v1/devices/" ]]; then
		continue
	fi
	grep -Fq "$value" "$document" || {
		printf 'Launcher behavior contract is missing %s\n' "$value" >&2
		exit 1
	}
done < <(rg -o --no-filename '/(health|v1/[A-Za-z0-9_./:-]*)' "${sources[@]}" | sort -u)

for value in '/v1/devices/{installationId}'; do
	grep -Fq "$value" "$document" || {
		printf 'Launcher behavior contract is missing templated service point %s\n' "$value" >&2
		exit 1
	}
done

while IFS= read -r value; do
	grep -Fq "\`$value\`" "$document" || {
		printf 'Launcher behavior contract is missing state %s\n' "$value" >&2
		exit 1
	}
done < <(rg -o --no-filename '(Mode|Stage) = "[a-z-]+"' --glob '*.go' --glob '!vendor/**' |
	sed -E 's/.*"([a-z-]+)"/\1/' | sort -u)
