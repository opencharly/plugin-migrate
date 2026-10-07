package migrate

// engine.go — the shape-driven, VERSION-FREE declarative migration engine
// (`charly migrate`). There are no schema HEAD/floor and no version stamp: the
// authored `version:` field was removed from the schema, so a charly.yml still
// carrying it is an unknown field and fails closed-CUE. `charly migrate` is a
// pure, idempotent reshape pass. There are two moving parts:
//
//   1. A declarative migration TABLE (migrations.cue), validated at process start
//      against #Migration and interpreted by ONE generic op-walker. It is an
//      ORDERED list — steps run in declaration order and each MUST be idempotent.
//   2. A universal final `strip-version-stamp` step that removes the top-level and
//      per-entity `version:` keys a pre-cutover file may still carry.
//
// A future migration is DATA (rename_key / delete_key / remap_scalar / move_key) —
// zero new Go for the common case; a structural reshape registers one goHooks entry.

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/cuecontext"
	cueerrors "cuelang.org/go/cue/errors"
	"gopkg.in/yaml.v3"

	"github.com/opencharly/sdk/kit"
	"github.com/opencharly/spec/spec"
)

// migCtx is the plugin-local CUE context (this plugin owns the migration engine, so it
// no longer shares charly's core cueSchemaCtx). migrationSchema is #Migration compiled
// standalone from this plugin's OWN schema/migration.cue (embedded below — a plugin-only
// schema living in its plugin per the kernel/plugin boundary law, not the SDK contract) —
// it pulls neither charly's full ingress schema nor any SDK version def (there is none).

//go:embed schema/migration.cue
var migrationSchemaCUE []byte

var (
	migCtx          = cuecontext.New()
	migrationSchema = compileMigrationDefs()
)

func compileMigrationDefs() cue.Value {
	v := migCtx.CompileString(string(migrationSchemaCUE))
	if v.Err() != nil {
		panic(fmt.Sprintf("compileMigrationDefs: #Migration schema does not compile: %v", cueerrors.Details(v.Err(), nil)))
	}
	return v
}

// migrationsCUE is the declarative migration table (charly/migrations.cue). It is
// engine DATA, not ingress schema — it lives outside sdk/schema/ so it never
// enters the spec codegen / vocab concatenation.
//
//go:embed migrations.cue
var migrationsCUE []byte

// migration is one decoded table step. Exactly one of Ops / Apply is set.
type migration struct {
	Name        string
	TouchesHost bool
	Ops         []migrationOp
	Apply       string // names a goHooks entry (the structural-reshape escape hatch)
}

// migrationOp is one declarative op. The relevant fields depend on Op (validated
// against #MigrationOp's closed arms in migration.cue before decode).
type migrationOp struct {
	Op         string `json:"op"`
	From       string `json:"from"`
	To         string `json:"to"`
	Key        string `json:"key"`
	Scope      string `json:"scope"`
	UnderKind  string `json:"under_kind"`
	FromParent string `json:"from_parent"`
	ToParent   string `json:"to_parent"`
}

// goHooks holds the structural-reshape escape hatches a declarative step names via
// `apply:`. Registered HERE in the literal (never via init() — the migrationTable
// var below validates hook names during var initialization, which precedes every
// init() but follows this literal in dependency order).
var goHooks = map[string]func(*yaml.Node) bool{
	"compactNodeForm":         compactNodeForm,         // the schema-compaction reshaper (reshape_compact.go)
	"stripCandyLibvirtField":  stripCandyLibvirtField,  // candy-level libvirt: field removal (reshape_strip_candy_libvirt.go)
	"stripDeployShellOverlay": stripDeployShellOverlay, // deploy-scope shell: overlay field removal (reshape_strip_deploy_shell.go)
	"installTemplateToPhases": installTemplateToPhases, // format/builder install_template → phase.install.container move (reshape_install_template_to_phases.go)
	"reshapeGraphicsGL":       reshapeGraphicsGL,       // vm libvirt.devices.graphics[].gl scalar → {enable} mapping (reshape_graphics_gl.go)
	"recordFieldToInstrument": recordFieldToInstrument, // deploy record: field → instrument: entry harvest (reshape_record_field.go)
	"unrollGroupDeploy":       unrollGroupDeploy,       // targetless deploy group: node → primary substrate + deploy-level siblings (reshape_group_deploy.go)
	"reshapeDeployCPU":        reshapeDeployCPU,        // deploy override cpus: → cpu: direct-child rename, path-scoped (reshape_deploy_cpu.go)
	"rekeyLegacyVMOverlay":    rekeyLegacyVMOverlay,    // per-host overlay legacy vm:<identity> deploy keys → drop-on-twin / hard-error (reshape_rekey_vm_overlay.go)
	"reshapePipelineLobster":  reshapePipelineLobster,  // legacy kind:pipeline stages: grammar → lobster steps:/verb-sugar grammar (reshape_pipeline_lobster.go)
	"retireEmptyGroupNode":    retireEmptyGroupNode,    // barren `group:` node with nothing to promote → retired (reshape_retire_empty_group.go)
}

