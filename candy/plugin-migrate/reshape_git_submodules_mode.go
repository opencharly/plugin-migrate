package migrate

import "gopkg.in/yaml.v3"

// reshape_git_submodules_mode.go — the pin-advance rename (opencharly/opencharly#1347).
//
// `verb:git-submodules`'s advance mode `bump` became `ff` (opencharly/plugin-task#69):
// the verb now only ever FAST-FORWARDS, refusing a move whose target does not descend
// from the checkout's HEAD, and `currency` fails on a pin that is behind the tip its
// intent names. The retired word is refused by the per-plugin schema, so an authored
// `git-submodules: {mode: bump}` has to become `{mode: ff}`.
//
// WHY A HOOK, NOT A DECLARATIVE OP. The op vocabulary has no parent-path predicate:
// `remap_scalar` can key on `mode` but cannot require that `mode:` sit under a
// `git-submodules:` parent, and a document-wide `mode: bump` remap would rewrite an
// unrelated scalar (a check/tool mode) that happens to spell the retired word. This is
// the same reason `remapEngineRunWord` exists for `engine: {run: nerdctl}`, and the
// same shape: the exact path is this hook's job.
//
// SCOPE, EXACTLY: a `git-submodules:` key whose value is a MAPPING carrying a DIRECT
// child `scalar mode:` equal to `bump`. The verb's input is flat (`mode`, `pinned_from`,
// `pin_map`, `intent`, `skip`), so a deeper `mode:` is not this field and is left alone.
// Idempotent: after one run the value is `ff`, so a second full pass changes nothing.
func remapGitSubmodulesMode(doc *yaml.Node) bool {
	root := doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	return remapGitSubmodulesModeRec(root)
}

func remapGitSubmodulesModeRec(n *yaml.Node) bool {
	if n == nil {
		return false
	}
	changed := false
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			if remapGitSubmodulesModeRec(c) {
				changed = true
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, val := n.Content[i], n.Content[i+1]
			if key.Value == "git-submodules" && val.Kind == yaml.MappingNode {
				if remapScalarDirectChild(val, "mode", "bump", "ff") {
					changed = true
				}
			}
			if remapGitSubmodulesModeRec(val) {
				changed = true
			}
		}
	}
	return changed
}
