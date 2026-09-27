package migrate

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestMigrationTable_CompactNodeForm: the table carries the schema-compaction
// migration — an apply: goHook entry that touches host state — as its FIRST
// (oldest) step, ahead of any later step.
func TestMigrationTable_CompactNodeForm(t *testing.T) {
	if len(migrationTable) == 0 {
		t.Fatal("migration table is empty, expected at least the compact-node-form entry")
	}
	m := migrationTable[0]
	if m.Name != "compact-node-form" || m.Apply != "compactNodeForm" || !m.TouchesHost {
		t.Errorf("unexpected first table entry: %+v", m)
	}
	if _, ok := goHooks[m.Apply]; !ok {
		t.Errorf("hook %q not registered in goHooks", m.Apply)
	}
}

// TestMigrationTable_StripCandyLibvirtField: the table carries the candy-level
// `libvirt:` field removal as a project-only (non-touches_host) apply: goHook
// entry, strictly after compact-node-form.
func TestMigrationTable_StripCandyLibvirtField(t *testing.T) {
	if len(migrationTable) != 11 {
		t.Fatalf("migration table should carry exactly 11 entries, got %d", len(migrationTable))
	}
	m := migrationTable[1]
	if m.Name != "strip-candy-libvirt-field" || m.Apply != "stripCandyLibvirtField" || m.TouchesHost {
		t.Errorf("unexpected second table entry: %+v", m)
	}
	if _, ok := goHooks[m.Apply]; !ok {
		t.Errorf("hook %q not registered in goHooks", m.Apply)
	}
}

// TestMigrationTable_StripDeployShellOverlay: the table carries the deploy-scope
// `shell:` overlay field removal as a touches_host apply: goHook entry (the field
// was authorable on the per-host charly.yml too), strictly after
// strip-candy-libvirt-field.
func TestMigrationTable_StripDeployShellOverlay(t *testing.T) {
	m := migrationTable[2]
	if m.Name != "strip-deploy-shell-overlay" || m.Apply != "stripDeployShellOverlay" || !m.TouchesHost {
		t.Errorf("unexpected third table entry: %+v", m)
	}
	if _, ok := goHooks[m.Apply]; !ok {
		t.Errorf("hook %q not registered in goHooks", m.Apply)
	}
}

// TestMigrationTable_K8sToKubernetes: the table carries the deploy-substrate
// kind rename as the 4th entry: op 1 renames every `k8s:` key to `kubernetes:`
// (scope any), then op 2 renames the inner deploy-knobs `kubernetes:` block to
// `deploy:` scoped under_kind "kubernetes" — in that exact order, strictly after
// strip-deploy-shell-overlay.
func TestMigrationTable_K8sToKubernetes(t *testing.T) {
	m := migrationTable[3]
	if m.Name != "k8s-to-kubernetes" || len(m.Ops) != 2 {
		t.Fatalf("unexpected fourth table entry: %+v", m)
	}
	op1, op2 := m.Ops[0], m.Ops[1]
	if op1.Op != "rename_key" || op1.From != "k8s" || op1.To != "kubernetes" || op1.Scope != "any" {
		t.Errorf("op1 should rename k8s→kubernetes scope any: %+v", op1)
	}
	if op2.Op != "rename_key" || op2.From != "kubernetes" || op2.To != "deploy" || op2.UnderKind != "kubernetes" {
		t.Errorf("op2 should rename kubernetes→deploy under_kind kubernetes: %+v", op2)
	}
}

