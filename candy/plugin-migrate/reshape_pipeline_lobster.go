package migrate

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// reshape_pipeline_lobster.go — the legacy `kind: pipeline` grammar cutover: rewrite the
// RETIRED stage grammar (`stages:` / `kind:` / `command:` / `skip_when:` / `@stage.output`
// refs) into the NEW lobster-syntax grammar (`steps:` / verb sugar / `run:` / `when:` /
// `$id.json.path` refs), plus the required `description:`, the `config:` consolidation, the
// `args:` declaration the new schema requires, and the duplication of the entity-level knobs
// each lifted verb reads into that verb's own body (reshapePipelineVerbKnobs) so the lifted
// verb is self-contained.
//
// THE POOLED FAN-OUT ARM. The retired executor's `concurrency: {lanes: N}` was the WHOLE
// pipeline's worker-pool width over the run's PR list (the deleted `runBatch`: N goroutines
// pulling items from `--prs`, each item bound as `$pr`, every stage run once per item, ANY
// failed lane failing the run). The new grammar's equivalent is the loop ARM — a step
// carrying `lanes: <collection>` + `batch_size: N` + `item_var` + a `steps:` body — so the
// migration wraps the whole migrated step list in ONE lane step:
//
//	concurrency: {lanes: 16}            →   steps:
//	                                           - id: lane
//	                                             lanes: ${prs}
//	                                             batch_size: 16
//	                                             item_var: pr
//	                                             steps: [<the migrated stages>]
//
// The collection is the run's `prs` ARG, not a seed step: `#WorkflowArgVal` admits a FLAT
// LIST of strings (spec#262), `plugin-lobster`'s `resolveLoopItems` parses a templated
// (non-ref) `lanes:` operand as JSON, and the arg reaches a `run:` body's `${…}` template
// through `renderTemplateValue` (a non-scalar renders as JSON). So `--prs a b c` becomes
// `--args-json '{"prs":["a","b","c"]}'` and the arm consumes it directly — no invented
// intermediate step, and nothing written to disk. Inside the lane body a stage's retired
// `$pr` becomes the lane item (`$pr.stdout`): `item_var` is installed as a SCOPED STEP
// RESULT, and `${pr}` reads ARGS ONLY (refs.go: "`${…}` reads ARGS ONLY and never a step
// result"), so the arg spelling would silently read the wrong thing per lane.
//
// The arm is emitted only where the TARGET GRAMMAR can express it: `workflowkit.
// ValidatePipeline` validates a loop body's sub-step references against the pipeline's
// TOP-LEVEL step ids, so a lane body whose stages reference each other is refused at load
// (`reference $X.* names a step that is not declared earlier`) even though the engine runs
// it (opencharly/sdk#441). For such a graph the knob is flattened into `config:` exactly as
// before — the one loadable home left — and the un-restored fan-out is the finding recorded
// on plugin-migrate#23, never a silent claim that the arm was emitted.
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
//     keys into one `config:` mapping;
//   - they cannot COPY an entity-level value INTO each lifted verb body — the self-containment
//     rule (a lifted verb must not depend on a pipeline config it may never be given) needs
//     every stage of a kind to receive its own copy of that knob.
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

// reshapePipelineLiftedIntent is the INTENT keyword a lifted verb step opens with. The new
// grammar's #Step arms are a CLOSED set (run/check/agent-run/agent-check/include —
// spec.StepKeywords) and every plan step must carry exactly one of them; the verb key alone is
// not an exec arm. `run` is the spelling plugin-pipeline's own bed uses for a lifted verb (its
// `gate:`/`generate:`/`emit:` steps are all `- run: <label>` + the verb key), so it is the
// producer's convention rather than a judgement about whether a given verb mutates.
const reshapePipelineLiftedIntent = "run"

// The pooled fan-out arm's emitted vocabulary (see "THE POOLED FAN-OUT ARM" above). All
// four are FROZEN snapshots of the target grammar this cutover writes into, exactly like
// the verb-kind and intent vocabularies beside them.
const (
	// reshapePipelineLaneItemVar is the `item_var` a wrapped pipeline binds each of the
	// run's PR-list values to. It is the retired model's own name for the per-lane value
	// (`$pr`), so every rewritten stage ref reads the way the authored one did.
	reshapePipelineLaneItemVar = "pr"
	// reshapePipelineLaneListArg is the run ARG the lane's collection comes from — the
	// replacement for the deleted `--prs` flag. A list value crosses the wire
	// (`#WorkflowArgVal`, spec#262) and `resolveLoopItems` parses a templated operand as
	// JSON, so this needs no seed step.
	reshapePipelineLaneListArg = "prs"
	// reshapePipelineLaneItemField is the step-result field a STRING lane item is read
	// through. `createSyntheticStepResult` puts a string item in `stdout` (and only in
	// `json` when the string HAPPENS to parse as JSON), so `.stdout` is the field that is
	// correct for every value of a `#WorkflowArgVal` list.
	reshapePipelineLaneItemField = "stdout"
	// reshapePipelineLaneID is the wrapping step's id; reshapePipelineLaneStepID moves to
	// `lane-2`, `lane-3`, … when a stage already claimed it.
	reshapePipelineLaneID = "lane"
)

