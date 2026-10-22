package migrate

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// reshape_engine_word_test.go — the retired ENGINE WORD `nerdctl` (spec#266 deleted it
// from #EngineName; #EngineBinaries now maps containerd → nerdctl). Two authored
// surfaces, two shapes:
//
//   - PROJECT: `engine:` is a SCALAR #EngineName on #Deploy/#Candy/#Kindcluster, so one
//     declarative remap_scalar (engine: nerdctl → containerd) covers every occurrence.
//   - HOST: the per-host runtime config (~/.config/charly/config.yml) authors `engine:`
//     as a MAPPING (`engine: {run: nerdctl}`), and the op vocabulary has no parent-path
//     predicate — remap_scalar keyed on `run` would rewrite ANY unrelated
//     `run: nerdctl` scalar — so this surface needs the apply: hook
//     `remapEngineRunWord` with touches_host and host_config: "runtime_config".
//
// Both tests run the FULL engine (every table step, in order), so they also prove the
// step is idempotent in the real chain.

// TestRunMigrations_EngineWordProjectSurface: a project document carrying the retired
// engine word on a candy body and a deploy node migrates to `containerd`, and a second
// run is a no-op.
func TestRunMigrations_EngineWordProjectSurface(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "charly.yml")
	body := "" +
		"mycandy:\n" +
		"    candy:\n" +
		"        description: d\n" +
		"        engine: nerdctl\n" +
		"app:\n" +
		"    local:\n" +
		"        candy:\n" +
		"            - mycandy\n" +
		"        engine: nerdctl\n"
	if err := os.WriteFile(root, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	changed, err := runMigrations(&MigrateContext{Dir: dir, Out: &out}, true)
	if err != nil {
		t.Fatalf("runMigrations: %v", err)
	}
	if !changed {
		t.Fatalf("the retired engine word must be migrated, output: %q", out.String())
	}
	if !strings.Contains(out.String(), "applied engine-word-containerd") {
		t.Errorf("want the project engine step reported, output: %q", out.String())
	}
	afterFirst, err := os.ReadFile(root)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(afterFirst), "nerdctl") {
		t.Errorf("the retired engine word survived on the project surface:\n%s", afterFirst)
	}
	if got := strings.Count(string(afterFirst), "engine: containerd"); got != 2 {
		t.Errorf("want both engine: scalars rewritten to containerd, got %d:\n%s", got, afterFirst)
	}

	// Idempotence: the second full run changes nothing.
	out.Reset()
	changed, err = runMigrations(&MigrateContext{Dir: dir, Out: &out}, true)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if changed {
		t.Errorf("second run must be a no-op, output: %q", out.String())
	}
	if !strings.Contains(out.String(), "nothing to migrate") {
		t.Errorf("second run: want 'nothing to migrate', got %q", out.String())
	}
	afterSecond, err := os.ReadFile(root)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterFirst, afterSecond) {
		t.Error("second run modified the already-migrated project")
	}
}

