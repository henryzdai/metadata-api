# Metadata API

Go JSON handlers and storage adapters for Applications and their recorded creators.
This revision contains the HTTP package and its tests, not a runnable server command.

[API reference](openapi.json)

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

## Development note

The source was prototyped with AI assistance before this public repository was
created. These commits assemble that work as checked, logical delivery units.
