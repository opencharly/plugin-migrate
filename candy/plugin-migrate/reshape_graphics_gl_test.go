package migrate

import (
	"strings"
	"testing"
)

// The reshaper turns a graphics element's SCALAR gl into the {enable} mapping the new
// #LibvirtGraphicsGL shape requires, across the spellings libvirt's yes/no attributes and
// YAML 1.1 booleans produce between them.
func TestReshapeGraphicsGL_ScalarBecomesMapping(t *testing.T) {
	m := migration{Name: "t", Apply: "reshapeGraphicsGL"}
	in := "" +
		"vm:\n" +
		"  cstream-vm:\n" +
		"    libvirt:\n" +
		"      devices:\n" +
		"        graphics:\n" +
		"          - type: spice\n" +
		"            gl: yes\n" +
		"          - type: egl-headless\n" +
		"            gl: \"no\"\n" +
		"          - type: vnc\n" +
		"            autoport: \"yes\"\n"
	out, changed := applyTransform(t, m, in)
	if !changed {
		t.Fatal("expected the scalar gl fields to be reshaped")
	}
	for _, want := range []string{"enable: true", "enable: false"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// The vnc element has no gl at all and must be left completely alone — in
	// particular its autoport: "yes" is NOT a gl value.
	if !strings.Contains(out, "autoport: \"yes\"") {
		t.Errorf("the vnc element's autoport was disturbed:\n%s", out)
	}
	// A second pass is a no-op: gl is already a mapping.
	out2, changed2 := applyTransform(t, m, out)
	if changed2 {
		t.Errorf("second pass reported a change (not idempotent):\n%s", out2)
	}
}

// `gl` is a short word. The walk is pinned to libvirt → devices → graphics[] → gl, so a
// `gl:` key anywhere else — a candy's own step input, an env var, an unrelated block — must
// survive untouched. Without the path scoping this reshaper would corrupt them.
func TestReshapeGraphicsGL_OnlyTouchesTheLibvirtGraphicsPath(t *testing.T) {
	m := migration{Name: "t", Apply: "reshapeGraphicsGL"}
	in := "" +
		"candy:\n" +
		"  thing:\n" +
		"    env:\n" +
		"      gl: yes\n" + // an env var that happens to be called gl
		"    plan:\n" +
		"      - run: x\n" +
		"        write:\n" +
		"          gl: yes\n" + // a verb input field
		"vm:\n" +
		"  v:\n" +
		"    libvirt:\n" +
		"      devices:\n" +
		"        video:\n" +
		"          - model: virtio\n" +
		"            gl: yes\n" // video has no gl field; not the graphics path
	out, changed := applyTransform(t, m, in)
	if changed {
		t.Errorf("reshaper touched a gl: outside libvirt.devices.graphics[]:\n%s", out)
	}
	if strings.Contains(out, "enable:") {
		t.Errorf("an unrelated gl: was rewritten:\n%s", out)
	}
}

// A migration hook has no error channel, so an unrecognised scalar is left EXACTLY as
// authored rather than guessed at: `charly migrate` stays silent about that one field and the
// load-time CUE error names it precisely. Inventing a boolean would write a value the author
// never wrote.
func TestReshapeGraphicsGL_LeavesAnUnrecognisedScalarAlone(t *testing.T) {
	m := migration{Name: "t", Apply: "reshapeGraphicsGL"}
	in := "" +
		"vm:\n" +
		"  v:\n" +
		"    libvirt:\n" +
		"      devices:\n" +
		"        graphics:\n" +
		"          - type: spice\n" +
		"            gl: maybe\n"
	out, changed := applyTransform(t, m, in)
	if changed {
		t.Errorf("an unrecognised gl scalar was rewritten:\n%s", out)
	}
	if !strings.Contains(out, "gl: maybe") {
		t.Errorf("the authored value did not survive verbatim:\n%s", out)
	}
}

// TestMigrationTable_ReshapeGraphicsGL: the table carries the graphics-gl reshape as a
// project-only (non-touches_host) apply: goHook entry, strictly after the previous step. A
// vm-kind entity's libvirt: block is a project charly.yml / vm.yml section, never a per-host
// deploy-overlay field, so touches_host would make migrate rewrite files it has no business
// touching.
func TestMigrationTable_ReshapeGraphicsGL(t *testing.T) {
	m := migrationTable[6]
	if m.Name != "reshape-graphics-gl" || m.Apply != "reshapeGraphicsGL" || m.TouchesHost {
		t.Errorf("unexpected seventh table entry: %+v", m)
	}
	if _, ok := goHooks[m.Apply]; !ok {
		t.Errorf("hook %q not registered in goHooks", m.Apply)
	}
	if !migrationTable[5].Version.Less(m.Version) {
		t.Errorf("reshape-graphics-gl version %s must be strictly after install-template-to-phases %s", m.Version, migrationTable[5].Version)
	}
	// The entry's version IS the schema HEAD it migrates TO: a step stamped anything else
	// either never runs (older than HEAD leaves configs unmigrated at HEAD) or runs
	// forever (newer than HEAD never satisfies the gate).
	if got, want := m.Version.String(), "2026.240.1943"; got != want {
		t.Errorf("reshape-graphics-gl version = %s, want the spec #SchemaVersion %s", got, want)
	}
}