// reshapePipelineLanesKnob / reshapePipelineConcurrencyKey name the retired pair the arm
// replaces. Frozen like everything else here.
const (
	reshapePipelineConcurrencyKey = "concurrency"
	reshapePipelineLanesKey       = "lanes"
)

// reshapePipelineVerbKnobs is the table of ENTITY-LEVEL (`pipeline:`-level) knobs whose value
// each lifted verb body must carry a COPY of, so the lifted verb is SELF-CONTAINED. Today
// these verb bodies read their knob off the RUN CONTEXT — the pipeline's own config — e.g.
// `rc.skills` in the agent verb, `rc.media` in the probe/media verbs, `rc.report` in the
// generate/emit verbs. After the cutover the SAME verb is an ordinary provider reachable from
// ANY plan, including a plain `charly task` plan that has no pipeline config at all, and the
// knob would simply vanish. The migrator is the one place that knows both the old entity-level
// value and the stage being lifted, so the migrator is where the duplication happens.
//
//	agent    → reads rc.skills / rc.llm / rc.repo
//	probe    → reads rc.media
//	media    → reads rc.media
//	generate → reads rc.report
//	emit     → reads rc.report
//
// `ade` and `gate` are ABSENT ON PURPOSE — that absence is a FINDING, not an oversight: no
// entity-level knob has a reader in either body (the ade verb contains zero `rc.` reads; the
// gate verb reads only `condition`). Copying one would fabricate a key the verb never
// consults. `pr`, `calver`, `workdir`, `env` and `ledger` are ENGINE/run-scoped, never config
// knobs, and are never copied here.
//
// The key SPELLINGS are frozen — they must match the lifted verb defs exactly
// (`skills?: {corpus: string}`, `llm?: #LLMSpec`, `repo?: string`, `media?: #MediaSpec`,
// `report?: #ReportSpec`), because the value is copied VERBATIM under the SAME name. The list
// order below is the emitted order and is pinned by the tests, so keep it as written.
var reshapePipelineVerbKnobs = map[string][]string{
	"agent":    {"skills", "llm", "repo"},
	"probe":    {"media"},
	"media":    {"media"},
	"generate": {"report"},
	"emit":     {"report"},
}

// reshapePipelineKnobNames is the UNION of the table above, derived from it (never a second
// hand-kept list) so the collection pass in reshapePipelineBody and the table can never drift.
var reshapePipelineKnobNames = func() map[string]bool {
	names := map[string]bool{}
	for _, knobs := range reshapePipelineVerbKnobs {
		for _, name := range knobs {
			names[name] = true
		}
	}
	return names
}()