// migrationTable is the validated, declaration-ordered step list, loaded once at
// process start. A malformed table panics here (fail-fast, like registerCueKind).
var migrationTable = loadMigrationTable()

// loadMigrationTable compiles migrations.cue, validates each entry against
// #Migration (from the compiled plugin schema), enforces exactly-one ops/apply + a
// registered hook name, and decodes. The table is a plain ordered list — there is no
// version to validate.
func loadMigrationTable() []migration {
	v := migCtx.CompileString(string(migrationsCUE))
	if v.Err() != nil {
		panic(fmt.Sprintf("migrations.cue failed to compile: %v", cueerrors.Details(v.Err(), nil)))
	}
	list := v.LookupPath(cue.ParsePath("migrations"))
	if !list.Exists() {
		panic("migrations.cue: missing top-level `migrations:` list")
	}
	migDef := migrationSchema.LookupPath(cue.ParsePath("#Migration"))
	if migDef.Err() != nil {
		panic(fmt.Sprintf("#Migration schema not found: %v", migDef.Err()))
	}
	iter, err := list.List()
	if err != nil {
		panic(fmt.Sprintf("migrations.cue: `migrations:` is not a list: %v", err))
	}
	var out []migration
	for i := 0; iter.Next(); i++ {
		elem := iter.Value()
		if verr := elem.Unify(migDef).Validate(cue.Concrete(true)); verr != nil {
			panic(fmt.Sprintf("migrations.cue: step %d invalid: %v", i, cueerrors.Details(verr, nil)))
		}
		var raw struct {
			Name        string        `json:"name"`
			TouchesHost bool          `json:"touches_host"`
			Ops         []migrationOp `json:"ops"`
			Apply       string        `json:"apply"`
		}
		if derr := elem.Decode(&raw); derr != nil {
			panic(fmt.Sprintf("migrations.cue: step %d decode: %v", i, derr))
		}
		if raw.Name == "" {
			panic(fmt.Sprintf("migrations.cue: step %d has an empty name", i))
		}
		if (len(raw.Ops) > 0) == (raw.Apply != "") {
			panic(fmt.Sprintf("migrations.cue: step %q must set EXACTLY one of `ops:` or `apply:`", raw.Name))
		}
		if raw.Apply != "" {
			if _, ok := goHooks[raw.Apply]; !ok {
				panic(fmt.Sprintf("migrations.cue: step %q names unknown Go hook %q (register it in goHooks)", raw.Name, raw.Apply))
			}
		}
		out = append(out, migration{Name: raw.Name, TouchesHost: raw.TouchesHost, Ops: raw.Ops, Apply: raw.Apply})
	}
	return out
}