// TestStripCandyLibvirtField_RemovesCandyLevelOnly: the reshaper removes ONLY the
// direct candy-body `libvirt:` field, leaving a same-named `vm:`-kind entity's own
// domain-config `libvirt: {...}` object AND a `libvirt:` check-verb step nested in
// a candy's `plan:` completely untouched (the exact ambiguity a blanket
// under_kind-scoped delete_key op would have gotten wrong — see the hook's header).
func TestStripCandyLibvirtField_RemovesCandyLevelOnly(t *testing.T) {
	m := migration{Name: "t", Apply: "stripCandyLibvirtField"}
	in := "" +
		"qemu-guest-agent:\n" +
		"  candy:\n" +
		"    version: 2026.149.1200\n" +
		"    package: [qemu-guest-agent]\n" +
		"    libvirt: [\"<channel type='unix'/>\"]\n" +
		"    plan:\n" +
		"      - check: the libvirt domain is queryable\n" +
		"        libvirt: info\n" +
		"vm-libvirt:\n" +
		"  vm:\n" +
		"    libvirt:\n" +
		"      devices:\n" +
		"        channels: [{type: unix}]\n"
	out, changed := applyTransform(t, m, in)
	if !changed {
		t.Fatal("expected the candy-level libvirt: field to be removed")
	}
	var doc struct {
		QemuGuestAgent struct {
			Candy struct {
				Package []string         `yaml:"package"`
				Libvirt []string         `yaml:"libvirt"`
				Plan    []map[string]any `yaml:"plan"`
			} `yaml:"candy"`
		} `yaml:"qemu-guest-agent"`
		VmLibvirt struct {
			Vm struct {
				Libvirt map[string]any `yaml:"libvirt"`
			} `yaml:"vm"`
		} `yaml:"vm-libvirt"`
	}
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("reparse: %v\n%s", err, out)
	}
	if doc.QemuGuestAgent.Candy.Libvirt != nil {
		t.Errorf("candy-level libvirt: field survived: %v", doc.QemuGuestAgent.Candy.Libvirt)
	}
	if len(doc.QemuGuestAgent.Candy.Package) != 1 || doc.QemuGuestAgent.Candy.Package[0] != "qemu-guest-agent" {
		t.Errorf("unrelated candy field damaged: %v", doc.QemuGuestAgent.Candy.Package)
	}
	if len(doc.QemuGuestAgent.Candy.Plan) != 1 || doc.QemuGuestAgent.Candy.Plan[0]["libvirt"] != "info" {
		t.Errorf("the candy's own plan-step libvirt: check-verb sugar was damaged: %v", doc.QemuGuestAgent.Candy.Plan)
	}
	if doc.VmLibvirt.Vm.Libvirt == nil {
		t.Error("the vm entity's own libvirt: domain config was incorrectly removed")
	}
	if _, changed2 := applyTransform(t, m, out); changed2 {
		t.Error("second pass changed an already-migrated doc")
	}
}

// TestStripDeployShellOverlay_RemovesSequenceValuedOnly: the reshaper removes ONLY
// a direct, SEQUENCE-valued `shell:` key (the retired deploy-scope overlay),
// leaving a candy's own MAPPING-valued `shell:` (#Shell intrinsic init) on a
// DIFFERENT entity completely untouched — the exact ambiguity a blanket
// scope-scoped delete_key op would have gotten wrong (see the hook's header).
func TestStripDeployShellOverlay_RemovesSequenceValuedOnly(t *testing.T) {
	m := migration{Name: "t", Apply: "stripDeployShellOverlay"}
	in := "" +
		"mydeploy:\n" +
		"  pod:\n" +
		"    image: x\n" +
		"    shell:\n" +
		"    - id: direnv\n" +
		"      bash:\n" +
		"        init: direnv hook bash\n" +
		"mycandy:\n" +
		"  candy:\n" +
		"    version: 2026.149.1200\n" +
		"    description: d\n" +
		"    shell:\n" +
		"      init: export FOO=bar\n" +
		"    plan:\n" +
		"    - check: c\n" +
		"      file: /x\n"
	out, changed := applyTransform(t, m, in)
	if !changed {
		t.Fatal("expected the deploy-scope sequence-valued shell: field to be removed")
	}
	var doc struct {
		MyDeploy struct {
			Pod struct {
				Image string           `yaml:"image"`
				Shell []map[string]any `yaml:"shell"`
			} `yaml:"pod"`
		} `yaml:"mydeploy"`
		MyCandy struct {
			Candy struct {
				Shell map[string]any   `yaml:"shell"`
				Plan  []map[string]any `yaml:"plan"`
			} `yaml:"candy"`
		} `yaml:"mycandy"`
	}
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("reparse: %v\n%s", err, out)
	}
	if doc.MyDeploy.Pod.Shell != nil {
		t.Errorf("deploy-scope sequence-valued shell: survived: %v", doc.MyDeploy.Pod.Shell)
	}
	if doc.MyDeploy.Pod.Image != "x" {
		t.Errorf("unrelated pod field damaged: %v", doc.MyDeploy.Pod.Image)
	}
	if doc.MyCandy.Candy.Shell == nil {
		t.Error("the candy's own mapping-valued shell: field was incorrectly removed")
	}
	if len(doc.MyCandy.Candy.Plan) != 1 || doc.MyCandy.Candy.Plan[0]["file"] != "/x" {
		t.Errorf("the candy's own plan was damaged: %v", doc.MyCandy.Candy.Plan)
	}
	if _, changed2 := applyTransform(t, m, out); changed2 {
		t.Error("second pass changed an already-migrated doc")
	}
}

