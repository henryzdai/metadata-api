# Storage plugins

Storage providers are compiled Go packages, not dynamically loaded libraries.
SQLite and memory are the built-in implementations. Adding a provider requires
rebuilding the executable, but does not require changing HTTP handlers.

## Select a provider

```powershell
go run .\cmd\metadata-server --storage sqlite --storage-option path=data/metadata.db
go run .\cmd\metadata-server --storage memory
```

`--db` remains an alias for SQLite's `path` option. Do not supply both the alias
and the generic option. Unknown providers, unknown options, repeated options,
and incompatible aliases fail before the server listens.

The selected provider owns option validation and defaults. Options are not
database connection strings unless that provider explicitly defines them as such.
SQLite still treats its path as a filesystem path, not a DSN.

## Add a compiled provider

1. Implement the complete [MetadataStore contract](../internal/metadata/store.go)
   in a package within this source module.
2. Return a [Provider descriptor](../internal/storage/registry.go) with its name,
   durability, option descriptions, and preparation function. Preparation validates
   options and returns an opener that captures typed configuration.
3. Register it at the executable's composition root through
   `builtin.NewRegistry(custom.Provider())`. The registry supplies accepted
   names, configuration, and startup metadata from that one catalog.
4. Run the shared [storage contract](../internal/storagetest/) from the adapter's
   tests, then exercise startup, CRUD, readiness, errors, and shutdown through
   a real listener.

The [SQLite provider](../internal/storage/sqlite/provider.go) and
[memory provider](../internal/storage/memory/provider.go) are working registration
examples. The [separate test provider](../internal/testsupport/probe/provider.go)
and its [real-HTTP test](../internal/server/provider_test.go) prove that another
compiled package can be selected without changing the router.

Providers must preserve atomic revision checks, unique contacts, immutable creator
references, and the documented error precedence. They must not return success
before committing, ignore cancellation, or substitute another backend after an
open failure. The durability descriptor is a promise to test, not a guess from
the provider's name.

The contract remains under Go's `internal` boundary. This is an in-repository
extension mechanism, not a versioned third-party plugin SDK. That keeps
deployment portable and lets the compiler reject incomplete implementations.
