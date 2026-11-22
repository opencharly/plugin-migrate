package migrate

import (
	"strings"
	"testing"
)

// TestMigrationTable_AurNotABuildFormat: aur is a builder, not a build format (opencharly/sdk#608),
// so `aur` leaves every `build:` list. The step is one value-scoped remove_list_value: it never
// touches a run step's `build: all` verb, an emptied list (or a bare `build: aur`) loses the key so
// the default formats apply, and a second pass changes nothing.
func TestMigrationTable_AurNotABuildFormat(t *testing.T) {
	m := findMigration(t, "aur-not-a-build-format")
	if len(m.Ops) != 1 || m.Ops[0].Op != "remove_list_value" || m.Ops[0].Key != "build" || m.Ops[0].Value != "aur" {
		t.Fatalf("unexpected table entry: %+v", m)
	}
	if m.TouchesHost || m.Apply != "" {
		t.Errorf("a pure PROJECT ops step must carry no host flag and no hook: %+v", m)
	}
	cases := []struct{ name, in, want, absent string }{
		{"list keeps the OS formats", "arch-test:\n  candy:\n    base: arch\n    build:\n      - pac\n      - aur\n", "build:\n      - pac\n", "aur"},
		{"flow list", "b:\n  candy:\n    build: [pac, aur]\n", "build: [pac]", "aur"},
		{"emptied list loses the key", "b:\n  candy:\n    base: arch\n    build: [aur]\n", "base: arch", "build"},
		{"scalar loses the key", "b:\n  candy:\n    base: arch\n    build: aur\n", "base: arch", "build"},
		{"defaults list", "defaults:\n  build: [pac, aur]\n", "build: [pac]", "aur"},
	}
	for _, c := range cases {
		out, changed := applyTransform(t, m, c.in)
		if !changed || !strings.Contains(out, c.want) || strings.Contains(out, c.absent) {
			t.Errorf("%s: got %q (changed=%v), want it to contain %q and not %q", c.name, out, changed, c.want, c.absent)
		}
		if again, changed := applyTransform(t, m, out); changed || again != out {
			t.Errorf("%s: a second pass changed the document: %q", c.name, again)
		}
	}
	// A run step's `build:` verb and an aur DECLARATION are not build lists: untouched.
	for _, in := range []string{
		"c:\n  candy:\n    plan:\n      - run: compile\n        build: all\n",
		"c:\n  candy:\n    distro:\n      arch:\n        aur:\n          package: [kdotool]\n",
	} {
		if out, changed := applyTransform(t, m, in); changed {
			t.Errorf("the step touched a document with no build list naming aur: %q", out)
		}
	}
}