// reshapePipelineCloneNode deep-copies a node. The SAME *yaml.Node must never appear twice in
// one tree: yaml.v3 would emit it twice and any later mutation of one occurrence would alias
// into the other. Comments (Head/Line/Foot) are deliberately NOT copied — the original keeps
// them, and duplicating a comment block into every lifted verb body is noise. Anchor/Alias are
// deliberately dropped too: re-emitting one anchor name at several sites would define it more
// than once, which is worse than losing it. Nil-safe.
func reshapePipelineCloneNode(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	c := &yaml.Node{Kind: n.Kind, Style: n.Style, Tag: n.Tag, Value: n.Value}
	if len(n.Content) > 0 {
		c.Content = make([]*yaml.Node, len(n.Content))
		for i, child := range n.Content {
			c.Content[i] = reshapePipelineCloneNode(child)
		}
	}
	return c
}

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

	// 1b. Collect the ENTITY-LEVEL knob values the lifted verb bodies must carry a copy of
	//     (reshapePipelineVerbKnobs). This MUST run before anything mutates `b`: the config
	//     consolidation below rebuilds b.Content, which would otherwise move these nodes out
	//     from under the lookup. Only the POINTER is kept here; reshapePipelineVerbStage
	//     clones at the use site, so the original (still routed into `config:`) is untouched.
	knobs := map[string]*yaml.Node{}
	for i := 0; i+1 < len(b.Content); i += 2 {
		k, v := b.Content[i], b.Content[i+1]
		if !reshapePipelineKnobNames[k.Value] {
			continue
		}
		if _, seen := knobs[k.Value]; !seen {
			knobs[k.Value] = v
		}
	}

	// 1c. The retired `concurrency: {lanes: N}` knob: the pool width, and whether THIS stage
	//     graph can be expressed as one lane body. Both answers must be taken before the body
	//     is rebuilt, and the chaining answer is measured on the OLD grammar (where a stage
	//     ref is `@<id>.<out>` — unambiguous, unlike the knob refs `$report.<knob>`, which
	//     name a key of an entity-level block and are never rewritten).
	lanesWidth, fanOut := reshapePipelineLanesPlan(name, b, stagesSeq, ids)

	// 2. Reshape every stage element (the key is renamed to `steps:` below).
	for i, st := range stagesSeq.Content {
		stagesSeq.Content[i] = reshapePipelineStage(name, st, knobs)
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
		case k.Value == reshapePipelineConcurrencyKey:
			if v.Kind != yaml.MappingNode {
				panic(fmt.Sprintf("reshape-pipeline-lobster: pipeline %q: `concurrency:` must be a mapping", name))
			}
			for j := 0; j+1 < len(v.Content); j += 2 {
				ck, cv := v.Content[j], v.Content[j+1]
				if ck.Value == reshapePipelineLanesKey && fanOut {
					continue // the lane arm carries the width; a leftover copy would be inert
				}
				config.Content = append(config.Content, ck, cv)
				haveConfig = true
			}
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

	// 4b. Wrap the migrated steps in the pooled fan-out arm. This runs AFTER the ref rewrite
	//     (so the item rebind sees the `$…` forms the rewrite just produced) and BEFORE the
	//     args pass (so a `$pr.<field>` left in the lane body is not mistaken for a `${pr}`
	//     arg reference). See "THE POOLED FAN-OUT ARM" in the file header.
	if fanOut {
		reshapePipelineWrapLanes(name, b, stepsVal, lanesWidth, ids)
		reshapePipelineEnsureArg(b, reshapePipelineLaneListArg,
			"the pr values this fan-out run is invoked with, as a JSON list (--args-json)")
	}

	// 5. `$pr` referenced anywhere in the body ⇒ the new schema requires it DECLARED.
	//    `calver`/`workdir` are engine-provided and are never declared here.
	if reshapePipelineUsesRef(b, "${pr}") {
		// The description names the surface that ACTUALLY binds the arg today. The retired
		// executor's per-lane `--prs` binder was deleted with it (plugin-pipeline#32/#43) and
		// has no successor yet, so documenting it here would be a claim about a flag no binary
		// has: an arg reaches a run as a `--args-json` member and is exposed to the steps as an
		// env/ref value (plugin-lobster engine.go mergeEnv).
		reshapePipelineEnsureArg(b, "pr", "the pr value this run is invoked with (--args-json)")
	}
	return true
}

