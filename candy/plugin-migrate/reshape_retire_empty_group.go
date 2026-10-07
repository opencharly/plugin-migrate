package migrate

// reshape_retire_empty_group.go — the DEGENERATE `group:` node retirement (charly#827).
//
// `unroll-group-deploy` promotes a group's FIRST MEMBER into the primary kind. A group with
// NOTHING to promote is deliberately left untouched by that hook — "the load-time gate then names
// the node and points at `charly migrate`". For an EMPTY group (`group: {}`) that gate can never
// clear: the node carries no kind discriminator at all, the loader hard-fails the whole document,
// and its message tells the reader to run `charly migrate` — the very command that just walked
// past the node. The confusion is total, and the blast radius is a whole HOST: the per-host
// overlay is loaded for every `kind: vm` command, so one leftover `check-dsh-pod: {group: {}}` in
// `~/.config/charly/charly.yml` makes every VM bed on that machine unbootable (charly#827).
//
// This hook RETIRES such a node instead of tolerating it. The predicate is deliberately narrow —
// an entry whose value carries a mapping-valued `group:` key and NO other mapping-valued child at
// all (no member to promote, and no other mapping content that would be dropped). That is the one
// shape with no post-cutover spelling: a group with no members cannot become a primary substrate,
// and the modern node form has nowhere to keep it. The group's own SCALAR fields (disposable,
// lifecycle, description, …) go with it, and that is deliberate rather than lossy: a group with no
// member describes no workload, so there is nothing for those scalars to describe. Anything else —
// a group whose first member merely lacks a kind word, a node with some other unknown key — is left
// alone, exactly as before, so this hook never guesses and never discards authored content.
//
// Idempotent: after one run no such entry exists, so a second run reports no change. FROZEN
// SNAPSHOT like its sibling hook: it matches the literal kind word "group" and reads no live
// registry, so it replays against arbitrarily old configs.

import "gopkg.in/yaml.v3"

// retireEmptyGroupNode is the hook named by the `retire-empty-group-node` table entry.
func retireEmptyGroupNode(doc *yaml.Node) bool {
	root := doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	return retireEmptyGroupRec(root)
}

// retireEmptyGroupRec walks the document depth-first, removing every mapping ENTRY whose value is
// a barren group node (see isBarrenGroupNode). It walks in document order and recurses into what
// remains, so a barren group nested inside a surviving entity is retired too.
func retireEmptyGroupRec(n *yaml.Node) bool {
	if n == nil {
		return false
	}
	changed := false
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			if isBarrenGroupNode(n.Content[i+1]) {
				// Drop the key/value pair. Stepping i back by two (the loop adds it again)
				// re-examines the pair now sitting at this index.
				n.Content = append(n.Content[:i], n.Content[i+2:]...)
				changed = true
				i -= 2
			}
		}
	}
	for _, c := range n.Content {
		if retireEmptyGroupRec(c) {
			changed = true
		}
	}
	return changed
}

// isBarrenGroupNode reports whether v is a node that carries a mapping-valued `group:` key and
// nothing else mapping-valued — the degenerate group `unroll-group-deploy` cannot reshape and the
// loader rejects forever.
func isBarrenGroupNode(v *yaml.Node) bool {
	if v == nil || v.Kind != yaml.MappingNode {
		return false
	}
	hasGroup := false
	for i := 0; i+1 < len(v.Content); i += 2 {
		val := v.Content[i+1]
		if val.Kind != yaml.MappingNode {
			continue // a scalar/sequence field (disposable, lifecycle, description, …)
		}
		if v.Content[i].Value == "group" {
			hasGroup = true
			continue
		}
		return false // a mapping-valued sibling: a promotable member, or content we must not drop
	}
	return hasGroup
}
