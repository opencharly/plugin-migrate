package migrate

import "gopkg.in/yaml.v3"

// reshape_engine_word.go — the retired ENGINE WORD cutover (spec#266).
//
// `opencharly/spec` v0.2026295.1243 deleted `nerdctl` from #EngineName, now
// `podman | docker | containerd`, and `#EngineBinaries` maps the WORD `containerd` to
// the CLI `nerdctl`. The word was never the binary name, and no alias or dual path is
// allowed, so an authored `nerdctl` has to become `containerd` on every surface.
//
// TWO SURFACES, TWO SHAPES — and this hook owns only the mapping one:
//
//   - PROJECT (`#Deploy` / `#Candy` / `#Kindcluster`): `engine:` is a SCALAR, so the
//     declarative `remap_scalar` step (`engine-word-containerd`) already covers every
//     occurrence at any depth. This hook does not touch it.
//   - HOST (the runtime config, ~/.config/charly/config.yml — `hostenv.RuntimeConfig`,
//     selected by the step's `host_config: "runtime_config"`): the runtime engine is a
//     MAPPING — `engine: {run: nerdctl}` (`spec/hostenv.EngineConfig`). This is NOT the
//     deploy overlay (~/.config/charly/charly.yml), whose `spec.DeployConfig` has no
//     engine field at all.
//
// WHY A HOOK, NOT A DECLARATIVE OP. The op vocabulary has no parent-path predicate:
// `remap_scalar` can key on `run` but cannot require that `run:` sit under an
// `engine:` parent, and a document-wide `run: nerdctl` remap would rewrite an unrelated
// scalar (a pipeline/check value) that happens to spell the old word. `under_kind`
// cannot scope it either — the runtime config has no entity discriminator, and the field
// is a direct child of the root `engine:` mapping. So the exact path is this hook's job
// (the reshapeDeployCPU precedent).
//
// SCOPE, EXACTLY: an `engine:` key whose value is a MAPPING carrying a DIRECT child
// `run:` scalar equal to `nerdctl`. Direct-child only — EngineConfig is flat (`run`,
// `rootful`), so a deeper `run:` is not this field and is left alone. Idempotent: after
// one run the value is `containerd`, so a second full pass changes nothing.
func remapEngineRunWord(doc *yaml.Node) bool {
	root := doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	return remapEngineRunWordRec(root)
}

func remapEngineRunWordRec(n *yaml.Node) bool {
	if n == nil {
		return false
	}
	changed := false
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			if remapEngineRunWordRec(c) {
				changed = true
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, val := n.Content[i], n.Content[i+1]
			if key.Value == "engine" && val.Kind == yaml.MappingNode {
				if remapScalarDirectChild(val, "run", "nerdctl", "containerd") {
					changed = true
				}
			}
			if remapEngineRunWordRec(val) {
				changed = true
			}
		}
	}
	return changed
}
