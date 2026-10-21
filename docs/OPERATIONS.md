# Operating the server

## Resource limits

The server bounds named work and retained state. These limits reduce exhaustion
risk; they are not an absolute OOM or physical-disk-size guarantee.

| Limit | Default | Configuration |
| --- | --- | --- |
| Accepted open TCP connections | 128 | `--max-connections`, from 1 through 2048. |
| Active metadata requests | 16 | `--max-inflight`, from 1 through 256. |
| Active health/readiness probes | 4 | Separate fixed allowance. |
| Maintainers | 10000 | `--max-maintainers`, from 1 through 1000000. |
| Applications | 10000 | `--max-applications`, from 1 through 1000000. |
| Normalized resource fields | 64 MiB | `--max-metadata-bytes`, from 1 through 1073741824 bytes. |
| Encoded list response | 1 MiB | Fixed, alongside the existing maximum of 100 items. |

Zero is not an unlimited setting. Existing body, header, request-timeout, and
shutdown bounds still apply.

For a smaller local budget:

```powershell
go run .\cmd\metadata-server --max-inflight 8 --max-metadata-bytes 33554432
```

The field-byte budget counts normalized user strings, including an Application's
recorded creator ID. Record counts separately bound generated IDs, timestamps,
and per-record state. Memory owns normalized string copies instead of retaining
large trimmed backing allocations.

Full request admission returns `503 overloaded` with `Retry-After: 1` before
reading the request body. Probes use their own bounded allowance. An excess TCP
connection closes before HTTP handling, so it cannot receive a JSON error.

A growing create or replacement beyond storage policy returns
`503 resource_capacity_exceeded`. Rejected writes leave records, revisions, and
indexes unchanged. Delete or reduce data, or choose an appropriate larger limit.
Immediate retries do not themselves free storage capacity.

List pages can contain fewer records than `limit` requests. Follow `next_after`
until it is absent. Byte budgeting never truncates fields or silently skips the
first record that did not fit.

Logical field quotas do not measure total process memory. The runtime, stacks,
maps, indexes, JSON encoding, driver allocations, and filesystem caches need
headroom. Use an appropriate process/container memory budget and log/disk policy.
SQLite file and WAL size are not hard-capped by these logical limits, and deleting
rows need not shrink the file.

## SQLite upgrades and shared files

Stop every process using a SQLite file before upgrading it. Schema version 2 adds
resource accounting in a transaction without rewriting resource IDs, revisions,
timestamps, or creator links. A failed or over-limit migration leaves old data
intact rather than deleting records to fit.

All processes sharing one file must use matching resource limits. Configuration
agreement is an operator requirement, not a distributed coordination feature.
Opening with limits below existing usage fails before the server listens.

Mixed-version writers are unsupported. An old executable rejects a newer schema
when reopening, but an already-open old writer cannot be retroactively taught new
accounting rules. Downgrade requires a compatible pre-upgrade backup. Do not lower
`user_version` manually or discard accounting tables to force a downgrade.
