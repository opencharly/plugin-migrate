package migrate

import (
	"bytes"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// reshape_retire_empty_group_test.go — the barren `group:` node retirement
// (charly#827). The local runner encodes at indent 4, the SAME setting engine.go's
// write path uses, so the asserted output IS the on-disk migrated shape.

func runRetire(t *testing.T, in string) (string, bool) {
	t.Helper()
	transform, err := buildTransform(migration{Name: "t", Apply: "retireEmptyGroupNode"})
	if err != nil {
		t.Fatalf("buildTransform: %v", err)
	}
	return applyTransformToYAML(t, transform, in)
}

func applyTransformToYAML(t *testing.T, transform func(*yaml.Node) bool, in string) (string, bool) {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(in), &doc); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	changed := transform(&doc)
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(4)
	if err := enc.Encode(&doc); err != nil {
		t.Fatalf("encode: %v", err)
	}
	_ = enc.Close()
	return buf.String(), changed
}

// TestMigrationTable_RetireEmptyGroupNode pins the table entry itself: it is a
// touches_host `apply:` goHook (so the per-host overlay is swept — that is where the
// live residual lives), the hook is registered, and it is ordered AFTER
// unroll-group-deploy (a group WITH a member must be reshaped into its primary before
// this step decides whether anything is left barren).
func TestMigrationTable_RetireEmptyGroupNode(t *testing.T) {
	idx, entry := -1, migration{}
	for i, m := range migrationTable {
		if m.Name == "retire-empty-group-node" {
			idx, entry = i, m
			break
		}
	}
	if idx < 0 {
		t.Fatalf("retire-empty-group-node is not in the migration table")
	}
	if entry.Apply != "retireEmptyGroupNode" || !entry.TouchesHost {
		t.Errorf("unexpected retire-empty-group-node table entry: %+v", entry)
	}
	if _, ok := goHooks[entry.Apply]; !ok {
		t.Errorf("hook %q not registered in goHooks", entry.Apply)
	}
	unroll := -1
	for i, m := range migrationTable {
		if m.Apply == "unrollGroupDeploy" {
			unroll = i
		}
	}
	if unroll < 0 || unroll > idx {
		t.Errorf("retire-empty-group-node (index %d) must follow unroll-group-deploy (index %d)", idx, unroll)
	}
}

// TestRetireEmptyGroupNode_PerHostOverlay is the live shape (charly#827): the
// operator's per-host config carries `check-dsh-pod: {group: {}}` beside the
// `cache:` and `ledger:` sections. The barren node goes; nothing else moves.
func TestRetireEmptyGroupNode_PerHostOverlay(t *testing.T) {
	in := `cache:
    git:
        ttl: 1h
ledger:
    deploys:
        check-dsh-pod:
            state: gone
check-dsh-pod:
    group: {}
`
	out, changed := runRetire(t, in)
	if !changed {
		t.Fatalf("barren group node NOT retired:\n%s", out)
	}
	if strings.Contains(out, "group:") {
		t.Fatalf("`group:` survived:\n%s", out)
	}
	// The TOP-LEVEL entries must be exactly the two sections: the barren node is gone.
	if keys := topLevelKeys(t, out); len(keys) != 2 || keys[0] != "cache" || keys[1] != "ledger" {
		t.Fatalf("top-level keys = %v, want [cache ledger]:\n%s", keys, out)
	}
	for _, keep := range []string{"cache:", "git:", "ttl: 1h", "ledger:", "deploys:", "state: gone"} {
		if !strings.Contains(out, keep) {
			t.Fatalf("unrelated content %q was lost:\n%s", keep, out)
		}
	}
	// The nested ledger entry named check-dsh-pod is a LEDGER LEAF, not a node — it
	// must survive (the hook keys on the `group:` shape, never on the name).
	if !strings.Contains(out, "check-dsh-pod:\n            state: gone") {
		t.Fatalf("the ledger leaf under `deploys:` was dropped:\n%s", out)
	}
}

