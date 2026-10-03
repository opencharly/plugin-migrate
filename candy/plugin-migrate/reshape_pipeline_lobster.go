package migrate

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// reshape_pipeline_lobster.go — the legacy `kind: pipeline` grammar cutover: rewrite the
// RETIRED stage grammar (`stages:` / `kind:` / `command:` / `skip_when:` / `@stage.output`
// refs) into the NEW lobster-syntax grammar (`steps:` / verb sugar / `run:` / `when:` /
// `$id.json.path` refs), plus the required `description:`, the `config:` consolidation, and
// the `args:` declaration the new schema requires.
//
// WHY A RESHAPER HOOK, NOT THE DECLARATIVE OPS. None of the four generic ops can express
// this step:
//   - they rename / delete / remap / move KEYS; they cannot REBUILD a SEQUENCE ELEMENT —
//     each stage mapping must be replaced by a DIFFERENT mapping shape whose `plan:` value
//     is a NESTED one-element sequence carrying the verb body;
//   - they cannot INVERT an expression (`skip_when: X` ⇒ `when: !(X)`);
//   - they cannot REWRITE string scalars (`@id.out` ⇒ `$id.json.out`, `$pr` ⇒ `${pr}`,
//     `$env.N` ⇒ `${env.N}`) uniformly across every string in the entity;
//   - they cannot ADD a required key (`description:`) nor CONSOLIDATE scattered top-level
//     keys into one `config:` mapping.
// So the cutover registers one goHooks entry, exactly like compactNodeForm.
//
// The vocabulary below is a FROZEN snapshot of the pre-cutover pipeline grammar. A
// migration replays against arbitrarily old configs forever, so nothing here may read the
// live registry or the current plugin-pipeline schema — and the NEW `steps:` grammar does
// not exist in this unit's tree (the lobster schema lands later), which is why there is no
// load-error bed here.
//
// IDEMPOTENCY is structural: a pipeline body is reshaped ONLY when it still carries a
// `stages:` key. After one run the key is `steps:`, so a second run is a no-op — the same
// discipline every step in the table follows.

// reshapePipelineVerbKinds is the closed set of pre-cutover stage kinds that become a
// `plan: [{<kind>: {…}}]` verb-sugar step. `command` is NOT here: it becomes a `run:` step
// (and `run:` is not an object-verb form).
var reshapePipelineVerbKinds = setOfWords("agent", "probe", "ade", "generate", "emit", "media", "gate")

// reshapePipelinePassThroughKeys are the top-level pipeline keys that are ALREADY valid in
// the new grammar and pass through untouched. `stages:` is handled explicitly (it is
// renamed) and `concurrency:` is handled explicitly (it is flattened into `config:`), so
// neither appears here. EVERY other top-level key routes verbatim into `config:`.
var reshapePipelinePassThroughKeys = setOfWords(
	"description", "engine", "args", "env", "cwd", "cost_limit", "triggers", "entities", "config",
)

// The ref rewrite, applied to every string scalar in a reshaped pipeline body. Separate
// ordered patterns (rather than one alternation) because RE2 has no look-behind/-ahead and
// each form must be matched without disturbing the others:
//
//	scalar {pr,calver,workdir}: `$pr` → `${pr}` (`$pr` never matches inside `${pr}` — after
//	    the `$` comes `{`, so the BARE form is exactly the old form and the BRACE form is
//	    exactly the already-new form; both patterns are therefore idempotent)
//	env: `$env.NAME` → `${env.NAME}`
//	stage refs: `@id.out[.field]` → `$id.json.out[.field]`, ONLY when `id` is a stage
//	    declared in the same entity — so `@github.com/...` (a candy ref) is left
//	    byte-identical, never rewritten.
var (
	reshapePipelineScalarBraceRe = regexp.MustCompile(`\$\{(pr|calver|workdir)\}`)
	reshapePipelineScalarBareRe  = regexp.MustCompile(`\$(pr|calver|workdir)`)
	reshapePipelineEnvBraceRe    = regexp.MustCompile(`\$\{env\.([A-Z0-9_]+)\}`)
	reshapePipelineEnvBareRe     = regexp.MustCompile(`\$env\.([A-Z0-9_]+)`)
	reshapePipelineOldStageRefRe = regexp.MustCompile(`@([A-Za-z0-9_-]+)\.([A-Za-z0-9_-]+)(?:\.([A-Za-z0-9_-]+))?`)
)

