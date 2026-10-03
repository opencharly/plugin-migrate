# plugin-migrate

The config-schema migration engine for OpenCharly — the `charly migrate` command
that brings any project's `charly.yml` files to the current schema.

The plugin is a Go module at `candy/plugin-migrate/`, **compiled into** the
charly binary (listed in `compiled_plugins:`) so `command:migrate` has a registry
word at init time. Migration is whole-project and file-based, and must run when
the config is exactly what cannot load, so it can never be discovered
out-of-process. The `cmd/serve` binary backs the out-of-process placement for a
consumer that does not compile migrate in.

## What it provides

| Capability | Surface |
|---|---|
| `command:migrate` | `charly migrate` — the operator command AND the in-proc engine charly's remote-cache auto-migration invokes with `OpRun --project-only` |

The engine is the CUE-anchored declarative migration table plus the generic
op-walker (`rename_key` / `delete_key` / `remap_scalar` / `move_key`) and the
file-walk drivers. The migration DATA (`migrations.cue`) is embedded here; the
`#Migration` shape is read from the SDK schema. The below-floor / behind-head
load-gate hints (the "run charly migrate" messages) stay core — they only point
at this command.

## How to use it

Run it against a project directory:

```bash
charly migrate --dir <project>          # apply migrations in place
charly migrate --dir <project> --dry-run   # report the plan, touch nothing
charly migrate --dir <project> --project-only
```

Rollback backups are written to the gitignored `.charly/backups/` tree, never
adjacent to the migrated files. A versionless project is a no-op (idempotent).

## Layout

- `candy/plugin-migrate/` — the plugin module: `plugin.go` (provider + meta),
  `command.go`, `engine.go` (the op-walker + file drivers), `context.go`,
  `reshape_*.go` (the individual transforms), `migrations.cue` (the migration
  data), `schema/migrate.cue` + `schema/migration.cue`, and `cmd/serve/main.go`.
- `charly.yml` — the root project manifest (`discover: candy`) plus the
  `check-migrate-local` disposable R10 bed.
- `.github/workflows/tag-on-merge.yml` — CalVer tag + `CHANGELOG/` on merge.

## R10 witness

`charly check run check-migrate-local` — a disposable `kind:local` deploy on
`host: local` that runs `charly migrate` host-side against mktemp fixtures,
proving the strip-version-stamp step, idempotency, and `--dry-run` plumbing. The
declarative transform ops are covered by the plugin's `engine_test.go` units.

## Related

- Owning skill: `/charly-build:migrate` — the `charly migrate` reference (the
  plugin candy carries no `skill:` entity of its own; the gap is tracked in
  [opencharly/opencharly#291](https://github.com/opencharly/opencharly/issues/291)).
- `/charly-internals:plugin` — the plugin/provider model and compiled-in
  placement.
- [`opencharly/charly`](https://github.com/opencharly/charly) — the charly CLI.
