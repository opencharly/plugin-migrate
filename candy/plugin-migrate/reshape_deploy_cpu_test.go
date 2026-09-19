package migrate

import (
	"strings"
	"testing"
)

// reshape_deploy_cpu_test.go — the deploy per-deploy VM-shape override `cpus:` → `cpu:`
// rename: the direct-child conversion, the SECURITY-COLLISION exclusion (the reason a
// declarative rename_key op could not be used), idempotency, and the both-keys case.

func deployCPUMigrate(t *testing.T, in string) string {
	t.Helper()
	out, changed := applyTransform(t, migration{Name: "t", Apply: "reshapeDeployCPU"}, in)
	if !changed {
		t.Fatal("reshapeDeployCPU reported no change")
	}
	return out
}

// TestDeployCPURenameBasic — a vm deploy's own cpus is renamed; ram and everything else
// pass through untouched.
func TestDeployCPURenameBasic(t *testing.T) {
	in := `version: 2026.249.2125
rdd-shape:
    vm:
        from: omarchy-vm
        ram: 4G
        cpus: 2
        disposable: true
`
	out := deployCPUMigrate(t, in)
	if !strings.Contains(out, "cpu: 2") {
		t.Errorf("cpu override not renamed:\n%s", out)
	}
	if strings.Contains(out, "cpus:") {
		t.Errorf("stale cpus: remains:\n%s", out)
	}
	if !strings.Contains(out, "ram: 4G") || !strings.Contains(out, "disposable: true") {
		t.Errorf("unrelated fields must pass through:\n%s", out)
	}
}

// TestDeployCPUDoesNotTouchSecurityCpus — THE regression this hook exists for: a vm
// deploy carrying a security: block with its OWN `cpus:` (a string quota on #Security,
// a different field/type) must keep it. The declarative rename_key op under
// under_kind: vm WOULD corrupt this. Deleting the direct-child guard (recursing instead)
// fails this test.
func TestDeployCPUDoesNotTouchSecurityCpus(t *testing.T) {
	in := `version: 2026.249.2125
guarded:
    vm:
        from: some-vm
        cpus: 4
        security:
            cpus: "2.5"
            memory_high: 1G
`
	out := deployCPUMigrate(t, in)
	if !strings.Contains(out, "cpu: 4") {
		t.Errorf("the deploy's own cpus must be renamed:\n%s", out)
	}
	if !strings.Contains(out, `cpus: "2.5"`) {
		t.Errorf("security.cpus MUST survive — it is a different field:\n%s", out)
	}
}

// TestDeployCPUDoesNotTouchSecurityCpusNested is the deeper collision guard: a
// security: block's own cpus survives even when the SAME body also carries a
// sibling variants: map (the direct-child rule must never descend into either).
func TestDeployCPUDoesNotTouchSecurityCpusNested(t *testing.T) {
	in := `version: 2026.249.2125
bed:
    vm:
        from: some-vm
        variants:
            small:
                cpu: 1
        security:
            cpus: "2.5"
`
	out, _ := applyTransform(t, migration{Name: "t", Apply: "reshapeDeployCPU"}, in)
	if !strings.Contains(out, `cpus: "2.5"`) {
		t.Errorf("security.cpus must survive:\n%s", out)
	}
	// The direct-child rule must not descend into variants: either — its inner
	// `cpu:` is left as authored (the variants surface is deleted).
	if !strings.Contains(out, "cpu: 1") {
		t.Errorf("the variants map must be left untouched:\n%s", out)
	}
}

// TestDeployCPURenameAllSubstrates — EVERY substrate kind body renames, not just vm:
// pod, vm, local, kubernetes, android.
func TestDeployCPURenameAllSubstrates(t *testing.T) {
	in := `version: 2026.249.2125
p:
    pod:
        from: base-pod
        cpus: 2
v:
    vm:
        from: base-vm
        cpus: 3
l:
    local:
        from: base-local
        cpus: 4
k:
    kubernetes:
        from: base-kube
        cpus: 5
a:
    android:
        from: base-android
        cpus: 6
`
	out := deployCPUMigrate(t, in)
	if strings.Contains(out, "cpus:") {
		t.Errorf("every substrate kind body must rename:\n%s", out)
	}
	for _, want := range []string{"cpu: 2", "cpu: 3", "cpu: 4", "cpu: 5", "cpu: 6"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q after rename:\n%s", want, out)
		}
	}
}

// TestDeployCPURenameIdempotent — a second pass sees no cpus: and reports no change (so
// `charly migrate` twice is a no-op).
func TestDeployCPURenameIdempotent(t *testing.T) {
	in := `version: 2026.249.2125
rdd:
    vm:
        from: omarchy-vm
        cpus: 2
`
	out := deployCPUMigrate(t, in)
	if _, changed := applyTransform(t, migration{Name: "t", Apply: "reshapeDeployCPU"}, out); changed {
		t.Errorf("second pass must be a no-op:\n%s", out)
	}
}

// TestDeployCPUBothKeysDropsStale — a hand-edited file carrying BOTH keys leaves the new
// `cpu` and drops the stale `cpus` (never a duplicate key).
func TestDeployCPUBothKeysDropsStale(t *testing.T) {
	in := `version: 2026.249.2125
rdd:
    vm:
        from: omarchy-vm
        cpu: 8
        cpus: 2
`
	out := deployCPUMigrate(t, in)
	if strings.Count(out, "cpu:") != 1 {
		t.Errorf("exactly one cpu key expected:\n%s", out)
	}
	if !strings.Contains(out, "cpu: 8") {
		t.Errorf("the pre-existing cpu value must win:\n%s", out)
	}
	if strings.Contains(out, "cpus:") {
		t.Errorf("stale cpus must be dropped:\n%s", out)
	}
}
