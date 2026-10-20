# Metadata API

A Go JSON API for Applications and their recorded creators, with SQLite persistence
and atomic conditional updates.

[API reference](openapi.json)

## Quick start

Build and run the server:

```powershell
go run .\cmd\metadata-server
```

It listens on `127.0.0.1:8080` and uses `data\metadata.db`. Stop with Ctrl+C.
Use `--listen` to change the address, `--db` for the SQLite path, or
`--storage memory` for volatile local state.

The process exposes `/healthz` and `/readyz`, bounds requests and shutdown, and
logs request IDs without payloads. It has no authentication or TLS; retain the
loopback default unless an authenticated TLS boundary protects it.

## Check the API and storage

Use Go 1.27.2. The module requires Go 1.26 or later.

```powershell
go test .\... -count=1
```

SQLite owns the schema, creator references, unique normalized emails, and atomic
revision-conditional mutations. It commits before returning success. WAL and FULL
synchronization support local durability; writers remain serialized.

An in-memory adapter implements the same contract for disposable state and tests.
The shared suite checks both implementations for CRUD, revision conflicts,
creator integrity, pagination, and record isolation. Memory is not durable.

## API contract

Both resource types support create, list, get, replace, and delete. An Application
has one immutable creator. Updating that creator's contact details does not rewrite
the Application.

**Atomic conditional writes prevent lost updates.** GET returns an ETag.
PUT and DELETE require `If-Match`; a stale condition is rejected rather than
overwriting newer data. PUT returns the transformed body without an ETag.
Strict JSON, bounded requests, and structured errors define the HTTP boundary.

## Real-process tests

Run the complete suite:

```powershell
go test .\... -count=1 -timeout=3m
```

The E2E tests build and start the server, then use HTTP to check CRUD, creator
integrity, concurrent conditional writes, restart behavior, and bounded failures.
They use isolated temporary data and stop their child processes.

Run the conflict-and-restart scenario on its own:

```powershell
go test .\e2e -run TestConcurrentWritersAcrossRestart -count=1 -v
```

SQLite retains committed data across a process restart. Memory does not.
The SIGTERM process test requires Linux and skips on Windows.

## Development note

The source was prototyped with AI assistance before this public repository was
created. These commits assemble that work as checked, logical delivery units.