// runMigrations applies EVERY table step in declaration order to the project files
// (and, unless projectOnly, to the per-host overlay for touches_host steps), then runs
// the universal strip-version-stamp step. Each step is idempotent, so running the whole
// set is a no-op on an already-current file. Returns whether anything changed.
func runMigrations(ctx *MigrateContext, projectOnly bool) (bool, error) {
	if ctx == nil {
		return false, errors.New("migrate: nil context")
	}
	out := ctx.Out
	if out == nil {
		out = io.Discard
	}

	rootPath := filepath.Join(ctx.Dir, spec.UnifiedFileName)
	if _, err := os.ReadFile(rootPath); err != nil {
		if os.IsNotExist(err) {
			_, _ = fmt.Fprintf(out, "no %s in %s — nothing to migrate\n", spec.UnifiedFileName, ctx.Dir)
			return false, nil
		}
		return false, fmt.Errorf("reading %s: %w", rootPath, err)
	}

	var applied []string
	for _, m := range migrationTable {
		transform, terr := buildTransform(m)
		if terr != nil {
			return len(applied) > 0, terr
		}
		files, ferr := runDocMigration(ctx.Dir, ctx.Dir, ctx.DryRun, kit.OpUnifyCandidateFiles, transform, ctx.Out)
		if ferr != nil {
			return len(applied) > 0, ferr
		}
		hostChanged := false
		if m.TouchesHost && !projectOnly {
			hostChanged, ferr = migrateHostOverlayDoc(ctx, transform)
			if ferr != nil {
				return len(applied) > 0, ferr
			}
		}
		if len(files) > 0 || hostChanged {
			applied = append(applied, m.Name)
			_, _ = fmt.Fprintf(out, "applied %s\n", m.Name)
		}
	}
	stripped, serr := stripVersionStamp(ctx, projectOnly)
	if serr != nil {
		return len(applied) > 0, serr
	}
	if len(stripped) > 0 {
		applied = append(applied, "strip-version-stamp")
		_, _ = fmt.Fprintf(out, "applied strip-version-stamp\n")
	}
	if len(applied) > 0 {
		_, _ = fmt.Fprintf(out, "migrated\n")
	} else {
		_, _ = fmt.Fprintf(out, "nothing to migrate\n")
	}
	return len(applied) > 0, nil
}

// buildTransform returns the per-document transform for a step: the generic
// op-walker for a declarative step, or the named Go hook for an `apply:` step.
func buildTransform(m migration) (func(*yaml.Node) bool, error) {
	if m.Apply != "" {
		hook, ok := goHooks[m.Apply]
		if !ok {
			return nil, fmt.Errorf("migration %q: unknown Go hook %q", m.Name, m.Apply)
		}
		return hook, nil
	}
	ops := m.Ops
	return func(doc *yaml.Node) bool {
		root := kit.MappingRoot(doc)
		if root == nil {
			return false
		}
		changed := false
		for _, op := range ops {
			if applyOp(root, op) {
				changed = true
			}
		}
		return changed
	}, nil
}

// applyOp applies one declarative op to the target mappings selected by its scope /
// under_kind. Returns whether anything changed.
func applyOp(root *yaml.Node, op migrationOp) bool {
	changed := false
	for _, m := range opTargetMappings(root, op) {
		if applyOpToMapping(m, op) {
			changed = true
		}
	}
	return changed
}

// opTargetMappings selects the mapping nodes an op applies to:
//   - under_kind K: every mapping that is (or is nested within) an entity value
//     carrying a direct `K:` discriminator key;
//   - scope root: the document root mapping only;
//   - scope any (default): every mapping in the tree.
func opTargetMappings(root *yaml.Node, op migrationOp) []*yaml.Node {
	if op.UnderKind != "" {
		var out []*yaml.Node
		var rec func(n *yaml.Node, inside bool)
		rec = func(n *yaml.Node, inside bool) {
			if n == nil {
				return
			}
			switch n.Kind {
			case yaml.MappingNode:
				here := inside || mappingHasKey(n, op.UnderKind)
				if here {
					out = append(out, n)
				}
				for i := 0; i+1 < len(n.Content); i += 2 {
					rec(n.Content[i+1], here)
				}
			case yaml.DocumentNode, yaml.SequenceNode:
				for _, c := range n.Content {
					rec(c, inside)
				}
			}
		}
		rec(root, false)
		return out
	}
	if op.Scope == "root" {
		return []*yaml.Node{root}
	}
	return allMappings(root)
}

