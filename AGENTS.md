# AGENTS.md — plugin-migrate

Standalone plugin repo owning the config-schema migration engine
(`command:migrate`). The plugin is a Go module at `candy/plugin-migrate/`
(module path `github.com/opencharly/plugin-migrate/candy/plugin-migrate`); the
root `charly.yml` declares `discover: candy` (so the repo is a project and its
candy is scanned) and the `check-migrate-local` R10 witness bed.

Canonical files:

- `candy/plugin-migrate/charly.yml` — the `plugin-migrate:` candy entity
  (`plugin:` block, `plan:` check).
- `candy/plugin-migrate/` — the Go source: `plugin.go`, `command.go`,
  `engine.go` (op-walker + file drivers), `context.go`, `reshape_*.go`,
  `migrations.cue` (the migration data), `schema/migrate.cue`,
  `schema/migration.cue`, `cmd/serve/main.go`.
- `charly.yml` — the root manifest (`discover: candy`) + the `check-migrate-local`
  disposable R10 bed.
- `.github/workflows/tag-on-merge.yml` — CalVer tag + `CHANGELOG/` on merge.
- `README.md` — user overview only; never agent guidance.

## Load these skills first (R0)

- `/charly-internals:plugin` — the plugin authoring reference: the `plugin:`
  block, the `command` provider class, placement (this one is COMPILED-IN),
  the per-plugin CUE-schema contract.
- `/charly-build:migrate` — the `charly migrate` reference, the plugin's
  user-facing surface. Load before changing a transform or the CLI.
- `/charly-internals:go` — SDD, the schema → generated-code pipeline, and
  `charly task cue-gen`.
- `/charly-internals:git-workflow` — before any git/PR action.

## Build / validate / test

- `go build ./...` in `candy/plugin-migrate/` — compile the plugin module.
- `go test ./...` in `candy/plugin-migrate/` — the plugin's Go tests (the
  declarative transform ops, `engine_test.go`, the schema-serve seam).
- `charly box validate` at the repo root — the structural check (the candy +
  `plugin:` block, CUE schema).
- The merge gate is the **org-wide** `charly/pr-validator` (required check
  `validate / validate`, defined in `opencharly/.github`); this repo has **no**
  per-repo candy gate.
- The live R10 witness is `charly check run check-migrate-local` (declared in
  the root `charly.yml`).

## Modify this repo

- Keep migrate **compiled-in**: it must resolve when the config is exactly what
  cannot load, so it cannot be discovered out-of-process.
- Edit the `plugin-migrate:` candy entity, the Go source, the `migrations.cue`
  data, and the `schema/*.cue` **together** — the SDK `#Migration` shape is the
  single source for the migration data.
- Every new transform ships its `reshape_*_test.go` unit.

## Landing

Load `/charly-internals:git-workflow` before any git/PR action; it owns the
landing mechanics. The authoritative rulebook is the umbrella `AGENTS.md` in
`opencharly/opencharly` and `charly/AGENTS.md` in the charly repo.
