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

// TestDeployCPURenamesVariantShape — #VmVariant was aligned in the same cutover
// (cpus→cpu, memory→ram), so a variants: map value's shape fields ARE renamed.
func TestDeployCPURenamesVariantShape(t *testing.T) {
	in := `version: 2026.249.2125
bed:
    vm:
        from: some-vm
        variants:
            small:
                cpus: 1
                memory: 2G
`
	out := deployCPUMigrate(t, in)
	if !strings.Contains(out, "cpu: 1") || !strings.Contains(out, "ram: 2G") {
		t.Errorf("variants[].cpus/memory must be aligned to cpu/ram:\n%s", out)
	}
	if strings.Contains(out, "cpus:") || strings.Contains(out, "memory:") {
		t.Errorf("stale variant spellings remain:\n%s", out)
	}
}

// TestDeployCPUDoesNotTouchSecurityCpus is the exact collision guard (see the
// schema-side mirror): security.cpus is a different field on a different def.
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
		t.Errorf("security.cpus must survive even alongside variants:\n%s", out)
	}
}

// TestDeployCPURenameAllSubstrates — every substrate kind body is covered, not just vm.
func TestDeployCPURenameAllSubstrates(t *testing.T) {
	in := `version: 2026.249.2125
p:
    pod:
        from: base-pod
        cpus: 2
l:
    local:
        from: base-local
        cpus: 3
`
	out := deployCPUMigrate(t, in)
	if strings.Contains(out, "cpus:") {
		t.Errorf("all substrate kind bodies must rename:\n%s", out)
	}
	if !strings.Contains(out, "cpu: 2") || !strings.Contains(out, "cpu: 3") {
		t.Errorf("both renamed values must be present:\n%s", out)
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