// reshapePipelineStage rewrites ONE legacy stage mapping into its new-grammar step mapping.
// `knobs` carries the entity-level values a lifted verb body must duplicate; the command
// path ignores them (a `run:` step has no verb input).
func reshapePipelineStage(name string, st *yaml.Node, knobs map[string]*yaml.Node) *yaml.Node {
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
		return reshapePipelineVerbStage(name, st, kindNode.Value, knobs)
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
// The entity-level knobs this verb reads (reshapePipelineVerbKnobs) are then copied IN, so the
// lifted verb is self-contained; a knob the stage already declared wins and is never clobbered.
func reshapePipelineVerbStage(name string, st *yaml.Node, kind string, knobs map[string]*yaml.Node) *yaml.Node {
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
	// Duplicate the entity-level knobs this verb reads off the run context (see
	// reshapePipelineVerbKnobs), so the lifted verb stays self-contained on a plan that has no
	// pipeline config. The guard is the whole precedence rule: a value the stage ALREADY
	// declared was emitted into verb.Content by the loop above, so it always WINS and is never
	// clobbered — the entity value only fills a gap. Cloned (never the same pointer twice):
	// the original node is still routed into `config:`.
	for _, knob := range reshapePipelineVerbKnobs[kind] {
		if reshapeMapValue(verb, knob) != nil {
			continue
		}
		val, ok := knobs[knob]
		if !ok || val == nil {
			continue
		}
		verb.Content = append(verb.Content, reshapePipelineScalarKey(knob), reshapePipelineCloneNode(val))
	}
	// A LIFTED VERB IS NOT ITS OWN EXEC ARM. The new plan grammar requires every step to open
	// with one INTENT keyword (spec.StepKeywords: run/check/agent-run/agent-check/include) AND
	// to carry the verb key beside it; a `plan: [{agent: {…}}]` element is rejected by the
	// loader with `plan[0] has no intent keyword`, which is what this reshape used to emit into
	// every migrated config (plugin-migrate#20). plugin-pipeline's own bed states the rule and
	// spells it `- run: <label>` + `gate: {…}`, so `run` is the producer's own convention for a
	// lifted verb rather than a per-kind semantic choice: the verb body IS the exec arm, and the
	// intent keyword is the label that carries it.
	//
	// The label is the stage's OWN `id:` — the identifier the author already wrote — so nothing
	// here invents prose; `id:` is optional on these stages, so the verb word is the fallback.
	label := kind
	if idVal != nil && idVal.Kind == yaml.ScalarNode && idVal.Value != "" {
		label = idVal.Value
	}
	planElem := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
		reshapePipelineScalarKey(reshapePipelineLiftedIntent), reshapePipelineScalarValue(label),
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

// reshapePipelineEnsureArg adds `args.<name>: {description: …}` if the body does not already
// declare it. `args:` is inserted before `steps:` when absent.
//
// The entry carries ONLY `description:` because that is all `#TaskParamSpec` admits
// (description/default/required — spec/schema/task.cue): an emitted `type: string` is an
// unknown field against a CLOSED def and fails the load with `args.pr.type: field not allowed`
// (plugin-migrate#20). The value is a string by construction — a pipeline arg is lowered to
// `-p NAME="$NAME"` on the engine's command line — so there is no type left to write down.
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

// ---------------------------------------------------------------------------
// the pooled fan-out arm (see "THE POOLED FAN-OUT ARM" in the file header)
// ---------------------------------------------------------------------------

// reshapePipelineLanesPlan reads the retired `concurrency: {lanes: N}` knob off a pipeline
// body and decides whether THIS stage graph can be expressed as one lane body. It returns
// the pool width (0 when the knob is absent, i.e. "no fan-out") and whether to emit the arm.
//
// The width is faithful to the deleted `runBatch`, which clamped `lanes < 1` to 1 rather
// than refusing: the new arm's own gate REQUIRES `batch_size >= 1`, so an authored 0 has to
// arrive as 1 or the migrated document would not load for a knob the executor accepted.
//
// The arm is emitted only when `workflowkit.ValidatePipeline` accepts the resulting lane
// body. It validates a loop body's sub-step references against the pipeline's TOP-LEVEL step
// ids, so a body whose stages read each other is refused at load even though the engine runs
// it (opencharly/sdk#441). In that case the knob stays where the one loadable home for it is
// — flattened into `config:` — and plugin-migrate#23 records the un-restored fan-out.
func reshapePipelineLanesPlan(name string, b, stagesSeq *yaml.Node, ids map[string]bool) (int, bool) {
	width := reshapePipelineLanesWidth(name, b)
	if width == 0 {
		return 0, false
	}
	if chained := reshapePipelineChainedStageRef(stagesSeq, ids); chained != "" {
		return width, false
	}
	return width, true
}

// reshapePipelineLanesWidth reads `concurrency.lanes` as a pool width, or 0 when the body
// carries no `concurrency:` at all.
func reshapePipelineLanesWidth(name string, b *yaml.Node) int {
	conc := reshapeMapValue(b, reshapePipelineConcurrencyKey)
	if conc == nil {
		return 0
	}
	if conc.Kind != yaml.MappingNode {
		panic(fmt.Sprintf("reshape-pipeline-lobster: pipeline %q: `concurrency:` must be a mapping", name))
	}
	lanesNode := reshapeMapValue(conc, reshapePipelineLanesKey)
	if lanesNode == nil {
		return 0
	}
	if lanesNode.Kind != yaml.ScalarNode {
		panic(fmt.Sprintf("reshape-pipeline-lobster: pipeline %q: `concurrency.lanes:` must be a scalar integer", name))
	}
	n, err := strconv.Atoi(strings.TrimSpace(lanesNode.Value))
	if err != nil {
		panic(fmt.Sprintf("reshape-pipeline-lobster: pipeline %q: `concurrency.lanes: %s` is not an integer", name, lanesNode.Value))
	}
	if n < 1 {
		return 1
	}
	return n
}

// reshapePipelineChainedStageRef reports whether any stage of the entity references ANOTHER
// stage (`@<id>.<out>`), returning that stage id, or "" when the stages are independent.
//
// Measured BEFORE the ref rewrite, where a stage reference is unambiguously the `@` form:
// the `$` form that also looks like `$<id>.<path>` — `$report.bed_template` — is a KNOB
// reference naming a key of an entity-level block, never a stage, and rewriting or
// classing it as a stage ref is the corruption TestPipelineLobsterDoesNotCorruptReportRef
// exists to prevent.
func reshapePipelineChainedStageRef(n *yaml.Node, ids map[string]bool) string {
	if n == nil {
		return ""
	}
	if n.Kind == yaml.ScalarNode {
		if n.Tag != "" && n.Tag != "!!str" {
			return ""
		}
		for _, m := range reshapePipelineOldStageRefRe.FindAllStringSubmatch(n.Value, -1) {
			if ids[m[1]] {
				return m[1]
			}
		}
		return ""
	}
	for _, c := range n.Content {
		if ref := reshapePipelineChainedStageRef(c, ids); ref != "" {
			return ref
		}
	}
	return ""
}

// reshapePipelineWrapLanes replaces the body's `steps:` value with ONE lane step carrying
// the migrated steps as its body, and rebinds the body's retired `$pr` reads to that lane's
// item. `ids` is the stage-id set the item name must not collide with.
func reshapePipelineWrapLanes(name string, b, stagesSeq *yaml.Node, width int, ids map[string]bool) {
	itemVar := reshapePipelineFreeID(ids, reshapePipelineLaneItemVar)
	laneID := reshapePipelineFreeID(ids, reshapePipelineLaneID)
	// The per-lane value is a SCOPED STEP RESULT, and `${…}` reads ARGS ONLY (refs.go), so a
	// stage inside the lane must read the item through the ref form. `.stdout` is the field a
	// string item lands in (createSyntheticStepResult), which is what a `#WorkflowArgVal` list
	// holds.
	reshapePipelineRebindItemRefs(stagesSeq, itemVar)
	laneStep := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
		reshapePipelineScalarKey("id"), reshapePipelineScalarValue(laneID),
		reshapePipelineScalarKey(reshapePipelineLanesKey),
		reshapePipelineScalarValue("${" + reshapePipelineLaneListArg + "}"),
		reshapePipelineScalarKey("batch_size"), reshapePipelineIntValue(width),
		reshapePipelineScalarKey("item_var"), reshapePipelineScalarValue(itemVar),
		reshapePipelineScalarKey("steps"), stagesSeq,
	}}
	wrapped := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{laneStep}}
	for i := 0; i+1 < len(b.Content); i += 2 {
		if b.Content[i].Value == "steps" {
			b.Content[i+1] = wrapped
			return
		}
	}
	panic(fmt.Sprintf("reshape-pipeline-lobster: pipeline %q: no `steps:` key to wrap", name))
}

