package migrate

// reshape_group_deploy.go — the targetless deploy `group:` node unroll (Cutover C
// task 1, consumer half). The group kind is REMOVED from #ResourceKind (spec #105):
// a residual authored `group:` node is a hard load error. This hook rewrites the
// authored shape to the post-migrate member-tree spelling documented at the removal:
//
//   - the FIRST member (document order) becomes the deploy PRIMARY — the entity
//     keeps its own name and gains the member's substrate-kind discriminator; the
//     member's own key is consumed (the entity name replaces it); the member's
//     ENTIRE value promotes (a member carrying IN-SUBSTRATE nested members — e.g. a
//     vm member nesting a local deploy — keeps them inside the promoted node);
//   - the group scalars (disposable / lifecycle / description / iterate — every
//     group-body field has a #Deploy-body home) MOVE onto that primary's kind
//     body; on a key collision the MEMBER's own value wins (the group body was the
//     default, the member the override);
//   - the REMAINING members stay deploy-level siblings, verbatim;
//   - a group nested INSIDE a member rewrites first (post-order recursion), so an
//     outer unroll always sees an already-converted member.
//
// Idempotent: a document with no mapping-valued `group:` key reports no change.
// A DEGENERATE group (no member to promote, or a first member whose value does not
// carry exactly one mapping-valued kind discriminator) is left untouched — the
// load-time gate then names the node and points at `charly migrate`; the hook
// never guesses a primary. This is a FROZEN-SNAPSHOT reshaper: it matches only the
// literal kind word "group" as a mapping-valued key and reads no live registry,
// so it replays against arbitrarily old configs.

import "gopkg.in/yaml.v3"

func unrollGroupDeploy(doc *yaml.Node) bool {
	root := doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	return unrollGroupDeployRec(root)
}

// unrollGroupDeployRec walks the whole document tree POST-ORDER (a nested group
// converts before its enclosing entity). Key scalars fall through untouched —
// only mapping VALUES can carry a nested entity.
func unrollGroupDeployRec(n *yaml.Node) bool {
	if n == nil {
		return false
	}
	changed := false
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode, yaml.MappingNode:
		for _, c := range n.Content {
			if unrollGroupDeployRec(c) {
				changed = true
			}
		}
	}
	if n != nil && n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == "group" && n.Content[i+1].Kind == yaml.MappingNode {
				if unrollOneGroup(n, i) {
					changed = true
				}
				break // "group" is a unique key per mapping — one unroll per entity
			}
		}
	}
	return changed
}

// unrollOneGroup converts ONE group entity: parent.Content[gi] is the `group:`
// discriminator, the sibling pairs are its members. Returns false (untouched) when
// no member can be promoted unambiguously.
func unrollOneGroup(parent *yaml.Node, gi int) bool {
	body := parent.Content[gi+1]

	// The FIRST member (document order) becomes the primary.
	memberIdx := -1
	for j := 0; j+1 < len(parent.Content); j += 2 {
		if j == gi {
			continue
		}
		if parent.Content[j+1].Kind == yaml.MappingNode {
			memberIdx = j
			break
		}
	}
	if memberIdx < 0 {
		return false // degenerate: no member to promote — leave it to the load gate
	}
	memberKey, memberValue := parent.Content[memberIdx], parent.Content[memberIdx+1]

	// The member's kind discriminator: the FROZEN substrate vocabulary (migrations
	// replay against arbitrarily old configs — no live registry), exactly ONE
	// mapping-valued kind word, else ambiguous. A member value may ALSO carry
	// mapping keys that are IN-SUBSTRATE nested members (e.g. a vm member nesting
	// a local deploy) — those are not discriminators; they promote verbatim inside
	// the primary node.
	kindIdx := -1
	for j := 0; j+1 < len(memberValue.Content); j += 2 {
		if memberValue.Content[j+1].Kind != yaml.MappingNode || !frozenSubstrateWords[memberValue.Content[j].Value] {
			continue
		}
		if kindIdx >= 0 {
			return false // two kind words — never guess a primary
		}
		kindIdx = j
	}
	if kindIdx < 0 {
		return false
	}
	kindKey, kindBody := memberValue.Content[kindIdx], memberValue.Content[kindIdx+1]

	// Move the group-body fields onto the primary's kind body; the member's own
	// keys win on collision. Nodes move VERBATIM (the group body is removed from
	// the document right after, so a moved node never has two reachable parents).
	for j := 0; j+1 < len(body.Content); j += 2 {
		if reshapeMapValue(kindBody, body.Content[j].Value) == nil {
			kindBody.Content = append(kindBody.Content, body.Content[j], body.Content[j+1])
		}
	}

	// Comment preservation: the promoted kind key inherits the member key's
	// comments (the member name is consumed), falling back to the group key's.
	groupKey := parent.Content[gi]
	if kindKey.HeadComment == "" {
		if memberKey.HeadComment != "" {
			kindKey.HeadComment = memberKey.HeadComment
		} else {
			kindKey.HeadComment = groupKey.HeadComment
		}
	}
	if kindKey.LineComment == "" && memberKey.LineComment != "" {
		kindKey.LineComment = memberKey.LineComment
	}

	// Rebuild the entity: the primary leads — the member's ENTIRE value (its kind
	// pair + any in-substrate nested-member pairs) becomes the entity's own content,
	// then the remaining members keep their document order; the `group:` pair and
	// the consumed member pair die.
	newContent := append([]*yaml.Node{}, memberValue.Content...)
	for j := 0; j < len(parent.Content); j += 2 {
		if j == gi || j == memberIdx {
			continue
		}
		newContent = append(newContent, parent.Content[j], parent.Content[j+1])
	}
	parent.Content = newContent
	return true
}

// frozenSubstrateWords is the FROZEN substrate-kind vocabulary a group member's
// discriminator is recognized by (the 5 living substrate words + the retired k8s
// spelling — renamed before this step by the k8s-to-kubernetes row, kept so an
// arbitrarily old config still reshapes cleanly).
var frozenSubstrateWords = setOfWords("pod", "vm", "kubernetes", "local", "android", "k8s")