// TestRunMigrations_EngineRunWordHostRuntimeConfig: the per-host runtime config
// (~/.config/charly/config.yml, hostenv.RuntimeConfig) authors `engine: {run: nerdctl}`;
// full mode rewrites it and preserves its siblings, a second run is a no-op, and
// project-only mode (the remote-cache path) leaves it untouched. The step selects this
// surface with `host_config: "runtime_config"` — the deploy overlay (spec.DeployConfig)
// has no engine field at all.
func TestRunMigrations_EngineRunWordHostRuntimeConfig(t *testing.T) {
	dir := writeRoot(t)
	overlay := filepath.Join(t.TempDir(), "charly.yml")
	runtimeCfg := filepath.Join(t.TempDir(), "config.yml")
	body := "" +
		"engine:\n" +
		"    run: nerdctl\n" +
		"    rootful: auto\n" +
		"bind_address: 127.0.0.1\n"
	if err := os.WriteFile(runtimeCfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	// Project-only must NOT touch host state: the step is touches_host-gated.
	var out bytes.Buffer
	ctx := &MigrateContext{Dir: dir, HostDeployPath: overlay, HostRuntimePath: runtimeCfg, Out: &out}
	changed, err := runMigrations(ctx, true)
	if err != nil {
		t.Fatalf("project-only run: %v", err)
	}
	if changed {
		t.Errorf("project-only run must not change the runtime config, output: %q", out.String())
	}
	untouched, err := os.ReadFile(runtimeCfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(untouched), "run: nerdctl") {
		t.Errorf("project-only run touched host state:\n%s", untouched)
	}

	// Full mode rewrites the nested host engine word.
	out.Reset()
	changed, err = runMigrations(ctx, false)
	if err != nil {
		t.Fatalf("full run: %v", err)
	}
	if !changed {
		t.Fatalf("the host engine word must be migrated, output: %q", out.String())
	}
	if !strings.Contains(out.String(), "applied engine-run-word-containerd") {
		t.Errorf("want the host engine step reported, output: %q", out.String())
	}
	afterFirst, err := os.ReadFile(runtimeCfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(afterFirst), "run: nerdctl") {
		t.Errorf("the retired host engine word survived:\n%s", afterFirst)
	}
	if !strings.Contains(string(afterFirst), "run: containerd") {
		t.Errorf("host engine.run was not rewritten:\n%s", afterFirst)
	}
	if !strings.Contains(string(afterFirst), "rootful: auto") || !strings.Contains(string(afterFirst), "bind_address: 127.0.0.1") {
		t.Errorf("runtime config siblings must survive:\n%s", afterFirst)
	}

	// Idempotence on the host surface.
	out.Reset()
	changed, err = runMigrations(ctx, false)
	if err != nil {
		t.Fatalf("second full run: %v", err)
	}
	if changed {
		t.Errorf("second run must be a no-op, output: %q", out.String())
	}
	afterSecond, err := os.ReadFile(runtimeCfg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterFirst, afterSecond) {
		t.Error("second run modified the already-migrated runtime config")
	}
}

// TestRunMigrations_HostConfigSelectsOneSurface proves the two per-host surfaces are
// NEVER swept by the same step: the engine step rewrites only the runtime config, and
// the other touches_host transforms keep running only on the deploy overlay. The
// `vm: {cpus: …}` arm is the concrete regression — reshapeDeployCPU renames a direct
// `cpus:` child of a `vm:` mapping, which in the RuntimeConfig is the LIVE
// RuntimeVmConfig.cpus field; running every host step against config.yml would corrupt it.
func TestRunMigrations_HostConfigSelectsOneSurface(t *testing.T) {
	dir := writeRoot(t)
	overlay := filepath.Join(t.TempDir(), "charly.yml")
	runtimeCfg := filepath.Join(t.TempDir(), "config.yml")
	overlayBody := "leftover:\n    group: {}\n"
	if err := os.WriteFile(overlay, []byte(overlayBody), 0o644); err != nil {
		t.Fatal(err)
	}
	runtimeBody := "" +
		"engine:\n" +
		"    run: nerdctl\n" +
		"vm:\n" +
		"    cpus: 4\n" +
		"    ram: 8G\n"
	if err := os.WriteFile(runtimeCfg, []byte(runtimeBody), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	changed, err := runMigrations(&MigrateContext{Dir: dir, HostDeployPath: overlay, HostRuntimePath: runtimeCfg, Out: &out}, false)
	if err != nil {
		t.Fatalf("runMigrations: %v", err)
	}
	if !changed {
		t.Fatalf("expected changes, output: %q", out.String())
	}

	// The overlay was swept by its OWN host steps (the barren group is retired) …
	overlayAfter, err := os.ReadFile(overlay)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(overlayAfter), "group:") {
		t.Errorf("the overlay's own host step did not run:\n%s", overlayAfter)
	}
	// … and the engine step did NOT touch it.
	if strings.Contains(string(overlayAfter), "engine") {
		t.Errorf("the runtime-config step rewrote the deploy overlay:\n%s", overlayAfter)
	}

	// The runtime config got ONLY the engine step: vm.cpus survives.
	runtimeAfter, err := os.ReadFile(runtimeCfg)
	if err != nil {
		t.Fatal(err)
	}
	got := string(runtimeAfter)
	if !strings.Contains(got, "run: containerd") {
		t.Errorf("runtime config engine word not rewritten:\n%s", got)
	}
	if !strings.Contains(got, "cpus: 4") {
		t.Errorf("RuntimeVmConfig.cpus was corrupted by an overlay step:\n%s", got)
	}
	if !strings.Contains(got, "ram: 8G") {
		t.Errorf("runtime config vm block damaged:\n%s", got)
	}
}

// TestMigrationTable_EngineWordContainerd pins the two table entries and their ORDER:
// the declarative PROJECT step first (a scalar remap, NOT touches_host — it must run in
// remote-cache auto-migration too), then the HOST mapping step (a touches_host apply:
// hook). The hook must be registered; the project step must carry no hook.
func TestMigrationTable_EngineWordContainerd(t *testing.T) {
	project := findMigration(t, "engine-word-containerd")
	if project.TouchesHost {
		t.Error("engine-word-containerd rewrites a PROJECT key; touches_host would make remote-cache auto-migration skip it")
	}
	if project.Apply != "" {
		t.Errorf("the project scalar remap is declarative — no apply hook expected: %+v", project)
	}
	if len(project.Ops) != 1 {
		t.Fatalf("want exactly one remap_scalar op, got %+v", project.Ops)
	}
	op := project.Ops[0]
	if op.Op != "remap_scalar" || op.Key != "engine" || op.From != "nerdctl" || op.To != "containerd" || op.UnderKind != "" {
		t.Errorf("unexpected project op: %+v", op)
	}
	if op.Scope != "" {
		t.Errorf("remap_scalar has no scope arm; an empty scope already means `any`, got %q", op.Scope)
	}

	host := findMigration(t, "engine-run-word-containerd")
	if !host.TouchesHost {
		t.Error("engine-run-word-containerd rewrites per-host state; touches_host must be set")
	}
	if host.HostConfig != "runtime_config" {
		t.Errorf("the host engine word lives in the runtime config, got host_config=%q", host.HostConfig)
	}
	if host.Apply != "remapEngineRunWord" || len(host.Ops) != 0 {
		t.Errorf("unexpected host entry: %+v", host)
	}
	if _, ok := goHooks[host.Apply]; !ok {
		t.Errorf("hook %q is not registered in goHooks", host.Apply)
	}

	projectIdx, hostIdx := -1, -1
	for i, m := range migrationTable {
		switch m.Name {
		case project.Name:
			projectIdx = i
		case host.Name:
			hostIdx = i
		}
	}
	if projectIdx < 0 || hostIdx < 0 || projectIdx > hostIdx {
		t.Errorf("want the project step before the host step, got indexes %d/%d", projectIdx, hostIdx)
	}

	// The shipped project entry really rewrites a scalar engine word.
	out, changed := applyTransform(t, project, "app:\n    local:\n        engine: nerdctl\n")
	if !changed || !strings.Contains(out, "engine: containerd") || strings.Contains(out, "engine: nerdctl") {
		t.Errorf("the shipped project entry did not rewrite the scalar engine word: %q", out)
	}
}

// TestRemapEngineRunWord_MappingOnly pins the hook's EXACT scope: a direct `run:` child
// of an `engine:` mapping is rewritten, while an identical `run: nerdctl` scalar NOT
// under `engine:` survives (the false positive a document-wide remap_scalar on `run`
// would introduce — the reason this is a hook).
func TestRemapEngineRunWord_MappingOnly(t *testing.T) {
	in := "" +
		"engine:\n" +
		"    run: nerdctl\n" +
		"    rootful: auto\n" +
		"pipeline:\n" +
		"    steps:\n" +
		"        - run: nerdctl\n"
	out, changed := applyTransformToYAML(t, remapEngineRunWord, in)
	if !changed {
		t.Fatal("remapEngineRunWord reported no change")
	}
	var got map[string]any
	if err := yaml.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("reparse: %v", err)
	}
	engine, ok := got["engine"].(map[string]any)
	if !ok || engine["run"] != "containerd" || engine["rootful"] != "auto" {
		t.Errorf("host engine mapping not rewritten in place: %v", got["engine"])
	}
	steps, ok := got["pipeline"].(map[string]any)["steps"].([]any)
	if !ok || len(steps) != 1 {
		t.Fatalf("pipeline.steps malformed: %v", got["pipeline"])
	}
	if step, ok := steps[0].(map[string]any); !ok || step["run"] != "nerdctl" {
		t.Errorf("an unrelated run: nerdctl was rewritten: %v", steps[0])
	}

	// Idempotent, and the hook OWNS ONLY the mapping form: the project scalar is the
	// declarative step's job, never the hook's.
	if _, again := applyTransformToYAML(t, remapEngineRunWord, out); again {
		t.Error("second pass changed an already-migrated document")
	}
	scalarOut, scalarChanged := applyTransformToYAML(t, remapEngineRunWord, "engine: nerdctl\n")
	if scalarChanged || !strings.Contains(scalarOut, "engine: nerdctl") {
		t.Errorf("the hook must not touch the scalar project form (remap_scalar owns it): %q", scalarOut)
	}
}
