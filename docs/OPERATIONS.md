# Operating the server

## Authentication and authorization

The default `--auth-mode local` trusts local clients and requires no credential.
Supplying Authorization on a non-probe request in this mode is an error, not
proof that a token was verified.

Use `--auth-mode tokens --auth-file private/tokens.json` for a shared installation.
The file defines stable principal UUIDs, `read` or `write` roles, and SHA-256 token
digests. Read allows resource GET/list. Write also allows POST, PUT, and DELETE.
These permissions cover the whole catalog; Maintainer contacts and creator links
do not grant ownership or access.

Generate a new configuration with PowerShell 7.2 or later:

```powershell
New-Item -ItemType Directory -Path .\private -Force | Out-Null
if (Test-Path .\private\tokens.json) { throw "Refusing to overwrite an existing credential file." }
$token = [Convert]::ToBase64String([Security.Cryptography.RandomNumberGenerator]::GetBytes(32)).TrimEnd('=').Replace('+', '-').Replace('/', '_')
$hash = [Convert]::ToHexString([Security.Cryptography.SHA256]::HashData([Text.Encoding]::UTF8.GetBytes($token))).ToLowerInvariant()
@{ principals = @(@{ id = [Guid]::NewGuid().ToString(); role = "write"; token_sha256 = @($hash) }) } |
	ConvertTo-Json -Depth 4 | Set-Content .\private\tokens.json -Encoding utf8
$token
```

The last line displays a new credential once. Save it securely for the client.
Do not commit tokens or credential configuration. The repository ignores
`private`, but that is not an access-control mechanism.

Restrict the file to its owner and administrators before use. On POSIX, use
owner-only permissions such as `chmod 600`. On Windows, set an owner/admin-only
ACL; portable Go mode bits do not verify NTFS permissions.

Start the server:

```powershell
go run .\cmd\metadata-server --auth-mode tokens --auth-file .\private\tokens.json
```

In a client terminal, set `$token` to the securely retained credential:

```powershell
Invoke-RestMethod http://127.0.0.1:8080/v1/maintainers -Headers @{ Authorization = "Bearer $token" }
```

Tokens are canonical unpadded base64url encodings of 32 random bytes. The file
stores SHA-256 of the UTF-8 token text, not of the decoded bytes. Use cryptographic
randomness, not a password. A digest cannot prove that the original token had
adequate entropy.

The file is one strict UTF-8 JSON object with a `principals` array. Each principal
has exactly `id`, `role`, and `token_sha256`. It is limited to 256 KiB, 128
principals, and one or two distinct lowercase hexadecimal digests per principal.
Malformed, duplicate, unknown, case-mismatched, or excessive configuration fails
before storage initialization or listening. Authentication never silently turns
off after a load failure.

The file loads once. To rotate, retain the same principal ID and temporarily list
old and new digests, restart, move clients, then remove the old digest and restart.
Revocation and role changes also require restart. Never reassign a principal ID
to a different person or automation. There is no browser login, token issuance
endpoint, identity-provider integration, hot reload, or tenant isolation.

Missing, malformed, repeated, or invalid credentials return `401 unauthenticated`.
A reader's mutation returns `403 forbidden` before body decoding or storage access.
GET `/healthz` and `/readyz` remain generic public probes with their own bounds.
Metadata and error responses use `Cache-Control: no-store`.

## Transport boundary

Both access modes bind only to literal loopback addresses or the reserved
`localhost` name. The latter maps directly to loopback without arbitrary DNS
resolution. Request Host must be local and match the actual listener port;
unacceptable authorities return `400 invalid_host`. Host may omit the port only
when the listener uses HTTP's default port 80.

This executable serves loopback HTTP, not public HTTPS. Remote use requires a
same-host TLS reverse proxy with certificate validation and its own connection
policy. Preserve Authorization and rewrite upstream Host to the local listener.
Forwarded identity, address, and scheme headers do not establish trust.

Loopback is not protection from a hostile administrator, compromised local
client, or someone who can modify the token file or database. Use tokens mode
when local clients are not all trusted.

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