// reshapePipelineRebindItemRefs rewrites every `${pr}` inside a lane body to the lane's own
// item ref (`$<itemVar>.stdout`). `${prs}` is untouched: the replacement is the exact braced
// token, so the list arg the arm reads cannot be rewritten into an item read.
func reshapePipelineRebindItemRefs(n *yaml.Node, itemVar string) {
	if n == nil {
		return
	}
	if n.Kind == yaml.ScalarNode {
		if n.Tag == "" || n.Tag == "!!str" {
			n.Value = strings.ReplaceAll(n.Value,
				"${"+reshapePipelineLaneItemVar+"}",
				"$"+itemVar+"."+reshapePipelineLaneItemField)
		}
		return
	}
	for _, c := range n.Content {
		reshapePipelineRebindItemRefs(c, itemVar)
	}
}

// reshapePipelineFreeID returns `want`, or `want-2`, `want-3`, … when a stage already
// claimed it — step ids are unique across a pipeline (ValidatePipeline), so a generated id
// must not collide with an authored one.
func reshapePipelineFreeID(taken map[string]bool, want string) string {
	if !taken[want] {
		return want
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", want, i)
		if !taken[candidate] {
			return candidate
		}
	}
}

// reshapePipelineIntValue builds a plain `!!int` node (a width must not be emitted as a
// string: `batch_size` is `int & >0`).
func reshapePipelineIntValue(v int) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(v)}
}