// reshapePipelineLobster is the goHooks transform: walk the whole document and rewrite every
// `pipeline:` body that still carries the legacy `stages:` grammar.
func reshapePipelineLobster(doc *yaml.Node) bool {
	if doc == nil {
		return false
	}
	return reshapePipelineLobsterRec(doc)
}

// reshapePipelineLobsterRec walks the document tree. A pipeline entity is authored as
// `<name>: {pipeline: {…}}` (nested members make arbitrary depth legal, so this recurses
// defensively). For every mapping `val` carrying a direct `pipeline:` child whose body has
// a `stages:` key, that body is reshaped with the ENCLOSING key as its name; the walk then
// continues into every child (now `steps:`-shaped, so re-visits find nothing).
func reshapePipelineLobsterRec(n *yaml.Node) bool {
	if n == nil {
		return false
	}
	changed := false
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			if reshapePipelineLobsterRec(c) {
				changed = true
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, val := n.Content[i], n.Content[i+1]
			if val.Kind == yaml.MappingNode {
				if pv := reshapeMapValue(val, "pipeline"); pv != nil && pv.Kind == yaml.MappingNode && mappingHasKey(pv, "stages") {
					if reshapePipelineBody(key.Value, pv) {
						changed = true
					}
				}
			}
			if reshapePipelineLobsterRec(val) {
				changed = true
			}
		}
	}
	return changed
}

// reshapePipelineBody rewrites ONE pipeline body `b` (the value of an entity's `pipeline:`
// key) in place, with `name` the enclosing entity key. It returns true (it is only called
// when `stages:` is present, so it always changes something).
func reshapePipelineBody(name string, b *yaml.Node) bool {
	stagesIdx := -1
	for i := 0; i+1 < len(b.Content); i += 2 {
		if b.Content[i].Value == "stages" {
			stagesIdx = i
			break
		}
	}
	stagesSeq := b.Content[stagesIdx+1]
	if stagesSeq.Kind != yaml.SequenceNode {
		panic(fmt.Sprintf("reshape-pipeline-lobster: pipeline %q: `stages:` must be a sequence", name))
	}

	// 1. Collect the DECLARED stage ids FIRST — the ref rewrite only turns `@<id>.<out>`
	//    into `$<id>.json.<out>` when `<id>` names a stage of THIS entity (never
	//    `@github.com/...`, whose `github` is not a stage here).
	ids := map[string]bool{}
	for _, st := range stagesSeq.Content {
		if st.Kind == yaml.MappingNode {
			if idv := reshapeMapValue(st, "id"); idv != nil && idv.Kind == yaml.ScalarNode {
				ids[idv.Value] = true
			}
		}
	}

	// 2. Reshape every stage element (the key is renamed to `steps:` below).
	for i, st := range stagesSeq.Content {
		stagesSeq.Content[i] = reshapePipelineStage(name, st)
	}

	// 3. Rebuild the body's top-level keys: the added `description:` first, then the
	//    already-new keys passed through in place, then the consolidated `config:`
	//    (flattened `concurrency:` + every routed key), then `steps:`.
	config := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	haveConfig := false
	var stepsKey, stepsVal *yaml.Node
	out := []*yaml.Node{}
	hasDescription := mappingHasKey(b, "description")
	for i := 0; i+1 < len(b.Content); i += 2 {
		k, v := b.Content[i], b.Content[i+1]
		switch {
		case k.Value == "stages":
			stepsKey = &yaml.Node{
				Kind: yaml.ScalarNode, Tag: "!!str", Value: "steps",
				HeadComment: k.HeadComment, LineComment: k.LineComment, FootComment: k.FootComment,
			}
			stepsVal = v
		case k.Value == "concurrency":
			// `concurrency: {lanes: N}` flattens to `config: {lanes: N}`.
			if v.Kind != yaml.MappingNode {
				panic(fmt.Sprintf("reshape-pipeline-lobster: pipeline %q: `concurrency:` must be a mapping", name))
			}
			config.Content = append(config.Content, v.Content...)
			haveConfig = true
		case k.Value == "config":
			if v.Kind == yaml.MappingNode {
				config.Content = append(config.Content, v.Content...)
			}
			haveConfig = true
		case reshapePipelinePassThroughKeys[k.Value]:
			out = append(out, k, v)
		default:
			// EVERY other top-level key routes VERBATIM under `config:` — `repo`,
			// `skills`, `media`, `report`, `llm`, `gates`, `channels`, `agent`,
			// any unknown (a `version:` stamp, …). Nothing is dropped.
			config.Content = append(config.Content, k, v)
			haveConfig = true
		}
	}
	if !hasDescription {
		out = append([]*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "description"},
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "Migrated pipeline " + name + "."},
		}, out...)
	}
	if haveConfig && len(config.Content) > 0 {
		out = append(out, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "config"}, config)
	}
	out = append(out, stepsKey, stepsVal)
	b.Content = out

	// 4. Rewrite refs uniformly across every string scalar in the body (values AND keys —
	//    a key can carry a ref too, and a ref in a key must be rewritten the same way).
	reshapePipelineRewriteRefs(b, ids)

	// 5. `$pr` referenced anywhere in the body ⇒ the new schema requires it DECLARED.
	//    `calver`/`workdir` are engine-provided and are never declared here.
	if reshapePipelineUsesRef(b, "${pr}") {
		reshapePipelineEnsureArg(b, "pr", "the value bound per lane by --prs")
	}
	return true
}

