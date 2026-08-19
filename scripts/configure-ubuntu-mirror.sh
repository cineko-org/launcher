#!/usr/bin/env bash
set -euo pipefail

for source in /etc/apt/apt-mirrors.txt /etc/apt/sources.list.d/ubuntu.sources; do
  if [[ -f "$source" ]]; then
    sudo sed -i \
      -E 's|https?://[^[:space:]]+/ubuntu|https://archive.ubuntu.com/ubuntu|g' \
      "$source"
  fi
done

printf '%s\n' \
  'Acquire::Retries "3";' \
  'Acquire::http::Timeout "20";' \
  'Acquire::https::Timeout "20";' \
  | sudo tee /etc/apt/apt.conf.d/80-cineko-ci >/dev/null
