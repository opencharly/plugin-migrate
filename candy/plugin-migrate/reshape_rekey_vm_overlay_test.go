package migrate

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// reshape_rekey_vm_overlay_test.go — the per-host-overlay legacy `vm:` key
// migration. The four tests below pin the rule: idempotency, twin-drop,
// orphan hard-error, and the no-op on valid identity keys.

// writeOverlay writes a per-host overlay fixture (~/.config/charly/charly.yml
// stand-in) and returns its path.
func writeOverlay(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "charly.yml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// overlayDeployKeys re-parses a migrated overlay and returns its top-level
// `deploy:` map keys.
func overlayDeployKeys(t *testing.T, path string) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Deploy map[string]any `yaml:"deploy"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("reparse overlay: %v\n%s", err, data)
	}
	keys := make(map[string]bool, len(doc.Deploy))
	for k := range doc.Deploy {
		keys[k] = true
	}
	return keys
}

// TestMigrationTable_RekeyLegacyVMOverlay: the table carries the per-host-overlay
// legacy `vm:` key migration as a touches_host apply: goHook entry (no longer the
// last step — pipeline-lobster-syntax was appended after it).
func TestMigrationTable_RekeyLegacyVMOverlay(t *testing.T) {
	m := migrationTable[len(migrationTable)-2]
	if m.Name != "rekey-legacy-vm-overlay" || m.Apply != "rekeyLegacyVMOverlay" || !m.TouchesHost {
		t.Errorf("unexpected rekey-legacy-vm-overlay table entry: %+v", m)
	}
	if _, ok := goHooks[m.Apply]; !ok {
		t.Errorf("hook %q not registered in goHooks", m.Apply)
	}
}

// TestRekeyLegacyVMOverlay_Idempotent: the first run drops the legacy `vm:` key
// whose dotted twin already carries the identity; the second run is a no-op and
// the overlay is byte-for-byte identical.
func TestRekeyLegacyVMOverlay_Idempotent(t *testing.T) {
	dir := writeRoot(t)
	overlay := writeOverlay(t, ""+
		"deploy:\n"+
		"    vm:charly-check-k3s-vm:\n"+
		"        pod:\n"+
		"            image: legacy\n"+
		"    charly.check-k3s-vm:\n"+
		"        pod:\n"+
		"            image: canonical\n")

	var out bytes.Buffer
	changed, err := runMigrations(&MigrateContext{Dir: dir, HostDeployPath: overlay, Out: &out}, false)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if !changed {
		t.Fatalf("first run must drop the legacy vm: key; output: %q", out.String())
	}
	afterFirst, _ := os.ReadFile(overlay)
	if bytes.Contains(afterFirst, []byte("vm:charly-check-k3s-vm")) {
		t.Errorf("legacy vm: key survived the first run:\n%s", afterFirst)
	}

	out.Reset()
	changed, err = runMigrations(&MigrateContext{Dir: dir, HostDeployPath: overlay, Out: &out}, false)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if changed {
		t.Errorf("second run must be a no-op; output: %q", out.String())
	}
	afterSecond, _ := os.ReadFile(overlay)
	if !bytes.Equal(afterFirst, afterSecond) {
		t.Errorf("second run changed the already-migrated overlay:\nfirst:\n%s\nsecond:\n%s", afterFirst, afterSecond)
	}
}

// TestRekeyLegacyVMOverlay_TwinDrop: `vm:<D>` plus its dotted twin → the vm: key
// is gone, the dotted twin survives with its state intact.
func TestRekeyLegacyVMOverlay_TwinDrop(t *testing.T) {
	dir := writeRoot(t)
	overlay := writeOverlay(t, ""+
		"deploy:\n"+
		"    vm:charly-check-k3s-vm:\n"+
		"        pod:\n"+
		"            image: legacy\n"+
		"    charly.check-k3s-vm:\n"+
		"        pod:\n"+
		"            image: canonical\n")

	var out bytes.Buffer
	changed, err := runMigrations(&MigrateContext{Dir: dir, HostDeployPath: overlay, Out: &out}, false)
	if err != nil {
		t.Fatalf("runMigrations: %v", err)
	}
	if !changed {
		t.Fatalf("expected the legacy vm: key to be dropped; output: %q", out.String())
	}
	keys := overlayDeployKeys(t, overlay)
	if keys["vm:charly-check-k3s-vm"] {
		t.Error("legacy vm: key survived the twin-drop")
	}
	if !keys["charly.check-k3s-vm"] {
		t.Error("the dotted twin was wrongly removed")
	}
	data, _ := os.ReadFile(overlay)
	if !bytes.Contains(data, []byte("canonical")) {
		t.Errorf("the twin's state was lost:\n%s", data)
	}
	if bytes.Contains(data, []byte("legacy")) {
		t.Errorf("the dropped vm: entry's body survived:\n%s", data)
	}
}

// TestRekeyLegacyVMOverlay_OrphanErrors: a lone `vm:<D>` with no dotted twin
// cannot be re-keyed — migrate fails loudly with the exact actionable message
// naming the key, and leaves the overlay untouched.
func TestRekeyLegacyVMOverlay_OrphanErrors(t *testing.T) {
	dir := writeRoot(t)
	overlay := writeOverlay(t, ""+
		"deploy:\n"+
		"    vm:charly-check-k3s-vm:\n"+
		"        pod:\n"+
		"            image: orphan\n")
	before, _ := os.ReadFile(overlay)

	const want = "cannot re-key legacy per-host entry \"vm:charly-check-k3s-vm\": the VM domain identity is lossy (`/`→`-`, `.`→`-`), so the deploy identity is not recoverable from the key. Re-key the entry to its deploy identity (e.g. `charly.check-k3s-vm`) and re-run `charly migrate`."

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("orphan vm: key did not fail migrate")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value is not the actionable string: %#v", r)
		}
		if msg != want {
			t.Fatalf("error message mismatch:\n got: %q\nwant: %q", msg, want)
		}
		after, _ := os.ReadFile(overlay)
		if !bytes.Equal(before, after) {
			t.Errorf("orphan failure must not modify the overlay:\nbefore:\n%s\nafter:\n%s", before, after)
		}
	}()

	var out bytes.Buffer
	_, _ = runMigrations(&MigrateContext{Dir: dir, HostDeployPath: overlay, Out: &out}, false)
	t.Fatal("expected runMigrations to panic on the orphan vm: key")
}

// TestRekeyLegacyVMOverlay_NestingRoundTripUntouched: valid identity keys that
// merely contain `-` or `.` (a nested/namespaced identity, an instance form) are
// never mistaken for legacy `vm:` keys — migrate is a byte-for-byte no-op.
func TestRekeyLegacyVMOverlay_NestingRoundTripUntouched(t *testing.T) {
	dir := writeRoot(t)
	overlay := writeOverlay(t, ""+
		"deploy:\n"+
		"    charly.check-k3s-vm:\n"+
		"        pod:\n"+
		"            image: k3s\n"+
		"    versa/ecovoyage:\n"+
		"        pod:\n"+
		"            image: eco\n")
	before, _ := os.ReadFile(overlay)

	var out bytes.Buffer
	changed, err := runMigrations(&MigrateContext{Dir: dir, HostDeployPath: overlay, Out: &out}, false)
	if err != nil {
		t.Fatalf("runMigrations: %v", err)
	}
	if changed {
		t.Errorf("a valid identity-key overlay must be untouched; output: %q", out.String())
	}
	after, _ := os.ReadFile(overlay)
	if !bytes.Equal(before, after) {
		t.Errorf("overlay modified by migrate:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	keys := overlayDeployKeys(t, overlay)
	if !keys["charly.check-k3s-vm"] || !keys["versa/ecovoyage"] {
		t.Errorf("valid identity keys were damaged: %v", keys)
	}
}