// writeRoot writes a minimal, already-current (version-free) project charly.yml.
func writeRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	body := "discover: []\n"
	if err := os.WriteFile(filepath.Join(dir, "charly.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestRunMigrations_AlreadyCurrentNoOp: a document with no old shapes and no
// version keys is untouched — nothing to migrate.
func TestRunMigrations_AlreadyCurrentNoOp(t *testing.T) {
	dir := writeRoot(t)
	before, _ := os.ReadFile(filepath.Join(dir, "charly.yml"))
	var out bytes.Buffer
	changed, err := runMigrations(&MigrateContext{Dir: dir, Out: &out}, false)
	if err != nil {
		t.Fatalf("runMigrations: %v", err)
	}
	if changed {
		t.Error("already-current config reported changed=true")
	}
	if !strings.Contains(out.String(), "nothing to migrate") {
		t.Errorf("want 'nothing to migrate', got %q", out.String())
	}
	after, _ := os.ReadFile(filepath.Join(dir, "charly.yml"))
	if !bytes.Equal(before, after) {
		t.Error("already-current config was modified")
	}
}

// TestRunMigrations_StripsVersionStamp: a document carrying a top-level `version:`
// and a per-entity (candy) `version:` has BOTH stripped by the universal
// strip-version-stamp step, while unrelated fields survive.
func TestRunMigrations_StripsVersionStamp(t *testing.T) {
	dir := t.TempDir()
	body := "" +
		"version: 2026.248.1030\n" +
		"discover: []\n" +
		"mycandy:\n" +
		"  candy:\n" +
		"    version: 2026.186.0100\n" +
		"    description: d\n"
	root := filepath.Join(dir, "charly.yml")
	if err := os.WriteFile(root, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	changed, err := runMigrations(&MigrateContext{Dir: dir, Out: &out}, false)
	if err != nil {
		t.Fatalf("runMigrations: %v", err)
	}
	if !changed {
		t.Fatalf("expected the version stamps to be stripped; output: %q", out.String())
	}
	after, _ := os.ReadFile(root)
	if strings.Contains(string(after), "version:") {
		t.Errorf("a version: key survived:\n%s", after)
	}
	if !strings.Contains(string(after), "description: d") || !strings.Contains(string(after), "discover: []") {
		t.Errorf("unrelated fields damaged:\n%s", after)
	}
	if !strings.Contains(out.String(), "applied strip-version-stamp") {
		t.Errorf("want 'applied strip-version-stamp', got %q", out.String())
	}
}

// TestRunMigrations_Idempotent: running the engine twice is a no-op — the second
// run changes nothing and reports "nothing to migrate". This is the proof that
// `charly migrate` is idempotent.
func TestRunMigrations_Idempotent(t *testing.T) {
	dir := t.TempDir()
	body := "" +
		"version: 2026.186.2323\n" +
		"mycandy:\n" +
		"  candy:\n" +
		"    version: 2026.186.0100\n" +
		"    description: d\n"
	root := filepath.Join(dir, "charly.yml")
	if err := os.WriteFile(root, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := runMigrations(&MigrateContext{Dir: dir, Out: &out}, false); err != nil {
		t.Fatalf("first run: %v", err)
	}
	afterFirst, _ := os.ReadFile(root)

	out.Reset()
	changed, err := runMigrations(&MigrateContext{Dir: dir, Out: &out}, false)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if changed {
		t.Errorf("second run must be a no-op, output: %q", out.String())
	}
	if !strings.Contains(out.String(), "nothing to migrate") {
		t.Errorf("second run: want 'nothing to migrate', got %q", out.String())
	}
	afterSecond, _ := os.ReadFile(root)
	if !bytes.Equal(afterFirst, afterSecond) {
		t.Error("second run modified the already-migrated config")
	}
}

// TestStripEntityVersionKey_RemovesEntityBodyOnly: the walk removes a direct
// `version:` child of candy/box/deploy entity bodies, leaving a same-named key
// nested deeper inside an entity untouched.
func TestStripEntityVersionKey_RemovesEntityBodyOnly(t *testing.T) {
	in := "" +
		"mycandy:\n" +
		"  candy:\n" +
		"    version: 2026.186.0100\n" +
		"    description: d\n" +
		"mybox:\n" +
		"  box:\n" +
		"    version: 2026.186.0100\n" +
		"mydeploy:\n" +
		"  deploy:\n" +
		"    version: 2026.186.0100\n" +
		"    nested:\n" +
		"      version: keep-me\n"
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(in), &doc); err != nil {
		t.Fatal(err)
	}
	if !stripEntityVersionKey(&doc) {
		t.Fatal("expected entity version keys to be stripped")
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		t.Fatal(err)
	}
	_ = enc.Close()
	got := buf.String()
	if strings.Contains(got, "version: 2026.186.0100") {
		t.Errorf("an entity-body version: survived:\n%s", got)
	}
	if !strings.Contains(got, "version: keep-me") {
		t.Errorf("a nested version: key was incorrectly removed:\n%s", got)
	}
	if stripEntityVersionKey(&doc) {
		t.Error("second pass changed an already-stripped doc")
	}
}

// TestRunMigrations_NoConfig: an empty dir is a friendly no-op (not an error).
func TestRunMigrations_NoConfig(t *testing.T) {
	changed, err := runMigrations(&MigrateContext{Dir: t.TempDir(), Out: &bytes.Buffer{}}, false)
	if err != nil || changed {
		t.Fatalf("empty dir: changed=%v err=%v", changed, err)
	}
}

// applyTransform runs a migration's transform over a YAML doc string.
func applyTransform(t *testing.T, m migration, in string) (out string, changed bool) {
	t.Helper()
	transform, err := buildTransform(m)
	if err != nil {
		t.Fatalf("buildTransform: %v", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(in), &doc); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	changed = transform(&doc)
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		t.Fatalf("encode: %v", err)
	}
	_ = enc.Close()
	return buf.String(), changed
}

// TestOpWalker_RenameKey: rename preserves the value + inline comment, and is a
// no-op on a second pass (the key is already renamed).
func TestOpWalker_RenameKey(t *testing.T) {
	m := migration{Name: "t", Ops: []migrationOp{{Op: "rename_key", From: "widget", To: "gadget", Scope: "any"}}}
	out, changed := applyTransform(t, m, "top:\n  widget: 5 # keep me\n")
	if !changed || !strings.Contains(out, "gadget: 5") || strings.Contains(out, "widget:") {
		t.Errorf("rename failed: %q", out)
	}
	if !strings.Contains(out, "keep me") {
		t.Errorf("inline comment lost: %q", out)
	}
	if _, changed2 := applyTransform(t, m, out); changed2 {
		t.Error("second pass changed an already-migrated doc")
	}
}

// TestOpWalker_DeleteKey: delete removes the pair and carries the deleted key's
// head comment onto the following key.
func TestOpWalker_DeleteKey(t *testing.T) {
	m := migration{Name: "t", Ops: []migrationOp{{Op: "delete_key", Key: "gone", Scope: "root"}}}
	out, changed := applyTransform(t, m, "gone: 1\nkept: 2\n")
	if !changed || strings.Contains(out, "gone:") || !strings.Contains(out, "kept: 2") {
		t.Errorf("delete failed: %q", out)
	}
}

// TestOpWalker_RenameKeyUnderKind: rename_key scoped by under_kind where the
// renamed key IS the kind renames the nested same-named field (the deploy-knobs
// block) but never the kind DISCRIMINATOR of the entity establishing the scope.
func TestOpWalker_RenameKeyUnderKind(t *testing.T) {
	m := migration{Name: "t", Ops: []migrationOp{{Op: "rename_key", From: "kubernetes", To: "deploy", Scope: "any", UnderKind: "kubernetes"}}}
	// A cluster template carries only the discriminator — nothing to rename.
	// A deploy node carries the discriminator PLUS the inner deploy-knobs block.
	in := "production:\n" +
		"  kubernetes:\n" +
		"    box: \"\"\n" +
		"openclaw:\n" +
		"  kubernetes:\n" +
		"    image: openclaw\n" +
		"    kubernetes:\n" +
		"      namespace: apps\n"
	out, changed := applyTransform(t, m, in)
	if !changed {
		t.Fatal("rename reported no change")
	}
	if strings.Count(out, "deploy:") != 1 {
		t.Fatalf("want exactly the inner block renamed to deploy:, got:\n%s", out)
	}
	if strings.Count(out, "kubernetes:") != 2 {
		t.Fatalf("want both kind discriminators preserved, got:\n%s", out)
	}
	// Idempotent: a second pass changes nothing.
	if _, changed2 := applyTransform(t, m, out); changed2 {
		t.Error("second pass changed an already-migrated doc")
	}
}

// TestOpWalker_RemapScalarUnderKind: remap flips only the scalar inside the
// under_kind-scoped subtree, leaving an identical key elsewhere untouched.
func TestOpWalker_RemapScalarUnderKind(t *testing.T) {
	m := migration{Name: "t", Ops: []migrationOp{{Op: "remap_scalar", Key: "target", From: "host", To: "local", UnderKind: "fleet"}}}
	out, changed := applyTransform(t, m, "d1:\n  fleet:\n    target: host\nother:\n  target: host\n")
	if !changed {
		t.Fatal("remap reported no change")
	}
	if strings.Count(out, "target: local") != 1 || strings.Count(out, "target: host") != 1 {
		t.Errorf("under_kind scope wrong — want exactly one flipped: %q", out)
	}
}

// TestOpWalker_MoveKey: move relocates a pair between two child mappings.
func TestOpWalker_MoveKey(t *testing.T) {
	m := migration{Name: "t", Ops: []migrationOp{{Op: "move_key", Key: "k", FromParent: "a", ToParent: "b"}}}
	out, changed := applyTransform(t, m, "root:\n  a:\n    k: v\n  b:\n    x: y\n")
	if !changed {
		t.Fatal("move reported no change")
	}
	var got map[string]map[string]map[string]string
	if err := yaml.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if _, stillInA := got["root"]["a"]["k"]; stillInA {
		t.Error("k still under a")
	}
	if got["root"]["b"]["k"] != "v" {
		t.Errorf("k not moved to b: %v", got["root"]["b"])
	}
}

// TestBuildTransform_UnknownHook: an `apply:` step naming an unregistered Go hook
// is a hard error (the escape-hatch fail-fast).
func TestBuildTransform_UnknownHook(t *testing.T) {
	if _, err := buildTransform(migration{Name: "t", Apply: "nope"}); err == nil {
		t.Fatal("unknown hook accepted; want error")
	}
}

// TestBuildTransform_RegisteredHook: an `apply:` step dispatches to its registered
// goHooks entry.
func TestBuildTransform_RegisteredHook(t *testing.T) {
	const name = "__test_hook_marker"
	called := false
	goHooks[name] = func(*yaml.Node) bool { called = true; return true }
	defer delete(goHooks, name)
	transform, err := buildTransform(migration{Name: "t", Apply: name})
	if err != nil {
		t.Fatalf("buildTransform: %v", err)
	}
	var doc yaml.Node
	_ = yaml.Unmarshal([]byte("a: 1\n"), &doc)
	if !transform(&doc) || !called {
		t.Error("registered hook not dispatched")
	}
}

// TestRunMigrations_StripsOverlayVersionStamp: in full mode the per-host overlay's
// top-level `version:` stamp is stripped too, and the run is idempotent.
func TestRunMigrations_StripsOverlayVersionStamp(t *testing.T) {
	dir := writeRoot(t)
	overlay := filepath.Join(t.TempDir(), "charly.yml")
	body := "version: 2026.186.2323\ngithubrunner:\n    pod:\n        image: githubrunner\n"
	if err := os.WriteFile(overlay, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	changed, err := runMigrations(&MigrateContext{Dir: dir, HostDeployPath: overlay, Out: &out}, false)
	if err != nil {
		t.Fatalf("runMigrations: %v", err)
	}
	if !changed {
		t.Fatalf("overlay version stamp must be stripped; output: %q", out.String())
	}
	after, err := os.ReadFile(overlay)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(after), "version:") {
		t.Errorf("overlay version: survived:\n%s", after)
	}
	if !strings.Contains(string(after), "githubrunner:") {
		t.Errorf("overlay content damaged:\n%s", after)
	}
	// Second run: full no-op.
	out.Reset()
	changed, err = runMigrations(&MigrateContext{Dir: dir, HostDeployPath: overlay, Out: &out}, false)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Errorf("second run must be a no-op, output: %q", out.String())
	}
}

// TestRunMigrations_StripsCandyManifestTopLevelStamp pins the fix for the remote-cache
// migration gap: the top-level `version:` stamp ALSO rides every CANDY manifest
// (candy/<name>/charly.yml) — not just the root charly.yml — and the migration must
// strip it, or an imported candy manifest fails the #NodeDoc gate ("node \"version\":
// #NodeDoc.version: conflicting values …"). Before the fix the root stamp was stripped
// but the candy manifest's top-level stamp survived.
func TestRunMigrations_StripsCandyManifestTopLevelStamp(t *testing.T) {
	dir := t.TempDir()
	candyDir := filepath.Join(dir, "candy", "mycandy")
	if err := os.MkdirAll(candyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "charly.yml"), []byte("discover:\n    - path: candy\n      recursive: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(candyDir, "charly.yml")
	body := "version: 2026.240.1943\nmycandy:\n  candy:\n    description: d\n"
	if err := os.WriteFile(manifest, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	changed, err := runMigrations(&MigrateContext{Dir: dir, Out: &out}, false)
	if err != nil {
		t.Fatalf("runMigrations: %v", err)
	}
	if !changed {
		t.Fatalf("expected the candy manifest's top-level stamp to be stripped; output: %q", out.String())
	}
	after, _ := os.ReadFile(manifest)
	if strings.Contains(string(after), "version:") {
		t.Errorf("a candy top-level version: key survived:\n%s", after)
	}
	if !strings.Contains(string(after), "description: d") {
		t.Errorf("unrelated fields damaged:\n%s", after)
	}
}
