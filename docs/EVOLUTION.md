# Development timeline

Dates use Eastern Time. Delivered milestones have implementation and verification
evidence. Planned work below is not a claim that those features already exist.

## Delivered

| Date | Milestone | Decision and tradeoff | Evidence |
| --- | --- | --- | --- |
| 2026-10-20 | MVP baseline preserved as `mvp-v1`. | One aggregate storage contract protects CRUD, revisions, and creator integrity. SQLite provides local durability; memory remains volatile. The server is local-first, without authentication or TLS. | [Baseline commit](https://github.com/henryzdai/metadata-api/commit/1bd8b645619d8d84b20957afa5fff187a6e3a25e), [MVP tag](https://github.com/henryzdai/metadata-api/tree/mvp-v1), and [passing Linux CI](https://github.com/henryzdai/metadata-api/actions/runs/39516044844). |

The MVP already includes strict input validation, business version and server
revision separation, atomic ETag/If-Match writes, request and page bounds,
request timeouts, graceful shutdown, readiness, and safe request logs.
Its recorded Linux statement coverage is 91.3 percent across `internal` and `cmd`,
excluding the separately launched E2E server process.

## Post-MVP sequence

Complete and verify one runtime feature before starting the next. Each increment
updates the operator documentation, relevant API contract, tests, and this timeline.

| Order | Planned improvement | Boundary to preserve |
| --- | --- | --- |
| 1 | Registerable compiled storage plugins. | One provider catalog and the complete aggregate storage contract. No dynamic shared-library loading. |
| 2 | Bounded server work and memory-backed state. | Explicit overload responses and tested limits, not an absolute OOM guarantee. |
| 3 | Authentication and authorization. | Authenticated principals are separate from creator contact records. Security configuration must fail explicitly. |
| 4 | Retry-safe POST creation. | Creation and replay evidence must commit atomically. Scope, payload conflicts, expiry, capacity, and restart behavior must be defined. |
| 5 | Rate limiting. | State and limiter scope must be bounded. Rejections need useful retry information. |

Existing validation, revision checks, timeouts, and shutdown behavior remain
regression gates throughout. A new backend or policy does not get a weaker contract.
