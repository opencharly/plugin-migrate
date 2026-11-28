package migrate

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// reshape_git_submodules_mode_test.go — the pin-advance rename
// (opencharly/opencharly#1347, opencharly/plugin-task#69).
//
// The `git-submodules` verb's advance mode `bump` became `ff` and the old word is
// refused by the plugin's schema. The reshape is PATH-SCOPED: only a `mode:` that is a
// DIRECT child of a `git-submodules:` mapping is rewritten, because a document-wide
// `mode: bump` remap would rewrite an unrelated scalar that happens to spell it — the
// reason this is an apply: hook and not a declarative remap_scalar.
//
// Both tests run the FULL engine (every table step, in order), so they also prove the
// step is idempotent in the real chain.

// TestRunMigrations_GitSubmodulesModeFF: an authored `git-submodules: {mode: bump}`
// migrates to `{mode: ff}`, an unrelated `mode: bump` elsewhere is UNTOUCHED, and a
// second run is a no-op.
func TestRunMigrations_GitSubmodulesModeFF(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "charly.yml")
	body := "" +
		"pins:\n" +
		"    task:\n" +
		"        description: d\n" +
		"        plan:\n" +
		"            - run: advance every pin\n" +
		"              git-submodules:\n" +
		"                  mode: bump\n" +
		"                  pinned_from: charly\n" +
		"            - run: an unrelated tool mode that happens to spell the retired word\n" +
		"              tool:\n" +
		"                  mode: bump\n"
	if err := os.WriteFile(root, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	changed, err := runMigrations(&MigrateContext{Dir: dir, Out: &out}, true)
	if err != nil {
		t.Fatalf("runMigrations: %v", err)
	}
	if !changed {
		t.Fatalf("the retired mode word must be migrated, output: %q", out.String())
	}
	if !strings.Contains(out.String(), "applied git-submodules-mode-ff") {
		t.Errorf("want the git-submodules step reported, output: %q", out.String())
	}
	afterFirst, err := os.ReadFile(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(afterFirst), "mode: ff"); got != 1 {
		t.Errorf("want exactly the git-submodules mode rewritten to ff, got %d:\n%s", got, afterFirst)
	}
	// The path scope: the unrelated `mode: bump` is NOT this field and must survive.
	if got := strings.Count(string(afterFirst), "mode: bump"); got != 1 {
		t.Errorf("want the unrelated `mode: bump` left alone, got %d:\n%s", got, afterFirst)
	}
	assertGitSubmodulesModeBlock(t, string(afterFirst), "ff")

	// Idempotence: the second full run changes nothing.
	out.Reset()
	changed, err = runMigrations(&MigrateContext{Dir: dir, Out: &out}, true)
	if err != nil {
		t.Fatalf("runMigrations (second): %v", err)
	}
	if changed {
		t.Errorf("the second run must be a no-op, output: %q", out.String())
	}
	afterSecond, err := os.ReadFile(root)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterFirst) != string(afterSecond) {
		t.Errorf("the second run changed the file:\nbefore:\n%s\nafter:\n%s", afterFirst, afterSecond)
	}
}

// assertGitSubmodulesModeBlock asserts the mode DIRECTLY under the git-submodules
// mapping carries want — proving the rewrite landed on the intended child rather than
// anywhere the string appears.
func assertGitSubmodulesModeBlock(t *testing.T, doc, want string) {
	t.Helper()
	i := strings.Index(doc, "git-submodules:")
	if i < 0 {
		t.Fatalf("fixture lost the git-submodules step:\n%s", doc)
	}
	block := doc[i:]
	if j := strings.Index(block, "- run:"); j >= 0 {
		block = block[:j]
	}
	if !strings.Contains(block, "mode: "+want) {
		t.Fatalf("the git-submodules block does not carry mode: %s:\n%s", want, block)
	}
}
