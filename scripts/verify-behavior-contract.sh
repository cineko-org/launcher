#!/usr/bin/env bash
set -euo pipefail

test ! -e internal/centralclient/client.go
test ! -e internal/launcher/login.go
grep -Fq 'ReleaseBaseURL' internal/launcher/launcher.go
grep -Fq 'ClientPath' internal/launcher/launcher.go
! grep -Eq 'case "publish"|releasecontract publish' cmd/releasecontract/main.go
! grep -Fq 'replace github.com/cineko-org/contracts/v3' go.mod
grep -Fq 'github.com/cineko-org/contracts/v3 v3.7.0' go.mod

printf 'Local Launcher behavior boundary checks passed\n'
