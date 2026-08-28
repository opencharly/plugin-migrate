package migrate

import (
	"strings"
	"testing"
)

// TestInstallTemplateToPhases_MovesToPhaseContainer: the reshaper MOVES a
// `format:`/`builder:` def's direct `install_template:` scalar into
// `phase.install.container` across all four shapes — a def with NO phase block
// (fresh phase created at the old position), a def whose phase.install already
// carries a `host:` cell (container inserted beside it), a def that already has
// a `container:` cell (a partially-migrated config — the cell's value is replaced),
// and a per-entity builder def (`<entity>: {builder: {<def fields directly>}}`).
func TestInstallTemplateToPhases_MovesToPhaseContainer(t *testing.T) {
	m := migration{Name: "t", Apply: "installTemplateToPhases"}
	in := "" +
		"format:\n" +
		"  apk:\n" +
		"    # the apk container template\n" +
		"    install_template: |\n" +
		"      RUN apk add --no-cache\n" +
		"      {{- range .Packages}} {{.}}{{end}}\n" +
		"  pac:\n" +
		"    phase:\n" +
		"      install:\n" +
		"        host: |\n" +
		"          pacman -Syu --needed{{range .Packages}} {{.}}{{end}}\n" +
		"    install_template: |\n" +
		"      RUN pacman-key --recv-keys{{range .Keys}} {{.}}{{end}}\n" +
		"  rpm:\n" +
		"    phase:\n" +
		"      install:\n" +
		"        container: |\n" +
		"          RUN dnf install -y OLD\n" +
		"    install_template: |\n" +
		"      RUN dnf install -y NEW\n" +
		"builder:\n" +
		"  cargo:\n" +
		"    cache_mount:\n" +
		"      - dst: /tmp/cargo-cache\n" +
		"    install_template: |\n" +
		"      RUN cargo install --path /ctx\n" +
		"go:\n" +
		"  builder:\n" +
		"    detect_file:\n" +
		"      - go.mod\n" +
		"    install_template: |\n" +
		"      RUN go install ./...\n"
	out, changed := applyTransform(t, m, in)
	if !changed {
		t.Fatal("expected install_template fields to be moved")
	}
	// The retired key is gone entirely; every template body survives exactly once;
	// the partially-migrated rpm container cell was REPLACED (the OLD body is gone);
	// the pac host cell is untouched beside its new container sibling; the apk key's
	// head comment rides onto the new container cell.
	for _, want := range []string{
		"# the apk container template",  // the retired key's comment rides onto container
		"RUN apk add --no-cache",        // apk: fresh phase block, body preserved
		"pacman -Syu --needed",          // pac: host cell untouched beside the new container
		"RUN pacman-key --recv-keys",    // pac: container inserted
		"RUN dnf install -y NEW",        // rpm: existing container replaced
		"RUN cargo install --path /ctx", // builder def moved (defs-map shape)
		"RUN go install ./...",          // builder def moved (per-entity shape)
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "install_template:") {
		t.Errorf("an install_template: key survived the move:\n%s", out)
	}
	if strings.Contains(out, "RUN dnf install -y OLD") {
		t.Errorf("the replaced rpm container cell's OLD value survived:\n%s", out)
	}
	// A second pass is a no-op (idempotent).
	if _, changed2 := applyTransform(t, m, out); changed2 {
		t.Error("second pass changed an already-migrated doc")
	}
}

// TestInstallTemplateToPhases_LeavesRetainedFields: the reshaper does NOT touch the
// RETAINED `install_template` uses — distro `local_pkg:`.install_template and
// distro `bootloader:`.install_template — because they sit under different parent
// keys than the format/builder defs.
func TestInstallTemplateToPhases_LeavesRetainedFields(t *testing.T) {
	m := migration{Name: "t", Apply: "installTemplateToPhases"}
	in := "" +
		"distro:\n" +
		"  debian:\n" +
		"    format:\n" +
		"      deb:\n" +
		"        local_pkg:\n" +
		"          install_template: apt-get install -y {{.Glob}}\n" +
		"          probe: command -v apt-get\n" +
		"    bootloader:\n" +
		"      install_template: |\n" +
		"        # bootloader content stays put\n"
	out, changed := applyTransform(t, m, in)
	if changed {
		t.Errorf("hook reported changed=true but only retained fields present:\n%s", out)
	}
	for _, want := range []string{
		"local_pkg:\n          install_template: apt-get install -y {{.Glob}}",
		"bootloader:\n      install_template: |",
		"# bootloader content stays put",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("retained field damaged, missing %q:\n%s", want, out)
		}
	}
}

// TestMigrationTable_InstallTemplateToPhases: the table carries the
// install_template → phase.install.container move as the 6th (newest) entry — a
// project-only (non-touches_host) apply: goHook entry, strictly after
// remove-candy-localpkg, with the hook registered.
func TestMigrationTable_InstallTemplateToPhases(t *testing.T) {
	m := migrationTable[5]
	if m.Name != "install-template-to-phases" || m.Apply != "installTemplateToPhases" || m.TouchesHost {
		t.Errorf("unexpected sixth table entry: %+v", m)
	}
	if _, ok := goHooks[m.Apply]; !ok {
		t.Errorf("hook %q not registered in goHooks", m.Apply)
	}
	if !migrationTable[4].Version.Less(m.Version) {
		t.Errorf("install-template-to-phases version %s must be strictly after remove-candy-localpkg %s", m.Version, migrationTable[4].Version)
	}
}

// The in-tree vocabulary guard that used to live here — asserting charly's embedded
// build vocabulary (charly/charly.yml, charly/testdata/build.yml) carries no residual
// format/builder-level `install_template:` — has MOVED to the charly repo, where those
// files actually live: charly/embed_defaults_install_template_test.go
// (opencharly/charly#424).
//
// It read `../../charly/charly.yml`, a path that resolved only while this plugin was a
// directory inside the charly repo. The de-submodule (c1b8b56, "remove the stray charly
// submodule") left the path dangling, and nothing caught it because this repo's CI runs
// `charly box validate` and never `go test`, so the guard had been dead — not failing,
// simply never executed — ever since. A guard that cannot run is worse than no guard:
// it reads as coverage.
//
// Keeping it here would need charly's source at a path this repo cannot guarantee, and
// the only ways to do that are a network fetch inside a unit test or a skip when the file
// is absent — and a skip is exactly how it died the first time.