// applyOpToMapping applies an op to a single mapping node (comment-preserving).
func applyOpToMapping(m *yaml.Node, op migrationOp) bool {
	if m == nil || m.Kind != yaml.MappingNode {
		return false
	}
	switch op.Op {
	case "rename_key":
		// Under an under_kind scope where the renamed key IS the kind, the mapping's
		// first key is the kind DISCRIMINATOR of the entity that established the
		// scope (compact node form: first child key = kind). It must never be
		// renamed — only same-named fields nested inside the entity (e.g. the
		// deploy-knobs block) are. Skip it; a mapping whose first key is not the
		// kind is a plain nested field map and searches from index 0 as usual.
		start := 0
		if op.UnderKind != "" && op.From == op.UnderKind && len(m.Content) >= 2 && m.Content[0].Value == op.From {
			start = 2
		}
		for i := start; i+1 < len(m.Content); i += 2 {
			if m.Content[i].Value == op.From {
				m.Content[i].Value = op.To
				return true
			}
		}
	case "delete_key":
		for i := 0; i+1 < len(m.Content); i += 2 {
			if m.Content[i].Value == op.Key {
				// carry the deleted key's head comment onto the following key so a
				// section banner is not lost.
				if i+2 < len(m.Content) && m.Content[i].HeadComment != "" && m.Content[i+2].HeadComment == "" {
					m.Content[i+2].HeadComment = m.Content[i].HeadComment
				}
				m.Content = append(m.Content[:i], m.Content[i+2:]...)
				return true
			}
		}
	case "remap_scalar":
		for i := 0; i+1 < len(m.Content); i += 2 {
			if m.Content[i].Value == op.Key {
				if v := m.Content[i+1]; v.Kind == yaml.ScalarNode && v.Value == op.From {
					v.Value = op.To
					return true
				}
			}
		}
	case "move_key":
		from := childMapping(m, op.FromParent)
		to := childMapping(m, op.ToParent)
		if from == nil || to == nil {
			return false
		}
		for i := 0; i+1 < len(from.Content); i += 2 {
			if from.Content[i].Value == op.Key {
				k, v := from.Content[i], from.Content[i+1]
				from.Content = append(from.Content[:i:i], from.Content[i+2:]...)
				to.Content = append(to.Content, k, v)
				return true
			}
		}
	}
	return false
}

// allMappings returns every mapping node in the tree (document root first).
func allMappings(n *yaml.Node) []*yaml.Node {
	var out []*yaml.Node
	var rec func(*yaml.Node)
	rec = func(n *yaml.Node) {
		if n == nil {
			return
		}
		if n.Kind == yaml.MappingNode {
			out = append(out, n)
		}
		for _, c := range n.Content {
			rec(c)
		}
	}
	rec(n)
	return out
}

// mappingHasKey reports whether mapping m has a direct child key named key.
func mappingHasKey(m *yaml.Node, key string) bool {
	if m == nil || m.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return true
		}
	}
	return false
}

