# Metadata API

Domain types and transactional SQLite persistence for Applications and their
recorded creators. This revision contains storage and validation tests, not a
runnable HTTP server.

## Check the storage adapters

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

## Development note

The source was prototyped with AI assistance before this public repository was
created. These commits assemble that work as checked, logical delivery units.