// reshapePipelineStage rewrites ONE legacy stage mapping into its new-grammar step mapping.
func reshapePipelineStage(name string, st *yaml.Node) *yaml.Node {
	if st.Kind != yaml.MappingNode {
		panic(fmt.Sprintf("reshape-pipeline-lobster: pipeline %q: every `stages:` entry must be a mapping", name))
	}
	kindNode := reshapeMapValue(st, "kind")
	if kindNode == nil || kindNode.Kind != yaml.ScalarNode || kindNode.Value == "" {
		panic(fmt.Sprintf(
			"reshape-pipeline-lobster: pipeline %q stage %s: no `kind:` — the new grammar has no stage discriminator to infer a verb from, so this cannot be guessed. Add an explicit kind, then re-run.",
			name, reshapePipelineStageIDText(st)))
	}
	switch {
	case kindNode.Value == "command":
		return reshapePipelineCommandStage(name, st)
	case reshapePipelineVerbKinds[kindNode.Value]:
		return reshapePipelineVerbStage(name, st, kindNode.Value)
	default:
		panic(fmt.Sprintf(
			"reshape-pipeline-lobster: pipeline %q stage %s: unknown `kind: %s` — cannot migrate. The old grammar's kinds are command|agent|probe|ade|generate|emit|media|gate.",
			name, reshapePipelineStageIDText(st), kindNode.Value))
	}
}

// reshapePipelineCommandStage turns `kind: command` into a `run:` step. `id` is carried;
// `skip_when` becomes an inverted `when`; `command` becomes `run`; `expect_exit` is
// DROPPED (a `run:` step has no exit contract). Any OTHER field has no meaning on a `run:`
// step and cannot be carried, so it is a HARD ERROR (never silently dropped) — the author
// rewrites it as `plan: [{command: {…}}]`, where every field survives.
//
// Comments are PRESERVED: the surviving keys are the ORIGINAL key nodes (renamed in place
// for `command`→`run` and `skip_when`→`when`), and a dropped `kind:`/`expect_exit:` key's
// leading comment moves onto the step's first key — the whole engine is comment-preserving
// node mutation, and reshape_graphics_gl/deleteDirectChildKey set the same precedent.
func reshapePipelineCommandStage(name string, st *yaml.Node) *yaml.Node {
	var idKey, idVal, commandKey, commandVal, skipKey, skipVal *yaml.Node
	var extraKey *yaml.Node
	var dropped []*yaml.Node
	for i := 0; i+1 < len(st.Content); i += 2 {
		k, v := st.Content[i], st.Content[i+1]
		switch k.Value {
		case "id":
			idKey, idVal = k, v
		case "command":
			commandKey, commandVal = k, v
		case "kind", "expect_exit":
			// dropped: the discriminator is the `run:` step itself; the exit
			// contract does not exist in the new grammar.
			dropped = append(dropped, k)
		case "skip_when":
			skipKey, skipVal = k, v
		default:
			if extraKey == nil {
				extraKey = k
			}
		}
	}
	if extraKey != nil {
		panic(fmt.Sprintf(
			"reshape-pipeline-lobster: pipeline %q stage %s: `kind: command` carries the field `%s:`, which a `run:` step cannot hold and which MUST NOT be silently dropped. Rewrite it by hand as plan: [{command: {…}}] (the verb form carries every field), then re-run.",
			name, reshapePipelineStageIDText(st), extraKey.Value))
	}
	if commandKey == nil {
		panic(fmt.Sprintf("reshape-pipeline-lobster: pipeline %q stage %s: `kind: command` without a `command:` value", name, reshapePipelineStageIDText(st)))
	}
	commandKey.Value = "run"
	out := []*yaml.Node{}
	if idKey != nil {
		out = append(out, idKey, idVal)
	}
	if skipKey != nil {
		skipKey.Value = "when"
		out = append(out, skipKey, reshapePipelineInvertWhen(name, st, skipVal))
	}
	out = append(out, commandKey, commandVal)
	for _, d := range dropped {
		if len(out) > 0 {
			reshapePipelineCarryComments(d, out[0])
		}
	}
	return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: out}
}

