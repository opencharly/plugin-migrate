package migrate

import (
	"bytes"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/opencharly/sdk/kit"
)

// reshape_group_deploy_test.go — the group: unroll: the two-member rewrite, the
// scalars moved onto the primary, nested-group recursion, idempotence, and the
// degenerate shapes the hook refuses to guess about. The local runner below
// encodes at indent 4 — the SAME setting engine.go's write path uses — so the
// asserted output IS the on-disk migrated shape.

func runUnroll(t *testing.T, in string) (string, bool) {
	t.Helper()
	transform, err := buildTransform(migration{Name: "t", Apply: "unrollGroupDeploy"})
	if err != nil {
		t.Fatalf("buildTransform: %v", err)
	}
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

// mapAt walks path (top-down keys) in the parsed doc and returns the value node.
func mapAt(t *testing.T, out string, path ...string) *yaml.Node {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	n := &doc
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		n = n.Content[0]
	}
	for _, key := range path {
		if n.Kind != yaml.MappingNode {
			t.Fatalf("path %v: parent is not a mapping", path)
		}
		found := false
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == key {
				n = n.Content[i+1]
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("path %v: key %q missing", path, key)
		}
	}
	return n
}

func hasKey(t *testing.T, n *yaml.Node, key string) bool {
	t.Helper()
	if n.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return true
		}
	}
	return false
}

// TestUnrollGroupTwoMembers: the FIRST member (web) becomes the deploy primary —
// the entity key keeps the bed name and gains the member's pod: discriminator —
// and the remaining member (chrome) stays a deploy-level sibling, verbatim.
func TestUnrollGroupTwoMembers(t *testing.T) {
	in := `version: 2026.249.2125
web-chrome:
    group:
        disposable: true
        lifecycle: dev
    web:
        pod:
            from: web-template
    chrome:
        pod:
            from: chrome-template
`
	out, changed := runUnroll(t, in)
	if !changed {
		t.Fatal("unroll reported no change")
	}
	if strings.Contains(out, "group:") {
		t.Errorf("group: key must die:\n%s", out)
	}
	// The primary: the bed kind is now the first member's substrate, carrying the
	// member body verbatim.
	primary := mapAt(t, out, "web-chrome", "pod")
	if v := mapAt(t, out, "web-chrome", "pod", "from"); v.Value != "web-template" {
		t.Errorf("first member's body did not become the primary body: %v", v.Value)
	}
	if hasKey(t, primary, "name") {
		t.Error("primary must not carry a name key")
	}
	// The remaining member keeps its name as a deploy-level sibling.
	if v := mapAt(t, out, "web-chrome", "chrome", "pod", "from"); v.Value != "chrome-template" {
		t.Errorf("remaining member must stay a named sibling with its body: %v", v.Value)
	}
	if hasKey(t, mapAt(t, out, "web-chrome"), "web") {
		t.Error("the consumed member key must die")
	}
}

// TestUnrollGroupScalarsMoved: the group scalars (disposable / lifecycle /
// description / iterate) MOVE onto the primary's kind body.
func TestUnrollGroupScalarsMoved(t *testing.T) {
	in := `version: 2026.249.2125
bed:
    group:
        disposable: true
        lifecycle: dev
        description: the R10 bed
        iterate:
            plateau_iteration: 3
    web:
        pod:
            from: t
`
	out, changed := runUnroll(t, in)
	if !changed {
		t.Fatal("unroll reported no change")
	}
	primary := mapAt(t, out, "bed", "pod")
	for _, want := range []string{"disposable", "lifecycle", "description", "iterate"} {
		if !hasKey(t, primary, want) {
			t.Errorf("group scalar %q not moved onto the primary:\n%s", want, out)
		}
	}
}

// TestUnrollGroupMemberWinsCollision: on a key collision the MEMBER's own value
// wins — the group body was the default, the member the override.
func TestUnrollGroupMemberWinsCollision(t *testing.T) {
	in := `version: 2026.249.2125
bed:
    group:
        description: group-level
        disposable: true
    web:
        pod:
            from: t
            description: member-level
`
	out, changed := runUnroll(t, in)
	if !changed {
		t.Fatal("unroll reported no change")
	}
	primary := mapAt(t, out, "bed", "pod")
	if v := mapAt(t, out, "bed", "pod", "description"); v.Value != "member-level" {
		t.Errorf("the member's own scalar must win, got %q:\n%s", v.Value, out)
	}
	if !hasKey(t, primary, "disposable") {
		t.Errorf("the non-colliding group scalar must still move:\n%s", out)
	}
	if strings.Contains(out, "group-level") {
		t.Errorf("the group body's colliding scalar must die:\n%s", out)
	}
}

