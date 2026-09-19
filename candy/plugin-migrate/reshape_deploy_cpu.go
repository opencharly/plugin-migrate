package migrate

import "gopkg.in/yaml.v3"

// reshape_deploy_cpu.go — the per-deploy VM-shape override cutover: the deploy node's
// CPU field is renamed from the outlier plural `cpus:` to the singular `cpu:`, matching
// every other VM-shape surface (`#Vm`, the template it overrides).
//
// WHY A RESHAPER HOOK, NOT THE DECLARATIVE `rename_key` OP. The obvious op —
// `{op: rename_key, from: cpus, to: cpu, under_kind: vm}` — is UNSAFE. The op-walker's
// under_kind marks EVERY mapping that is nested WITHIN the scoped entity (see
// opTargetMappings: "every mapping that is (or is nested within) an entity value"), so
// it would also match a LIVE `security:` block's own `cpus:` — a string CPU QUOTA
// (`cpus: "2.5"`), a different field with a different type on a different def — and
// rewrite it to the schema-invalid `security: {cpu: "2.5"}`. A key rename keyed on a
// short word therefore cannot be scoped by under_kind alone; it needs the exact PATH,
// which is this hook's job (the reshapeGraphicsGL precedent).
//
// SCOPE, EXACTLY: a `cpus:` key that is a DIRECT child of a substrate kind body
// (`pod:`/`vm:`/`local:`/`kubernetes:`/`android:`) — the `#Deploy` override field. A
// deeper mapping such as `security: {cpus: …}` inside the same `vm:` body is a different
// field with a different type and MUST be left alone. The `#DeployValue` shape puts the
// override fields directly on the kind body, which is what makes the direct-child rule
// correct rather than merely conservative.
//
// The rename is safe because the old `cpus:` key was DEAD (zero readers, zero authors in
// every repo at cutover) — the migration exists for correctness of the wire surface, not
// to preserve live meaning. The VALUE is copied through verbatim: a valid override is an
// int, and an author who wrote something else gets the schema gate's precise error
// rather than a guessed rewrite.
func reshapeDeployCPU(doc *yaml.Node) bool {
	root := doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	return reshapeDeployCPURec(root)
}

// deploySubstrateWords are the substrate kind words that carry a `#Deploy` override
// body. BOTH spellings are listed: `k8s` (the pre-rename form) and `kubernetes` — the
// k8s-to-kubernetes migration (`2026.223.1018`) runs BEFORE this step
// (`2026.261.1747`), so by the time this reshaper sees a config the kind is already
// `kubernetes:`; `k8s` is kept so a direct call is idempotent either way.
var deploySubstrateWords = setOfWords("pod", "vm", "k8s", "kubernetes", "local", "android")

// reshapeDeployCPURec walks the whole document (a deploy node can appear at any nesting
// depth — nested/peer members, per the recordFieldToInstrument precedent). For each
// mapping, it first processes any substrate kind body child, then recurses into every
// child value.
func reshapeDeployCPURec(n *yaml.Node) bool {
	if n == nil {
		return false
	}
	changed := false
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			if reshapeDeployCPURec(c) {
				changed = true
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, val := n.Content[i], n.Content[i+1]
			if deploySubstrateWords[key.Value] && val.Kind == yaml.MappingNode {
				if renameDirectChildKey(val, "cpus", "cpu") {
					changed = true
				}
			}
			if reshapeDeployCPURec(val) {
				changed = true
			}
		}
	}
	return changed
}

// renameDirectChildKey renames a DIRECT child key from → to on mapping m, returning
// whether it changed anything. Only the immediate mapping is touched; nested mappings are
// never descended (that is the whole point — see the file header's security: hazard).
//
// Idempotent: after one run there is no `from` key, so a second run is a no-op. A mapping
// that already carries `to` (both present, a hand-edited file) leaves the existing `to`
// and drops the `from`, rather than producing a duplicate key.
func renameDirectChildKey(m *yaml.Node, from, to string) bool {
	if m == nil || m.Kind != yaml.MappingNode {
		return false
	}
	fromIdx := -1
	hasTo := false
	for i := 0; i+1 < len(m.Content); i += 2 {
		switch m.Content[i].Value {
		case from:
			fromIdx = i
		case to:
			hasTo = true
		}
	}
	if fromIdx < 0 {
		return false
	}
	if hasTo {
		// `to` already present: drop the stale `from` pair (never duplicate the key).
		m.Content = append(m.Content[:fromIdx], m.Content[fromIdx+2:]...)
		return true
	}
	m.Content[fromIdx].Value = to
	return true
}
