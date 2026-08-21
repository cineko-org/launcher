# Launcher behavior contract

This inventory records externally observable behavior. Deployment topology, hosts, credentials, and implementation-only paths are intentionally omitted.

## Compatibility and authentication

- Central messages use the generated `cineko.client` and `cineko.release` ProtoJSON contracts directly. Every response must carry a non-negative release generation header.
- Startup checks `GET /health` before asking for a PIN. Only `ready` continues.
- A six-digit PIN is exchanged with `POST /v1/auth/pin`. A supplied access credential uses `POST /v1/auth/exchange`.
- Authenticated calls use bearer access tokens. An expired or rejected access token is refreshed once with `POST /v1/auth/refresh`, then the original call is retried once.
- Rotated access and refresh tokens are atomically persisted before the refreshed call succeeds.
- `POST /v1/auth/logout` is best-effort; local session removal is idempotent.
- Invalid PIN, rate limiting, unavailable service, unauthorized session, stale release, and incompatible response are distinct failures. Raw response bodies and secrets are not user-visible.

## Central service points

| Operation | Service point | Preconditions | Success | Error and retry |
| --- | --- | --- | --- | --- |
| Health check | `GET /health` | Valid Central origin | Continue to identity/authentication | No retry inside one run; UI offers retry |
| PIN exchange | `POST /v1/auth/pin` | Six ASCII digits and stable installation/device IDs | Persist session | Invalid PIN returns to login; rate limit/unavailable is classified |
| Credential exchange | `POST /v1/auth/exchange` | User ID and access credential | Persist session | Unauthorized is terminal for this attempt |
| Refresh | `POST /v1/auth/refresh` | Unexpired refresh token | Persist rotated session and retry original request once | Failure is terminal for this attempt |
| Logout | `POST /v1/auth/logout` | Resumable session | Revoke server session and remove local session | Server failure does not prevent local removal |
| Session validation | `GET /v1/client/bootstrap` | Resumable authenticated session | Confirm the session still belongs to the same user | Unauthorized or mismatched identity clears the local session and returns to login |
| Device registration | `PUT /v1/devices/{installationId}` | Authenticated session and installation identity | Device metadata is current | Original call retries once after refresh |
| Launcher release | `GET /v1/releases/launcher/current` | Stable channel and current platform/architecture | Continue or require manual portable update | Invalid metadata is terminal |
| Runtime release | `GET /v1/releases/runtime/current` | Stable compatible Client, browser, and Playwright set | Verify/install exact set | Release changes restart preparation, at most three times |
| Launch ticket | `POST /v1/launch-tickets` | Exact generation and component identities | Single-use launch envelope | Nonce is the idempotency key; stale release restarts preparation |

## Local mutations and rollback

| Mutation | State owner | Commit point | Failure/rollback |
| --- | --- | --- | --- |
| Create identity | Launcher data directory | Atomic JSON replacement | Failed write leaves no partial identity |
| Persist session | Launcher data directory | Atomic owner-only JSON replacement | Refresh/request fails if rotated session cannot be saved |
| Download artifact | Versioned cache | Size and SHA-256 verified; resumable range data promoted atomically | Invalid/partial content is discarded or resumed; default deadline is 10 minutes |
| Install runtime | Versioned component directories and installed manifest | Archive limits, executable, tree hash, compatibility, and probe-key metadata validate before activation | Previous manifest is retained for rollback |
| Launch Client | Child process | Generated `cineko.client.LaunchEnvelope` ProtoJSON and a non-secret startup nonce are passed through standard input/environment; sensitive environment values are removed | Exit, timeout, invalid marker, or cancellation before startup readiness rolls back newly activated runtime |
| Finalize runtime | Installed manifest and component cache | A matching owner-only atomic startup marker proves the new Wails runtime installed its supervisors | Previous Client, Chromium, and Playwright artifacts are removed immediately after readiness; later Client exit does not roll back a runtime that already started successfully |
| Download portable Launcher | User-selected destination | Verified cache copied through atomic replacement | Existing destination survives a failed download/copy |

## Launcher UI mutations

Go owns authoritative state and monotonically increasing revisions. React renders only the newest revision.

| User action | Submit/in-flight | Success | Error/retry/rollback |
| --- | --- | --- | --- |
| Enter PIN | Disabled until six digits; button shows loading | PIN clears and update/start state replaces login | Go publishes a classified state; loading clears; user may retry |
| Retry startup | Existing error remains until a newer state arrives | Startup state machine resumes | Go publishes the next classified error |
| Download Launcher | Update-required state remains visible | Portable file is atomically written to the chosen location | Go publishes failure; an existing file is preserved |
| Quit | No optimistic local mutation | Process closes | Bridge rejection is contained; Go remains authoritative |

Modes are `checking`, `login`, `updating`, `launcher-update`, `launching`, and `error`. Runtime stages are `authenticating`, `checking`, `downloading`, `installing`, `launching`, and `running`.

## Supply-chain and drift boundary

- Go dependencies are selected by `go.mod`/`go.sum` and reproduced from committed `vendor/` metadata.
- Frontend dependencies are selected by `frontend/package-lock.json`; embedded assets are generated from `frontend/src` during the checked build.
- Release Please creates a tagged draft. The release stays unpublished until every platform artifact is built and attached; failed signing, notarization, or packaging leaves only the private draft.
- The macOS release is signed with the repository-scoped Developer ID Application identity, submitted to Apple notarization with a 10-minute timeout, stapled, and assessed before its final ZIP is created. The final ZIP is then extracted and its signature, ticket, and Gatekeeper assessment are verified again.
- macOS signing credentials are read only from repository secrets and are imported into an ephemeral keychain. Secret values are never printed and the keychain is deleted after the job.
- Checksums, GitHub Release upload, and Central registration consume the final stapled ZIP, never the unsigned submission bundle.
- Release artifacts require HTTPS metadata, positive size, clean executable path, exact SHA-256, bounded archive expansion, and verified installed tree hashes. ZIP and TAR may preserve relative symbolic links only when every resolved target stays inside the extracted component; absolute, escaping, cyclic, or link-parent paths fail before extraction writes files.
- `scripts/verify-behavior-contract.sh` fails when a Launcher/Central service point or state literal appears in source without this inventory.
