package migrate

import (
	"strings"
	"testing"
)

// reshape_record_field_test.go — the deploy record: field harvest: conversion shape,
// value pass-through, the id default, nested members, and the step-sugar exclusion.

func recordHarvest(t *testing.T, in string) string {
	t.Helper()
	out, changed := applyTransform(t, migration{Name: "t", Apply: "recordFieldToInstrument"}, in)
	if !changed {
		t.Fatal("recordFieldToInstrument reported no change")
	}
	return out
}

func TestRecordFieldHarvestBasic(t *testing.T) {
	in := `version: 2026.240.1943
check-rec-bed:
    vm:
        from: rec-vm
        disposable: true
        record:
            record_name: pr-flow
            fps: 4
            artifact: /tmp/pr.cast
        plan:
            - check: subject
              command: "true"
`
	out := recordHarvest(t, in)
	for _, want := range []string{
		"instrument:",
		"id: pr-flow",
		"phase:",
		"  - live",
		"record:",
		"method: session",
		"fps: 4",
		"artifact: /tmp/pr.cast",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("harvested output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "record_name") {
		t.Errorf("record_name must be consumed as the id:\n%s", out)
	}
}

func TestRecordFieldHarvestDefaultID(t *testing.T) {
	in := `version: 2026.240.1943
check-rec-bed:
    pod:
        from: rec-pod
        disposable: true
        record:
            terminal: true
`
	out := recordHarvest(t, in)
	if !strings.Contains(out, "id: recording") {
		t.Errorf("default id missing:\n%s", out)
	}
	if !strings.Contains(out, "terminal: true") {
		t.Errorf("value field must pass through:\n%s", out)
	}
}

// TestRecordFieldSkipsStepSugar: a plan-step record: (carrying method:) is the record verb
// sugar, never the deploy field — the harvest must leave it untouched.
func TestRecordFieldSkipsStepSugar(t *testing.T) {
	in := `version: 2026.240.1943
check-rec-bed:
    vm:
        from: rec-vm
        disposable: true
        plan:
            - check: record the session
              record:
                method: start
                record_name: walk
`
	out, changed := applyTransform(t, migration{Name: "t", Apply: "recordFieldToInstrument"}, in)
	if changed {
		t.Fatalf("step-sugar-only config must report no change:\n%s", out)
	}
	if !strings.Contains(out, "method: start") {
		t.Errorf("step sugar record: must survive untouched:\n%s", out)
	}
	if strings.Contains(out, "instrument:") {
		t.Errorf("step sugar must NOT harvest into instrument::\n%s", out)
	}
}

// TestRecordFieldHarvestNestedMember: the walk reaches nested/peer member nodes.
func TestRecordFieldHarvestNestedMember(t *testing.T) {
	in := `version: 2026.240.1943
check-rec-bed:
    vm:
        from: rec-vm
        disposable: true
    driver:
        local:
            from: driver-tpl
            record:
                record_name: driver-view
`
	out := recordHarvest(t, in)
	if !strings.Contains(out, "id: driver-view") {
		t.Errorf("nested member harvest missing:\n%s", out)
	}
}

// TestRecordFieldHarvestAppendsExistingInstrument: a config that already carries an
// instrument: list appends the harvested entry instead of clobbering it.
func TestRecordFieldHarvestAppendsExistingInstrument(t *testing.T) {
	in := `version: 2026.248.1030
check-rec-bed:
    vm:
        from: rec-vm
        disposable: true
        instrument:
            - id: screen
              spice:
                method: session
        record:
            record_name: legacy
`
	out := recordHarvest(t, in)
	idxScreen := strings.Index(out, "id: screen")
	idxLegacy := strings.Index(out, "id: legacy")
	if idxScreen < 0 || idxLegacy < 0 || idxScreen > idxLegacy {
		t.Errorf("harvested entry must append after the existing instrument:\n%s", out)
	}
}