// reshapePipelineVerbStage turns `kind: <verb>` into `{id, when?, plan: [{<verb>: {…}}]}`.
// EVERY field except `id`, `kind`, and `skip_when` is carried VERBATIM into the verb body —
// including `redo`, `escalate_after`, `triggers`, `outputs`, `cache`, … Nothing is dropped.
// Comments ride along: surviving keys are the original nodes (renamed in place for
// `skip_when`→`when`), and a dropped `kind:` key's comment moves onto the step's first key.
func reshapePipelineVerbStage(name string, st *yaml.Node, kind string) *yaml.Node {
	var idKey, idVal, skipKey, skipVal, kindKey *yaml.Node
	verb := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for i := 0; i+1 < len(st.Content); i += 2 {
		k, v := st.Content[i], st.Content[i+1]
		switch k.Value {
		case "id":
			idKey, idVal = k, v
		case "kind":
			// the discriminator is replaced by the verb-sugar key — dropped here.
			kindKey = k
		case "skip_when":
			skipKey, skipVal = k, v
		default:
			verb.Content = append(verb.Content, k, v)
		}
	}
	planElem := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
		reshapePipelineScalarKey(kind), verb,
	}}
	planSeq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{planElem}}
	out := []*yaml.Node{}
	if idKey != nil {
		out = append(out, idKey, idVal)
	}
	if skipKey != nil {
		skipKey.Value = "when"
		out = append(out, skipKey, reshapePipelineInvertWhen(name, st, skipVal))
	}
	planKey := reshapePipelineScalarKey("plan")
	out = append(out, planKey, planSeq)
	reshapePipelineCarryComments(kindKey, out[0])
	return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: out}
}

// reshapePipelineCarryComments moves a dropped key's comments onto another node when that
// node has none — so a `kind:` comment is not silently lost (the deleteDirectChildKey /
// reshape_graphics_gl precedent).
func reshapePipelineCarryComments(from, to *yaml.Node) {
	if from == nil || to == nil {
		return
	}
	if to.HeadComment == "" && from.HeadComment != "" {
		to.HeadComment = from.HeadComment
	}
	if to.LineComment == "" && from.LineComment != "" {
		to.LineComment = from.LineComment
	}
	if to.FootComment == "" && from.FootComment != "" {
		to.FootComment = from.FootComment
	}
}

// reshapePipelineInvertWhen rewrites `skip_when: X` to `when: !(X)` — X's text VERBATIM,
// never re-parenthesized beyond the one enclosing negation. The ref rewrite runs later over
// the whole body, so any `@stage`/`$env` ref inside X is rewritten with everything else.
func reshapePipelineInvertWhen(name string, st, skip *yaml.Node) *yaml.Node {
	if skip.Kind != yaml.ScalarNode {
		panic(fmt.Sprintf("reshape-pipeline-lobster: pipeline %q stage %s: `skip_when:` must be a scalar expression", name, reshapePipelineStageIDText(st)))
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "!(" + skip.Value + ")"}
}