// TestUnrollGroupNestedRecursion: a group nested INSIDE a member rewrites FIRST
// (post-order), so the outer unroll sees an already-converted member.
func TestUnrollGroupNestedRecursion(t *testing.T) {
	in := `version: 2026.249.2125
outer:
    group:
        disposable: true
    inner:
        group:
            lifecycle: test
        leaf:
            pod:
                from: leaf-template
`
	out, changed := runUnroll(t, in)
	if !changed {
		t.Fatal("unroll reported no change")
	}
	if strings.Count(out, "group:") != 0 {
		t.Errorf("nested groups must ALL be rewritten:\n%s", out)
	}
	// inner rewrites first (post-order): the leaf member is promoted to the inner
	// primary and the inner scalars move onto it.
	// outer: inner IS the outer group's FIRST member, so it is consumed as the
	// outer primary — its converted pod body leads, carrying BOTH levels' scalars
	// (the member's own keys win on collision; here they merge).
	primary := mapAt(t, out, "outer", "pod")
	if v := mapAt(t, out, "outer", "pod", "from"); v.Value != "leaf-template" {
		t.Errorf("nested member did not become the outer primary:\n%s", out)
	}
	for _, want := range []string{"lifecycle", "disposable"} {
		if !hasKey(t, primary, want) {
			t.Errorf("scalar %q not moved onto the outer primary:\n%s", want, out)
		}
	}
	if hasKey(t, mapAt(t, out, "outer"), "inner") || hasKey(t, mapAt(t, out, "outer"), "leaf") {
		t.Errorf("consumed nested member keys must die:\n%s", out)
	}
}

// TestUnrollGroupIdempotent: running the rewrite twice is a no-op on the second
// pass — the migrated spelling carries no group: key.
func TestUnrollGroupIdempotent(t *testing.T) {
	in := `version: 2026.249.2125
web-chrome:
    group:
        disposable: true
    web:
        pod:
            from: web-template
    chrome:
        pod:
            from: chrome-template
`
	once, _ := runUnroll(t, in)
	if _, changed := runUnroll(t, once); changed {
		t.Errorf("second pass changed an already-migrated doc:\n%s", once)
	}
}

// TestUnrollGroupDegenerateLeftUntouched: a group with NO member (and a member
// that does not carry exactly one mapping-valued kind discriminator) is NEVER
// guessed at — the hook reports no change and the load-time gate names the node.
func TestUnrollGroupDegenerateLeftUntouched(t *testing.T) {
	for name, in := range map[string]string{
		"no-member": `version: 2026.249.2125
bed:
    group:
        disposable: true
`,
		"member-without-kind": `version: 2026.249.2125
bed:
    group:
        disposable: true
    web:
        not-a-kind: true
`,
	} {
		if _, changed := runUnroll(t, in); changed {
			t.Errorf("%s: degenerate group must be left untouched", name)
		}
	}
}

// TestUnrollGroupCommentsPreserved: a sibling member's head comment survives the
// rewrite (the consumed member's comment rides the promoted kind key).
func TestUnrollGroupCommentsPreserved(t *testing.T) {
	in := `version: 2026.249.2125
bed:
    group:
        disposable: true
    web:
        pod:
            from: t
    chrome:
        # the scribe member
        pod:
            from: c
`
	out, changed := runUnroll(t, in)
	if !changed {
		t.Fatal("unroll reported no change")
	}
	if !strings.Contains(out, "# the scribe member") {
		t.Errorf("sibling member comment lost:\n%s", out)
	}
	if !hasKey(t, mapAt(t, out, "bed", "pod"), "disposable") {
		t.Errorf("scalar lost:\n%s", out)
	}
}

// TestMigrationTable_UnrollGroupDeploy: the table carries the group-unroll as its
// LAST (newest) step, pinned at the schema head, with the hook registered.
func TestMigrationTable_UnrollGroupDeploy(t *testing.T) {
	m := migrationTable[len(migrationTable)-1]
	if m.Name != "unroll-group-deploy" || m.Apply != "unrollGroupDeploy" || m.TouchesHost {
		t.Errorf("unexpected last table entry: %+v", m)
	}
	if _, ok := goHooks[m.Apply]; !ok {
		t.Errorf("hook %q not registered in goHooks", m.Apply)
	}
	if !migrationTable[len(migrationTable)-2].Version.Less(m.Version) {
		t.Errorf("unroll-group-deploy version %s must be strictly after the previous step", m.Version)
	}
	if m.Version.String() != kit.LatestSchemaVersion().String() {
		t.Errorf("unroll-group-deploy version %s must be pinned at the schema head", m.Version)
	}
}
