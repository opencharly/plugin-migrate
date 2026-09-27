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
		files, ferr := runDocMigration(ctx.Dir, ctx.DryRun, kit.OpUnifyCandidateFiles, transform)
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
		did, err := stripVersionField(filepath.Join(ctx.Dir, name), ctx.DryRun)
		if err != nil {
			return changed, err
		}
		if did {
			changed = append(changed, name)
		}
	}
	if !projectOnly && ctx.HostDeployPath != "" {
		did, err := stripVersionField(ctx.HostDeployPath, ctx.DryRun)
		if err != nil {
			return changed, err
		}
		if did {
			changed = append(changed, ctx.HostDeployPath)
		}
	}
	files, err := runDocMigration(ctx.Dir, ctx.DryRun, kit.OpUnifyCandidateFiles, stripEntityVersionKey)
	if err != nil {
		return changed, err
	}
	changed = append(changed, files...)
	if !projectOnly && ctx.HostDeployPath != "" {
		hostChanged, herr := rewriteDocFile(ctx.HostDeployPath, ctx.DryRun, stripEntityVersionKey)
		if herr != nil {
			return changed, herr
		}
		if hostChanged {
			changed = append(changed, ctx.HostDeployPath)
		}
	}
	return changed, nil
}

// stripVersionField DELETES the first top-level `version:` line of one file. Returns
// (changed, err); changed is false when the file is absent or has no top-level
// `version:` key. A <path>.bak.<unix-ts> rollback is written before any rewrite.
func stripVersionField(path string, dryRun bool) (bool, error) {
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
	backup := fmt.Sprintf("%s.bak.%d", path, time.Now().Unix())
	if err := os.WriteFile(backup, data, 0o644); err != nil {
		return false, fmt.Errorf("writing backup %s: %w", backup, err)
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
// dryRun. Returns the rewritten paths; unreadable files are skipped.
func runDocMigration(dir string, dryRun bool, candidateFiles func(string) []string, transform func(*yaml.Node) bool) ([]string, error) {
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
	return rewriteDocFile(ctx.HostDeployPath, ctx.DryRun, transform)
}

// rewriteDocFile reads path, decodes ONE YAML document, applies transform, and —
// when it changed — re-encodes (4-space indent) and writes it back (0644) unless
// dryRun, after saving a .bak.<unix-ts> copy. A missing/unparseable file is a no-op.
func rewriteDocFile(path string, dryRun bool, transform func(*yaml.Node) bool) (bool, error) {
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
	bak := fmt.Sprintf("%s.bak.%d", path, time.Now().Unix())
	_ = os.WriteFile(bak, data, 0644)
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		return false, err
	}
	return true, nil
}