// childMapping returns the mapping value of key in m, or nil.
func childMapping(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key && m.Content[i+1].Kind == yaml.MappingNode {
			return m.Content[i+1]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// universal version-stamp removal
// ---------------------------------------------------------------------------

// universalStampFiles are the project files that historically carried a top-level
// schema `version:` stamp. In the single-filename world that is charly.yml alone
// (box/candy manifests carry a per-ENTITY version nested under their kind value,
// never a top-level stamp).
var universalStampFiles = []string{spec.UnifiedFileName}

// stripVersionStamp is the universal final migration step: it REMOVES the top-level
// `version:` key from every universalStampFiles entry (and, unless projectOnly, the
// per-host overlay), then removes the per-entity `version:` key from every authored
// candy/box/deploy entity body in the project files (and the overlay). Returns the
// changed paths. Idempotent — a file with no version keys is untouched.
func stripVersionStamp(ctx *MigrateContext, projectOnly bool) ([]string, error) {
	var changed []string
	for _, name := range universalStampFiles {
		did, err := stripVersionField(ctx.Dir, filepath.Join(ctx.Dir, name), ctx.DryRun, ctx.Out)
		if err != nil {
			return changed, err
		}
		if did {
			changed = append(changed, name)
		}
	}
	// The top-level `version:` stamp also rides every NON-root project document —
	// candy/<name>/charly.yml, box/<name>/charly.yml, and imported project
	// manifests (e.g. a distro repo's candy/…/charly.yml). Sweep every candidate
	// document's TOP-LEVEL stamp too, not only the root charly.yml above.
	// stripVersionField is idempotent (a file with no top-level stamp is
	// untouched), so re-visiting the root is a no-op.
	for _, p := range kit.OpUnifyCandidateFiles(ctx.Dir) {
		did, err := stripVersionField(ctx.Dir, p, ctx.DryRun, ctx.Out)
		if err != nil {
			return changed, err
		}
		if did {
			changed = append(changed, p)
		}
	}
	if !projectOnly && ctx.HostDeployPath != "" {
		did, err := stripVersionField(ctx.Dir, ctx.HostDeployPath, ctx.DryRun, ctx.Out)
		if err != nil {
			return changed, err
		}
		if did {
			changed = append(changed, ctx.HostDeployPath)
		}
	}
	files, err := runDocMigration(ctx.Dir, ctx.Dir, ctx.DryRun, kit.OpUnifyCandidateFiles, stripEntityVersionKey, ctx.Out)
	if err != nil {
		return changed, err
	}
	changed = append(changed, files...)
	if !projectOnly && ctx.HostDeployPath != "" {
		hostChanged, herr := rewriteDocFile(ctx.Dir, ctx.HostDeployPath, ctx.DryRun, stripEntityVersionKey, ctx.Out)
		if herr != nil {
			return changed, herr
		}
		if hostChanged {
			changed = append(changed, ctx.HostDeployPath)
		}
	}
	return changed, nil
}

// ---------------------------------------------------------------------------
// migration rollback backups — ONE gitignored location under the migrate root
// ---------------------------------------------------------------------------

const (
	// migrationStateDir is the per-project migrate state dir. Its own .gitignore
	// ignores the WHOLE tree (see migrationGitignore), so nothing under it ever
	// appears in `git status` — in ANY repo, with no per-repo .gitignore edit.
	migrationStateDir = ".charly"
	// migrationGitignore is the content of <root>/.charly/.gitignore. A bare `*`
	// ignores every entry in .charly/ (including the .gitignore itself), which
	// makes the untracked directory invisible to `git status` entirely.
	migrationGitignore = "*\n"
)

// writeMigrationBackup writes a rollback copy of data for path into the ONE
// gitignored backup location under root:
//
//	<root>/.charly/backups/<relpath-from-root>.<unix-ts>
//
// It idempotently ensures <root>/.charly/.gitignore exists with migrationGitignore
// (a bare `*`), so every rollback copy is invisible to `git status` in every repo
// WITHOUT a per-repo .gitignore change. The backups stay LOCAL to the tree they
// protect (never an XDG cache).
//
// A path inside root is keyed by its root-relative path. A path OUTSIDE root (the
// per-host overlay lives at ~/.config/charly/charly.yml, not under the project)
// is keyed under a synthetic `external/` prefix so it too stays inside the one
// backup tree — it can never escape via `..`. The result is a traversal-free
// relpath: the backup NEVER lands outside <root>/.charly/backups/. A path that
// cleans to root itself is refused (nothing to protect; "" is returned).
//
// The ONE helper both rewrite call sites share (R3) — the top-level version-strip
// and the node-form document rewrite. Returns the backup path written ("" when the
// path cleans to root).
func writeMigrationBackup(root, path string, data []byte) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolving migrate root %s: %w", root, err)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", path, err)
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return "", fmt.Errorf("relativizing %s under %s: %w", absPath, absRoot, err)
	}
	if rel == "." {
		return "", nil // path IS the root — nothing to back up
	}
	// Path-safety: keep the key traversal-free. A path outside root (or a crafted
	// `../…`) is re-keyed under `external/` from its cleaned absolute path, which
	// filepath.Abs+Clean guarantees carries no `..` components — so the backup can
	// never resolve outside <root>/.charly/backups/.
	if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		rel = filepath.Join("external", strings.TrimPrefix(filepath.ToSlash(absPath), "/"))
	}

	stateDir := filepath.Join(absRoot, migrationStateDir)
	gitignore := filepath.Join(stateDir, ".gitignore")
	if _, err := os.Stat(gitignore); os.IsNotExist(err) {
		if err := os.MkdirAll(stateDir, 0o755); err != nil {
			return "", fmt.Errorf("creating %s: %w", stateDir, err)
		}
		if err := os.WriteFile(gitignore, []byte(migrationGitignore), 0o644); err != nil {
			return "", fmt.Errorf("writing %s: %w", gitignore, err)
		}
	}

	// Nanosecond resolution: ONE `charly migrate` run rewrites a given file in more
	// than one step (the top-level stamp step, then the entity-stamp step), so a
	// whole-second stamp would let a later backup silently clobber the earlier one
	// and lose the pre-migration rollback content.
	backup := fmt.Sprintf("%s.%d", filepath.Join(stateDir, "backups", rel), time.Now().UnixNano())
	if err := os.MkdirAll(filepath.Dir(backup), 0o755); err != nil {
		return "", fmt.Errorf("creating backup dir for %s: %w", backup, err)
	}
	if err := os.WriteFile(backup, data, 0o644); err != nil {
		return "", fmt.Errorf("writing backup %s: %w", backup, err)
	}
	return backup, nil
}