// reshapePipelineRewriteRefs applies the ref rewrite to every string scalar in n (keys and
// values alike).
func reshapePipelineRewriteRefs(n *yaml.Node, ids map[string]bool) {
	if n == nil {
		return
	}
	if n.Kind == yaml.ScalarNode {
		if n.Tag == "" || n.Tag == "!!str" {
			n.Value = reshapePipelineRewriteRefString(n.Value, ids)
		}
		return
	}
	for _, c := range n.Content {
		reshapePipelineRewriteRefs(c, ids)
	}
}

// reshapePipelineRewriteRefString rewrites one scalar. Everything not matched is preserved
// byte-for-byte: `$latest`, `$b`, `$f`, `$s`, `${golden}`, `${sha}`, `${files}`,
// `${tests}`, `${checks}`, `${drive}`, the knob markers `${tests_body:indent}`,
// `${checks:negate}`, `${suggestions:bullets}`, `${what}`, and every `@github.com/...`
// candy ref.
func reshapePipelineRewriteRefString(s string, ids map[string]bool) string {
	if !strings.ContainsAny(s, "$@") {
		return s
	}
	s = reshapePipelineScalarBraceRe.ReplaceAllString(s, "${$1}")
	s = reshapePipelineScalarBareRe.ReplaceAllString(s, "${$1}")
	s = reshapePipelineEnvBraceRe.ReplaceAllString(s, "${env.$1}")
	s = reshapePipelineEnvBareRe.ReplaceAllString(s, "${env.$1}")
	s = reshapePipelineOldStageRefRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := reshapePipelineOldStageRefRe.FindStringSubmatch(m)
		id, out, field := sub[1], sub[2], sub[3]
		if !ids[id] {
			return m // e.g. @github.com/... — a candy ref, never a stage ref.
		}
		if field != "" {
			return "$" + id + ".json." + out + "." + field
		}
		return "$" + id + ".json." + out
	})
	return s
}

// reshapePipelineUsesRef reports whether any string scalar in n contains needle.
func reshapePipelineUsesRef(n *yaml.Node, needle string) bool {
	if n == nil {
		return false
	}
	if n.Kind == yaml.ScalarNode {
		return strings.Contains(n.Value, needle)
	}
	for _, c := range n.Content {
		if reshapePipelineUsesRef(c, needle) {
			return true
		}
	}
	return false
}

// reshapePipelineEnsureArg adds `args.<name>: {type: string, description: …}` if the body
// does not already declare it. `args:` is inserted before `steps:` when absent.
func reshapePipelineEnsureArg(b *yaml.Node, name, desc string) {
	args := reshapeMapValue(b, "args")
	if args != nil && args.Kind != yaml.MappingNode {
		panic(fmt.Sprintf("reshape-pipeline-lobster: `args:` must be a mapping to declare `%s` under it", name))
	}
	if args == nil {
		args = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		idx := len(b.Content)
		for i := 0; i+1 < len(b.Content); i += 2 {
			if b.Content[i].Value == "steps" {
				idx = i
				break
			}
		}
		insert := []*yaml.Node{reshapePipelineScalarKey("args"), args}
		head := append([]*yaml.Node{}, b.Content[:idx]...)
		head = append(head, insert...)
		b.Content = append(head, b.Content[idx:]...)
	}
	if reshapeMapValue(args, name) != nil {
		return
	}
	args.Content = append(args.Content,
		reshapePipelineScalarKey(name),
		&yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
			reshapePipelineScalarKey("type"), reshapePipelineScalarValue("string"),
			reshapePipelineScalarKey("description"), reshapePipelineScalarValue(desc),
		}})
}

// reshapePipelineScalarKey / reshapePipelineScalarValue build plain `!!str` nodes.
func reshapePipelineScalarKey(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

func reshapePipelineScalarValue(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

// reshapePipelineStageIDText renders a stage's id for a panic message.
func reshapePipelineStageIDText(st *yaml.Node) string {
	if idv := reshapeMapValue(st, "id"); idv != nil && idv.Kind == yaml.ScalarNode {
		return `id "` + idv.Value + `"`
	}
	return "(no id)"
}
