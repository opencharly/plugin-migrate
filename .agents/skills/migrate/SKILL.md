---
name: migrate
description: |-
  The `charly migrate` command — the single idempotent reshape pass bringing any opencharly config forward to the current schema. Covers the declarative migration table, the op vocabulary, the `apply:` hooks, the gitignored backup tree, and how to add a migration step.
---

# charly migrate — the version-free reshape pass

`charly migrate` is **one idempotent command** that applies the declarative migration table to an opencharly config on disk. There are no sub-verbs to choose between: it walks the whole table in declaration order, and because every step is idempotent a re-run on an already-current project is a no-op.

```bash
charly migrate                 # apply the table to every reachable config
charly migrate --dry-run       # print every change migrate would make; touch nothing
charly migrate --project-only  # skip the per-host overlay
```

The project directory is the current working directory; use the top-level `-C` / `--dir` / `CHARLY_PROJECT_DIR` global to point elsewhere (`main()` chdir's before dispatch).

## There is no schema version

A config carries **no `version:` field**, and the schema is CLOSED: a `charly.yml` still carrying a retired key — `version:` above all — is an unknown field and **fails closed-CUE when it loads**. That load failure is the trigger to run `charly migrate`, and `charly migrate` is what removes the retired keys.

There is no schema HEAD, no floor, and no version comparison anywhere in the engine. `charly migrate` does not decide *whether* a project needs migrating — it reshapes every project it is pointed at, and idempotency is what makes that safe. A universal final step, **`strip-version-stamp`**, removes a leftover top-level or per-entity `version:` key: from the root `charly.yml`, from every other project document, and — unless `--project-only` — from the per-host overlay.

### Per-merge release git tags

Every landing of ANY repo carries a **fresh** annotated release git tag `v<YYYY.DDD.HHMM>` — ONE per merge, so a repo accumulates MULTIPLE `v…` tags over time. The tag is **generated and applied by the fresh `pr-validator` at MERGE**, from the current UTC merge time — never by the author at push time (author-time stamps collide and mis-order across concurrent out-of-sync PRs; see `/charly-internals:git-workflow` "CalVer"). Tag EVERY landing — a content change, a submodule extraction, an image drop. Tags are **immutable**: only ever ADD new ones, never move or force-push an existing tag. Every component is fixed-width zero-padded — 4-digit year, 3-digit day-of-year, 4-digit HHMM (`v2026.064.1937`, `v2026.142.1640`) — so tags sort chronologically under a plain alphanumeric sort; the evaluator computes `v$(date -u +%Y.%j.%H%M)` at merge (`%j` and `%H%M` are already zero-padded). Each repo — the superproject, every `box/<distro>` submodule, `plugins`, and `docs` — is tagged independently on its merged `main` HEAD (the tag marks the merge, so a repo needs no `charly.yml` to be tagged); a producer repo's PR merges + tags BEFORE its consumer's pointer bump. The SOLE exception is `sdk`, which tags under its Go-module `v0.<YYYYDDD>.<HHMM leading-zeros-stripped>` scheme (a semver requirement, not an exemption). The tag is independent of the migration table: the tag marks the MERGE, a migration entry marks the SCHEMA. See the project rulebook "Commit, push, land" (`AGENTS.md`) and `/charly-internals:git-workflow`.

## The migration table (declarative data)

The migration steps are **declarative DATA, not code**: an ordered list embedded from **`candy/plugin-migrate/migrations.cue`**, validated at process start against the `#Migration` schema in **`candy/plugin-migrate/schema/migration.cue`**. That schema is `@go(-)` — it generates no Go type — and lives IN the plugin: it is validation-only, read by this engine and by no charly core code, so per the kernel/plugin boundary law it stays out of the SDK contract. The list is ORDERED — the order the cutovers were authored in, which is the only correct replay order — and `charly migrate` applies every step, in order.

A step is:

```cue
{name: string, touches_host?: bool, ops?: [...#MigrationOp] | apply?: string}
```

`name` labels the step for `--dry-run` output and progress. `touches_host: true` flags a step that mutates per-host state (`~/.config/charly`, quadlets, `.secrets`) — those steps are skipped by remote-cache auto-migration (see below). A step carries EITHER an `ops:` list OR a single `apply:` hook, never both; that "exactly one of the two" rule is enforced in Go, since CUE's field-presence comparison is too fragile for the gate.

**The op vocabulary — each op is DATA, zero Go.** Four generic key transforms cover the common cutovers, all applied by ONE comment-preserving yaml.v3 interpreter (the op-walker in `candy/plugin-migrate/engine.go`):

| Op | Shape | Effect |
|---|---|---|
| `rename_key` | `{from, to, scope?="any", under_kind?}` | rename a mapping key `from` → `to` |
| `delete_key` | `{key, scope?="any", under_kind?}` | drop a mapping key |
| `remap_scalar` | `{key, from, to, under_kind?}` | rewrite a scalar VALUE `from` → `to` at `key` |
| `move_key` | `{key, from_parent, to_parent, under_kind?}` | relocate a key between parent mappings |

`scope` is `root` (top-level keys only) or `any` (every depth, the default; only `rename_key` / `delete_key` take it); `under_kind` further scopes an op to entities of a named kind (e.g. only `vm:` nodes). None of the four ops needs a line of new Go — the walker interprets the data.

**The `apply:` hook — the deliberate, visible EXCEPTION.** A structural reshape the four ops can't express (splitting one node into siblings, folding a list into a map) sets `apply: "<hook>"` naming a Go function registered in the plugin's `goHooks` map. This is the ONLY path that runs bespoke Go for a migration, and it is intentionally conspicuous: a table entry carrying `apply:` is the signal that this step needed real code.

**CUE has NO transformation capability.** CUE is monotone unification — it can validate, default, require, and close, and nothing more; it CANNOT rewrite a config. So CUE owns exactly ONE thing here: the SHAPE of the declarative table (`#Migration` validates every entry at startup). Every actual transform is Go — the generic op-walker, or an `apply:` hook. Do not expect CUE to migrate anything.

**The current table.** `candy/plugin-migrate/migrations.cue` is the authoritative list — read it there; each entry carries its own comment explaining the reshape and why the declarative ops could not express it. The entries run in the order the cutovers were authored in (the only correct replay order) and are:

| name | declarative ops | `apply:` hook | `touches_host` |
|---|---|---|---|
| `compact-node-form` | — | `compactNodeForm` | `true` |
| `strip-candy-libvirt-field` | — | `stripCandyLibvirtField` | — |
| `strip-deploy-shell-overlay` | — | `stripDeployShellOverlay` | `true` |
| `k8s-to-kubernetes` | 2 × `rename_key` | — | — |
| `remove-candy-localpkg` | `delete_key` | — | — |
| `install-template-to-phases` | — | `installTemplateToPhases` | — |
| `reshape-graphics-gl` | — | `reshapeGraphicsGL` | — |
| `record-field-to-instrument` | — | `recordFieldToInstrument` | — |
| `unroll-group-deploy` | — | `unrollGroupDeploy` | — |
| `deploy-cpu-spelling` | — | `reshapeDeployCPU` | `true` |
| `rekey-legacy-vm-overlay` | — | `rekeyLegacyVMOverlay` | `true` |

`compact-node-form` is the schema-compaction cutover: it folds the former named data/step child-node grammar into the compact node form — collections (`env`/`volume`/`port`/`service`/…) inline in the kind value, steps as the ordered `plan:` list (a meaningful former step-node name becomes the step's `id:`), deploy data (`add_candy`/`install_opts`/`vm_state`/…) inline in the substrate node, and every authored step `plugin:`+`plugin_input:` pair rewritten to the `<word>: <input>` verb sugar. Member children (sub-entities under a deployable kind) stay named children. Remote layer caches auto-migrate on fetch (see "Remote-cache auto-migration").

On top of the table the engine ALWAYS applies one further step, **`strip-version-stamp`** — it is not a table entry, so it needs no author (see "There is no schema version").

## How it runs

The migration engine is a **COMPILED-IN command plugin** (`candy/plugin-migrate`, `command:migrate`) — the engine, table, and CLI live in the plugin, out of charly core; core keeps only the load-gate HINT (the `Run: charly migrate` message a closed-schema load failure carries). `charly migrate` dispatches to the compiled-in `command:migrate` provider; the remote-cache auto-migration resolves the same provider and Invokes it (`OpRun --project-only`) in-proc. Compiled-in placement is load-bearing: migrate is whole-project file-based and must run when the config is exactly what cannot load, so it can never be discovered out-of-process.

`runMigrations` (`candy/plugin-migrate/engine.go`) walks the table in declaration order:

- **no `charly.yml` in the directory** → reports `no <file> in <dir> — nothing to migrate`.
- **otherwise** → applies every step (the generic op-walker or an `apply:` hook) in order, then the universal `strip-version-stamp`, and reports `nothing to migrate` when nothing changed.

**Backups are gitignored, never adjacent.** A step's per-file backup goes to `<project>/.charly/backups/<relpath>.<unix-ts>`, and the tree is kept out of git by a `<project>/.charly/.gitignore` containing a bare `*` (created on demand). A migration therefore never leaves `*.bak` files beside the originals and never dirties a working tree.

### Remote-cache auto-migration (project-only)

`sdk/loaderkit`'s refs seams auto-run the remote-cache migration on a freshly-cloned remote-repo cache so external repos pull through at the current schema. It skips every `touches_host` step and leaves the host-deploy path empty, so a remote fetch **never mutates the user's per-host state** — even the final `strip-version-stamp` touches only the cache's project files.

## Load-time enforcement: closedness

There is no version gate to trip. Compatibility has ONE mechanism — the **CLOSED CUE schema**: a retired key (a leftover `version:`, `kind: deployment`, `target: host`, `secret_backend: kdbx`, …) is an unknown field, and the load fails with a hard `field not allowed` diagnostic naming it. The same failure carries the `Run: charly migrate` hint, and `charly migrate` is what strips the key (see "There is no schema version"). Residual-key checks remain as defense-in-depth alongside the closedness gate.

## Adding a future migration

**This section is maintenance the charly project performs on its OWN schema.** Nothing in it is a step a charly user runs — the user-facing surface is the single command `charly migrate`, unchanged. It is recorded here because this skill owns the migration table.

The declarative engine's payoff: the common case needs **zero new Go**. Append ONE entry to **`candy/plugin-migrate/migrations.cue`**, expressing the transform as an `ops:` list (`rename_key` / `delete_key` / `remap_scalar` / `move_key`). Because the table is order-driven and version-free, there is no CalVer to bump and no cross-repo sequence — the entry IS the whole change, and it is idempotent by contract. A **structural reshape the four ops can't express** additionally registers ONE `goHooks` entry (a Go function named by the step's `apply:` field). Ship the entry in the same change that alters the schema, and update the repo's own versioned YAML.

The operator command never changes — it stays `charly migrate`.

**The validated schema is CUE-single-source.** The `@go()`-annotated `spec/schema/*.cue` defs are the sole source for the Go param structs (generated into `spec/spec` by `charly task cue-gen`); a wire-key change is a CUE edit first, then `charly task cue-gen`, then — only if it breaks existing on-disk configs — a migration-table entry here. A pure codegen refactor that leaves every authored wire key untouched needs NO migration entry. See the `/charly-internals:go` recipe "How to change the charly.yml schema (CUE is the single source of truth)".

### Standing rule: a schema/format change ships its migration entry AND mints a git tag

Any change to the YAML schema or composition format (a key rename, a deleted key, a new key shape) is a hard-cutover that MUST ship a matching migration-table entry in the same change, so `charly migrate` can carry an existing config across. A migration entry WITHOUT the matching schema change (or vice-versa) is forbidden.

Separately, **the fresh `pr-validator` mints the per-merge git tag** — `v<YYYY.DDD.HHMM>` computed from the MERGE moment (see "Per-merge release git tags" above). The tag marks the MERGE and every landing gets one, schema change or not; it is independent of the migration table, which marks the SCHEMA.

## Idempotency

Running `charly migrate` twice is a no-op: every step's old shape is absent the second time, so each does nothing. An op with no matching key, a `strip-version-stamp` on a file with no stamp, and an `apply:` hook on an already-reshaped node are all inert. The table's own invariant — every entry a valid `#Migration` — is validated against CUE at process start.

## See Also

- `/charly-image:layer` — the compact-node-form candy schema migrations produce
- `/charly-image:image` — `image:` entries + `charly box build/validate/inspect`
- `/charly-core:deploy` — the deploy entries a migration may rewrite
- `/charly-local:local-spec` — `kind: local` templates
- `/charly-build:secrets`, `/charly-build:settings` — the credential schema
- `/charly-internals:go` — loader internals (`LoadUnified` and the closedness gate; the `candy/plugin-migrate/engine.go` op-walker)
- `/charly-internals:cutover-policy` — why hard-cutover + a single idempotent `charly migrate` is the required shape