// stripVersionField DELETES the first top-level `version:` line of one AUTHORED
// charly manifest (spec.UnifiedFileName). Returns (changed, err); changed is false
// when the file is absent, is not a charly manifest, or has no top-level `version:`
// key. A rollback copy is written to the gitignored <root>/.charly/backups/ tree
// (never adjacent) before any rewrite, and its path is printed to out.
//
// The manifest-name guard is load-bearing: the top-level `version:` is a charly
// SCHEMA stamp ONLY in a charly.yml. Every other candidate YAML a `version:` key
// belongs to a DIFFERENT tool — `.golangci.yml`'s `version: "2"` is golangci-lint's
// config-schema version. The candidate sweep (kit.OpUnifyCandidateFiles) covers
// every root-level *.yml/*.yaml sibling, so without this guard `charly migrate`
// silently deleted that key from unrelated tooling configs
// (opencharly/plugin-migrate#16).
func stripVersionField(root, path string, dryRun bool, out io.Writer) (bool, error) {
	if filepath.Base(path) != spec.UnifiedFileName {
		return false, nil // not an authored charly manifest — leave foreign keys alone
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading %s: %w", path, err)
	}
	lines := strings.Split(string(data), "\n")
	idx := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "version:") {
			idx = i
			break
		}
	}
	if idx == -1 {
		return false, nil // no top-level version: key
	}
	if dryRun {
		return true, nil
	}
	if backup, berr := writeMigrationBackup(root, path, data); berr != nil {
		return false, berr
	} else if backup != "" && out != nil {
		_, _ = fmt.Fprintf(out, "backup: %s\n", backup)
	}
	lines = append(lines[:idx], lines[idx+1:]...)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		return false, fmt.Errorf("writing %s: %w", path, err)
	}
	return true, nil
}

// stripEntityVersionKey removes a direct `version:` child from every authored
// candy/box/deploy entity body (the per-entity stamp that lived under the kind
// discriminator). It mirrors stripCandyLibvirtField's walk style: a mapping that
// directly carries a `candy:`/`box:`/`deploy:` key is an entity wrapper, and the
// version is a direct child of that kind's value mapping — never a same-named key
// nested deeper inside the entity.
func stripEntityVersionKey(doc *yaml.Node) bool {
	root := doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	return stripEntityVersionKeyRec(root)
}

