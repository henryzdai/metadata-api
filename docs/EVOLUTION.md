# Development timeline

Dates use Eastern Time. Only milestones under Delivered claim completed
implementation and verification. The sequence below defines the order of work.

## Delivered

| Date | Milestone | Decision and tradeoff | Evidence |
| --- | --- | --- | --- |
| 2026-10-20 | MVP baseline preserved as `mvp-v1`. | One aggregate storage contract protects CRUD, revisions, and creator integrity. SQLite provides local durability; memory remains volatile. The server is local-first, without authentication or TLS. | [Baseline commit](https://github.com/henryzdai/metadata-api/commit/1bd8b645619d8d84b20957afa5fff187a6e3a25e), [MVP tag](https://github.com/henryzdai/metadata-api/tree/mvp-v1), and [passing Linux CI](https://github.com/henryzdai/metadata-api/actions/runs/39516044844). |
| 2026-10-20 | Post-MVP 1. Compiled storage plugins. | One immutable catalog now owns names, defaults, option aliases, prepared openers, and durability metadata. A separately registered test provider serves real HTTP without router changes. New providers require a rebuild and the complete shared contract; this is not a dynamic loader or external SDK. | [Implementation](https://github.com/henryzdai/metadata-api/commit/fa2a6b6a4012c2aeb658ed319d6ea6268fa60d99), [passing Linux CI](https://github.com/henryzdai/metadata-api/actions/runs/39527182495), and [storage guide](STORAGE.md). |
| 2026-10-20 | Post-MVP 2. Resource bounds. | Finite connection/request admission, atomic count/byte quotas, owned memory strings, and byte-bounded pages constrain named state and work. SQLite schema v2 preserves existing data and adds transactional accounting. These controls are not hard RSS/disk caps; same-file writers require matching limits and an offline upgrade. | [Implementation](https://github.com/henryzdai/metadata-api/commit/9ed75b3ddff5e0880f099e14b0cf110e1af8d827), [passing Linux CI](https://github.com/henryzdai/metadata-api/actions/runs/39533734067), and [operator guide](OPERATIONS.md). |
| 2026-10-20 | Post-MVP 3. Authentication and authorization. | A strict token-hash file defines stable principals and catalog-wide read/write roles. Host checks and current authorization precede resource handling. Creator contacts remain separate from identities. Local mode preserves development without credentials; token changes require restart, and remote use needs a TLS proxy. | [Implementation](https://github.com/henryzdai/metadata-api/commit/18beccf095d0c6497bfa434c87294fe23ed40350), [passing Linux CI](https://github.com/henryzdai/metadata-api/actions/runs/39538710232), and [access configuration](OPERATIONS.md#authentication-and-authorization). |

The MVP already includes strict input validation, business version and server
revision separation, atomic ETag/If-Match writes, request and page bounds,
request timeouts, graceful shutdown, readiness, and safe request logs.
Its recorded Linux statement coverage is 91.3 percent across `internal` and `cmd`,
excluding the separately launched E2E server process.

Post-MVP coverage follows the executable's module dependency graph. Shared test
runners and test-only providers do not inflate that application coverage.

## Post-MVP sequence

Complete and verify one runtime feature before starting the next. Each increment
updates the operator documentation, relevant API contract, tests, and this timeline.

| Order | Increment | Boundary to preserve |
| --- | --- | --- |
| 1 | Registerable compiled storage plugins. | One provider catalog and the complete aggregate storage contract. No dynamic shared-library loading. |
| 2 | Bounded server work and memory-backed state. | Explicit overload responses and tested limits, not an absolute OOM guarantee. |
| 3 | Authentication and authorization. | Authenticated principals are separate from creator contact records. Security configuration must fail explicitly. |
| 4 | Retry-safe POST creation. | Creation and replay evidence must commit atomically. Scope, payload conflicts, expiry, capacity, and restart behavior must be defined. |
| 5 | Rate limiting. | State and limiter scope must be bounded. Rejections need useful retry information. |

Existing validation, revision checks, timeouts, and shutdown behavior remain
regression gates throughout. A new backend or policy does not get a weaker contract.