// topLevelKeys returns the document's top-level mapping keys in order.
func topLevelKeys(t *testing.T, out string) []string {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	n := &doc
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		n = n.Content[0]
	}
	var keys []string
	for i := 0; i+1 < len(n.Content); i += 2 {
		keys = append(keys, n.Content[i].Value)
	}
	return keys
}

// TestRetireEmptyGroupNode_Idempotent pins the contract: a second run reports no change.
func TestRetireEmptyGroupNode_Idempotent(t *testing.T) {
	in := "check-dsh-pod:\n    group: {}\n"
	once, changed := runRetire(t, in)
	if !changed {
		t.Fatalf("first run reported no change")
	}
	if _, again := runRetire(t, once); again {
		t.Fatalf("second run changed an already-retired document:\n%s", once)
	}
}

// TestRetireEmptyGroupNode_LeavesReshapableShapesAlone pins the NARROWNESS: this hook
// retires ONLY the shape with nothing to promote. A group WITH a member is the unroll
// hook's job, and any entity carrying a real kind discriminator (or unknown mapping
// content) is never touched — the hook must not guess and must not drop authored data.
func TestRetireEmptyGroupNode_LeavesReshapableShapesAlone(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"group with a member", "bed:\n    group: {}\n    member-a:\n        pod:\n            image: web\n"},
		{"group with scalars and a member", "bed:\n    group:\n        disposable: true\n    member-a:\n        vm:\n            from: base\n"},
		{"a plain substrate node", "app:\n    local:\n        plan: []\n"},
		{"a node with unknown mapping content", "thing:\n    weird: {}\n"},
		{"group with a non-empty body and no member", "bed:\n    group:\n        disposable: true\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, changed := runRetire(t, tc.in)
			if tc.name == "group with a non-empty body and no member" {
				// scalars only: still nothing to promote -> retired (the body has no
				// post-cutover home either).
				if !changed {
					t.Fatalf("scalar-only barren group not retired:\n%s", out)
				}
				return
			}
			if changed {
				t.Fatalf("hook changed a shape it does not own:\n%s", out)
			}
		})
	}
}

// TestRetireEmptyGroupNode_NestedAndInTableOrder pins (a) a barren group nested inside
// an entity is retired too, and (b) the table's order — unroll FIRST, retire SECOND —
// gives a two-member group its post-cutover primary and retires only what is left
// barren afterwards.
func TestRetireEmptyGroupNode_NestedAndInTableOrder(t *testing.T) {
	unroll, err := buildTransform(migration{Name: "u", Apply: "unrollGroupDeploy"})
	if err != nil {
		t.Fatalf("buildTransform unroll: %v", err)
	}
	retire, err := buildTransform(migration{Name: "r", Apply: "retireEmptyGroupNode"})
	if err != nil {
		t.Fatalf("buildTransform retire: %v", err)
	}
	inOrder := func(in string) (string, bool) {
		t.Helper()
		mid, c1 := applyTransformToYAML(t, unroll, in)
		out, c2 := applyTransformToYAML(t, retire, mid)
		return out, c1 || c2
	}

	// (a) nested barren group inside a surviving entity.
	out, changed := runRetire(t, "outer:\n    local:\n        plan: []\n        leftover:\n            group: {}\n")
	if !changed || strings.Contains(out, "group:") {
		t.Fatalf("nested barren group not retired:\n%s", out)
	}
	if !strings.Contains(out, "outer:") {
		t.Fatalf("the enclosing entity was dropped:\n%s", out)
	}

	// (b) table order: the member promotes, and the (now empty) group node is gone.
	two := "bed:\n    group:\n        disposable: true\n    member-a:\n        pod:\n            image: web\n    member-b:\n        local:\n            plan: []\n"
	got, changed := inOrder(two)
	if !changed {
		t.Fatalf("table order reported no change")
	}
	if strings.Contains(got, "group:") {
		t.Fatalf("`group:` survived the table order:\n%s", got)
	}
	if !strings.Contains(got, "pod:") || !strings.Contains(got, "member-b:") {
		t.Fatalf("a member was lost in the table order:\n%s", got)
	}
}