// stripEntityVersionKeyRec walks the whole document tree (entity member nesting is a
// general document capability, so it recurses defensively) and removes a direct
// `version:` child of every candy/box/deploy entity body it finds.
func stripEntityVersionKeyRec(n *yaml.Node) bool {
	if n == nil {
		return false
	}
	changed := false
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			if stripEntityVersionKeyRec(c) {
				changed = true
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, val := n.Content[i], n.Content[i+1]
			switch key.Value {
			case "candy", "box", "deploy":
				if val.Kind == yaml.MappingNode && deleteDirectChildKey(val, "version") {
					changed = true
				}
			}
			if stripEntityVersionKeyRec(val) {
				changed = true
			}
		}
	}
	return changed
}

// ---------------------------------------------------------------------------
// generic file-walk drivers (relocated verbatim from the retired candy — R3)
// ---------------------------------------------------------------------------

// runDocMigration scans candidateFiles(dir), decodes each as a YAML multi-document
// stream, applies transform to every document, and — when any document changed —
// re-encodes the whole stream (4-space indent) and writes it back (0o644) unless
// dryRun. A rollback copy of each rewritten file is written to the ONE gitignored
// <root>/.charly/backups/ tree (never adjacent) and its path is printed to out.
// Returns the rewritten paths; unreadable files are skipped.
func runDocMigration(root, dir string, dryRun bool, candidateFiles func(string) []string, transform func(*yaml.Node) bool, out io.Writer) ([]string, error) {
	var rewritten []string
	for _, path := range candidateFiles(dir) {
		data, err := os.ReadFile(path)
		if err != nil {
			continue // skip unreadable siblings; don't abort
		}
		dec := yaml.NewDecoder(bytes.NewReader(data))
		var docs []*yaml.Node
		changed := false
		for {
			var doc yaml.Node
			if derr := dec.Decode(&doc); derr != nil {
				break
			}
			d := doc
			if transform(&d) {
				changed = true
			}
			docs = append(docs, &d)
		}
		if !changed {
			continue
		}
		var buf bytes.Buffer
		enc := yaml.NewEncoder(&buf)
		enc.SetIndent(4)
		for _, d := range docs {
			if eerr := enc.Encode(d); eerr != nil {
				return rewritten, fmt.Errorf("encoding %s: %w", path, eerr)
			}
		}
		_ = enc.Close()
		if !dryRun {
			if backup, berr := writeMigrationBackup(root, path, data); berr != nil {
				return rewritten, berr
			} else if backup != "" && out != nil {
				_, _ = fmt.Fprintf(out, "backup: %s\n", backup)
			}
			if werr := os.WriteFile(path, buf.Bytes(), 0o644); werr != nil {
				return rewritten, fmt.Errorf("writing %s: %w", path, werr)
			}
		}
		rewritten = append(rewritten, path)
	}
	return rewritten, nil
}

// migrateHostOverlayDoc applies a document transform to the per-host deploy overlay
// (ctx.HostDeployPath) in addition to the project files. Gated on a non-empty
// HostDeployPath, so the project-only runner (remote-cache auto-migration) never
// touches the user's per-host state. Returns whether the overlay changed.
func migrateHostOverlayDoc(ctx *MigrateContext, transform func(*yaml.Node) bool) (bool, error) {
	if ctx.HostDeployPath == "" {
		return false, nil
	}
	return rewriteDocFile(ctx.Dir, ctx.HostDeployPath, ctx.DryRun, transform, ctx.Out)
}

// rewriteDocFile reads path, decodes ONE YAML document, applies transform, and —
// when it changed — re-encodes (4-space indent) and writes it back (0644) unless
// dryRun, after saving a rollback copy in the gitignored <root>/.charly/backups/
// tree (its path printed to out). A missing/unparseable file is a no-op.
func rewriteDocFile(root, path string, dryRun bool, transform func(*yaml.Node) bool, out io.Writer) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return false, nil
	}
	if !transform(&doc) {
		return false, nil
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(4)
	if err := enc.Encode(&doc); err != nil {
		return false, err
	}
	_ = enc.Close()
	if dryRun {
		return true, nil
	}
	if backup, berr := writeMigrationBackup(root, path, data); berr != nil {
		return false, berr
	} else if backup != "" && out != nil {
		_, _ = fmt.Fprintf(out, "backup: %s\n", backup)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		return false, err
	}
	return true, nil
}
