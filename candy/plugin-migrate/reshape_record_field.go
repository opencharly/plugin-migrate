package migrate

import "gopkg.in/yaml.v3"

// reshape_record_field.go — the legacy deploy record: field harvest (Cutover A): the
// bed-level whole-run recording wrap becomes the instrument: entry of the new capture
// model. A deploy node record: block
//
//	record: {record_name: "pr-flow", desktop: true, fps: 4, artifact: "/tmp/pr.cast"}
//
// harvests into the equivalent instrument entry (the runner-owned capture contract):
//
//	instrument:
//	  - id: pr-flow
//	    phase: [live]
//	    record: {method: session, desktop: true, fps: 4, artifact: "/tmp/pr.cast"}
//
// record_name becomes the instrument id (default "recording"); the remaining #RecordWrap
// value fields pass through into the record verb input (method: session is the session
// contract) — the word provider decides which of the old knobs still apply.
//
// A record: that carries method: is a PLAN-STEP verb sugar (the record verb is a live
// probe word), NOT the deploy field — the migrate must never touch step sugar. The two
// are structurally distinguishable by VALUE SHAPE alone (only the deploy field mapping
// has no method key), the stripDeployShellOverlay precedent.
func recordFieldToInstrument(doc *yaml.Node) bool {
	root := doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	return recordFieldToInstrumentRec(root)
}

// recordFieldToInstrumentRec walks the whole document tree (a deploy record: field can
// appear on any substrate node — pod/vm/local/kubernetes — at any nesting depth, e.g. a
// nested/peer member). For each mapping carrying a mapping-valued record: key it runs the
// harvest, then unconditionally recurses into every child value (never a key, always a
// scalar) — mirroring stripDeployShellOverlayRec iteration shape: the deletion + append
// on THIS mapping content never invalidates the loop that finds it, because the loop
// below re-reads n.Content fresh via len() each iteration.
func recordFieldToInstrumentRec(n *yaml.Node) bool {
	if n == nil {
		return false
	}
	changed := false
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			if recordFieldToInstrumentRec(c) {
				changed = true
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == "record" && n.Content[i+1].Kind == yaml.MappingNode {
				if harvestRecordField(i, n) {
					changed = true
				}
				break // "record" is a unique key per mapping — nothing left to find here
			}
		}
		for _, c := range n.Content {
			if recordFieldToInstrumentRec(c) {
				changed = true
			}
		}
	}
	return changed
}

// harvestRecordField converts ONE deploy record: mapping (keyed at index i in parent) into
// the instrument: entry. Returns false when the record: is step sugar (has method:), a
// malformed instrument: exists, or the key is not a deploy mapping.
func harvestRecordField(i int, parent *yaml.Node) bool {
	rec := parent.Content[i+1]
	// A record STEP sugar always carries method: — skip (the plan-step verb, not the
	// deploy field).
	if reshapeMapValue(rec, "method") != nil {
		return false
	}
	name := "recording"
	if nv := reshapeMapValue(rec, "record_name"); nv != nil && nv.Value != "" {
		name = nv.Value
	}

	// The instrument entry mapping: {id, phase: [live], record: {method: session, <old
	// value fields copied verbatim>}}.
	entry := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	entry.Content = append(entry.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "id"},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "phase"},
		&yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq",
			Content: []*yaml.Node{{Kind: yaml.ScalarNode, Tag: "!!str", Value: "live"}}})
	verb := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map",
		Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "method"},
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "session"},
		}}
	for j := 0; j+1 < len(rec.Content); j += 2 {
		k := rec.Content[j]
		if k.Value == "record_name" {
			continue // consumed as the instrument id
		}
		// Copy the value node VERBATIM. The source mapping (rec) is REMOVED from the
		// document by the caller right after, so a moved value node never ends up under
		// two reachable parents.
		verb.Content = append(verb.Content, k, rec.Content[j+1])
	}
	entry.Content = append(entry.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "record"}, verb)

	// Replace or extend the instrument: list.
	instr := reshapeMapValue(parent, "instrument")
	switch {
	case instr == nil:
		parent.Content = append(parent.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "instrument"},
			&yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{entry}})
	case instr.Kind == yaml.SequenceNode:
		instr.Content = append(instr.Content, entry)
	default:
		return false // malformed instrument: — leave it to validation to diagnose
	}
	// Remove the legacy field (comment-preserving deletion).
	return deleteDirectChildKey(parent, "record")
}
