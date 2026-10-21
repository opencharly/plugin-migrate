package migrate

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// pipelineGithubRefRe extracts a whole `@github.com/<owner>/<repo>/<path>:<tag>` candy ref.
var pipelineGithubRefRe = regexp.MustCompile(`@github\.com/[^ '"` + "`" + `\n]+`)

// pipelineDistinctGithubRefs returns the SET of distinct github candy refs in a document.
// A count assertion is the wrong shape here: the migrator now DUPLICATES entity-level knob
// values into lifted verb bodies, so a `report:` block carrying a github ref legitimately
// appears several times in the output. What must hold is that the SET is unchanged and that
// no ref was rewritten into a stage ref (`$github…`) — never that a total is a fixed number.
func pipelineDistinctGithubRefs(s string) map[string]bool {
	refs := map[string]bool{}
	for _, m := range pipelineGithubRefRe.FindAllString(s, -1) {
		refs[m] = true
	}
	return refs
}

// pipelineAssertGithubRefsPreserved asserts the migrated document preserves every distinct
// github candy ref from src byte-identically, and invented none.
func pipelineAssertGithubRefsPreserved(t *testing.T, src, out string) {
	t.Helper()
	want := pipelineDistinctGithubRefs(src)
	got := pipelineDistinctGithubRefs(out)
	for ref := range want {
		if !got[ref] {
			t.Errorf("github candy ref %q was lost or rewritten by the migration", ref)
		}
	}
	for ref := range got {
		if !want[ref] {
			t.Errorf("migration invented a github candy ref %q", ref)
		}
	}
	if strings.Contains(out, "$github") {
		t.Error("a github candy ref was rewritten into a stage ref ($github…)")
	}
}

// reshape_pipeline_lobster_test.go — the legacy `kind: pipeline` grammar → lobster-syntax
// cutover. The compact fixture below exercises EVERY mapping row and every hazard:
//
//   - `stages:` → `steps:`; the required `description:` added
//   - `kind: command` → `run:` (with `$pr` rewritten to `${pr}` while the SHELL vars
//     `$latest` / `$b` survive untouched), `expect_exit:` dropped, `skip_when:` →
//     inverted `when:`
//   - `kind: <verb>` → `plan: [{<verb>: {…}}]`, carrying every other field VERBATIM
//     (`redo` with `max`/`escalate_after`/`triggers`)
//   - `skip_when` inversion, in BOTH the single-ref form and the `&&` form
//   - `concurrency: {lanes: N}` → `config: {lanes: N}` (flattened)
//   - `report:`/`media:` knobs routed under `config:` with their refs rewritten while
//     `@github.com/…` candy refs stay BYTE-IDENTICAL, and the `${tests:bullets}` knob
//     marker left alone
//   - `$report.bed_template` survives even though `report` IS a declared stage id (the
//     hazard a naive `[@$]<id>\.` regex would corrupt)
//   - `args:` auto-declared for `pr`
//   - the entity-level knobs DUPLICATED into each lifted verb body, so the verb is
//     self-contained on a plan with no pipeline config (`skills`/`llm`/`repo` for agent,
//     `media` for probe/media, `report` for generate/emit; `ade`/`gate` get NONE — they have
//     no reader), with a stage-local value always winning over the entity value
//   - the two HARD-ERROR paths (unknown `kind:`, `kind: command` with an extra field)
//   - idempotency (a second apply reports NO change)
const pipelineLobsterFixture = `demo-pipeline:
    pipeline:
        repo: acme/widget
        skills:
            corpus: eval-skills
        llm:
            model: eval-model
        concurrency: {lanes: 16}
        media:
            dir: "eval/pr-$pr/media"
            min: {png: 10}
        report:
            template: |-
                # Report for $pr (bed-$pr-vm @ $calver)
                ${tests:bullets}
                @github.com/acme/candy:v1
            bed_template: |-
                bed-$pr:
                  add_candy:
                    - '@github.com/opencharly/plugin-record/candy/plugin-record:v2026.272.0412'
                  gate:
                    run: ${CHARLY_BIN} check run x-$pr
        stages:
            - id: build
              kind: command
              command: 'run $pr && echo $latest $b'
              expect_exit: 0
              skip_when: '$env.CI != true'
            - id: oracle
              kind: agent
              max_turns: 5
              redo:
                max: 4
                escalate_after: 5
                triggers: {redo-plan: oracle}
              skip_when: "@oracle.class == skip"
            - id: render
              kind: generate
              template: "$report.bed_template"
              vars:
                golden: "@oracle.golden"
              out: "out/pr-$pr.yml"
            - id: gate
              kind: probe
              verbs: [ledger_gate]
              input: {bed_name: "bed-$pr"}
            - id: grade
              kind: agent
              skip_when: "@gate.ok != PASS && @gate.ok != FAIL"
            - id: report
              kind: emit
              schema: eval-report
              value: {pr: "$pr", cls: "@oracle.golden"}
              out: "$report.bed_template"
            - id: capture
              kind: media
              files: [cast, png]
              transcode: mjpeg:mp4
            - id: audit
              kind: ade
              validate: |
                the chart is populated
            - id: publish
              kind: gate
              condition: "$env.PUBLISH == approve"
`

// pipelinePlanIntents is the FROZEN TARGET intent vocabulary a migrated plan element must open
// with (spec.StepKeywords, recorded here rather than read from the live registry for the reason
// the reshape's vocabulary note gives). A migrated element is
// `{<intent>: <label>, <verb>: {…}}`: the intent keyword is the step's exec arm, the verb key
// carries the lifted body beside it.
var pipelinePlanIntents = map[string]bool{
	"run": true, "check": true, "agent-run": true, "agent-check": true, "include": true,
}

// pipelinePlanElement is one migrated `plan:` element, split ONCE for every reader in this
// file: the intent count (the loader demands exactly one), the single verb key that carries the
// lifted body, and every non-intent key it found (so a diagnostic can name them).
type pipelinePlanElement struct {
	intents  int
	verbKind string
	verbBody map[string]any
	verbKeys []string
}

// pipelinePlanVerb splits a migrated plan element `{<intent>: <label>, <verb>: {…}}`. It is the
// ONE splitter: the knob walker, the verb-body lookup, the per-kind coverage guard and the
// load-gate control all read through it, so none of them can disagree about what an element is.
func pipelinePlanVerb(elem map[string]any) pipelinePlanElement {
	var pe pipelinePlanElement
	for k, v := range elem {
		if pipelinePlanIntents[k] {
			pe.intents++
			continue
		}
		pe.verbKeys = append(pe.verbKeys, k)
		if len(pe.verbKeys) == 1 {
			pe.verbKind, pe.verbBody = k, nil
			pe.verbBody, _ = v.(map[string]any)
		}
	}
	return pe
}

// wellFormed reports whether the element carries exactly one intent keyword and exactly one verb
// key — the shape the loader accepts.
func (pe pipelinePlanElement) wellFormed() bool {
	return pe.intents == 1 && len(pe.verbKeys) == 1
}

// pipelineLobsterMigrate runs the reshape over a fixture and returns the migrated document.
func pipelineLobsterMigrate(t *testing.T, in string) string {
	t.Helper()
	out, changed := applyTransform(t, migration{Name: "t", Apply: "reshapePipelineLobster"}, in)
	if !changed {
		t.Fatal("reshapePipelineLobster reported no change on a legacy pipeline")
	}
	return out
}

// TestPipelineLobsterDoesNotCorruptReportRef — THE regression: `$report.bed_template`
// references the entity's `report:` block, and `report` IS a declared stage id in this
// fixture. A naive ref regex `[@$]([A-Za-z0-9_-]+)\.` would rewrite it to
// `$report.json.bed_template` and corrupt the generate stage. The rewrite must match ONLY
// the old `@id.out` form.
func TestPipelineLobsterDoesNotCorruptReportRef(t *testing.T) {
	out := pipelineLobsterMigrate(t, pipelineLobsterFixture)
	if !strings.Contains(out, "$report.bed_template") {
		t.Errorf("$report.bed_template must survive (report is a stage id, but $report. is not a stage ref):\n%s", out)
	}
	if strings.Contains(out, "$report.json") {
		t.Errorf("$report.bed_template was corrupted into a stage ref:\n%s", out)
	}
}

// TestPipelineLobsterGithubRefsByteIdentical — an `@github.com/...` candy ref is not a
// stage ref; it must be byte-for-byte preserved, never rewritten.
func TestPipelineLobsterGithubRefsByteIdentical(t *testing.T) {
	out := pipelineLobsterMigrate(t, pipelineLobsterFixture)
	want := "@github.com/opencharly/plugin-record/candy/plugin-record:v2026.272.0412"
	if !strings.Contains(out, want) {
		t.Errorf("github candy ref must be byte-identical, missing %q:\n%s", want, out)
	}
	if !strings.Contains(out, "@github.com/acme/candy:v1") {
		t.Errorf("github candy ref in report.template missing:\n%s", out)
	}
	pipelineAssertGithubRefsPreserved(t, pipelineLobsterFixture, out)
}

// TestPipelineLobsterGrammarRewrite — every mapping row, asserted as properties.
func TestPipelineLobsterGrammarRewrite(t *testing.T) {
	out := pipelineLobsterMigrate(t, pipelineLobsterFixture)

	// OLD grammar gone.
	for _, gone := range []string{
		"stages:", "skip_when", "kind:", "expect_exit", "@oracle", "@gate", "$env.", "$pr", "$calver",
	} {
		if strings.Contains(out, gone) {
			t.Errorf("legacy token %q must not remain:\n%s", gone, out)
		}
	}

	// NEW grammar present.
	for _, want := range []string{
		"steps:",
		"description: Migrated pipeline demo-pipeline.",
		"config:",
		"lanes: 16",
		"repo: acme/widget",
		"run ${pr} && echo $latest $b",
		"$latest $b",
		"!(${env.CI} != true)",
		"redo:",
		"escalate_after: 5",
		"triggers:",
		"redo-plan: oracle",
		"- run: oracle",
		"- run: gate",
		"- run: report",
		"!($oracle.json.class == skip)",
		"!($gate.json.ok != PASS && $gate.json.ok != FAIL)",
		"$oracle.json.golden",
		"$oracle.json.class",
		"eval/pr-${pr}/media",
		"# Report for ${pr} (bed-${pr}-vm @ ${calver})",
		"${tests:bullets}",
		"${CHARLY_BIN}",
		"bed-${pr}",
		"args:",
		"the pr value this run is invoked with (--args-json)",
		"$report.bed_template",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected new-grammar token %q missing:\n%s", want, out)
		}
	}

	// The inverted `when` carries X's text VERBATIM (no re-parenthesizing beyond the
	// single negation) and the `&&` case is not re-parenthesized.
	if strings.Contains(out, "!((@") || strings.Contains(out, "!(($") {
		t.Errorf("when inversion must not double-parenthesize:\n%s", out)
	}
}

// TestPipelineLobsterEmittedShapeIsLoadable — the LOAD-GATE control this reshape shipped
// without, and the reason plugin-migrate#20 existed: the unit above asserts the emitted
// TEXT, so a fixture that encoded an unloadable shape as the expected shape passed. This
// test asks the question the LOADER asks, over the migrated document itself, and would have
// failed on every migrated config before the fix:
//
//   - every `plan:` element opens with EXACTLY ONE intent keyword from the closed set the
//     loader enforces (spec.StepKeywords); a lifted verb emitted as `{<verb>: {…}}` alone is
//     rejected with `plan[0] has no intent keyword`, which is a whole-document load failure;
//   - every emitted `args:` entry declares ONLY fields `#TaskParamSpec` admits
//     (description/default/required); an emitted `type:` is `field not allowed` on a CLOSED
//     def and is likewise a whole-document load failure.
//
// It deliberately does not depend on the pipeline schema or on plugin-pipeline: the intent
// vocabulary is the FROZEN TARGET shape (see the vocabulary note in the reshape), so this
// test can run in this unit's tree while still failing the moment the emitted form stops
// being loadable.
func TestPipelineLobsterEmittedShapeIsLoadable(t *testing.T) {
	// #TaskParamSpec's closed field set (spec/schema/task.cue). The INTENT vocabulary is not
	// redeclared here: pipelinePlanIntents + pipelinePlanVerb are the file's one definition.

	taskParamSpecFields := map[string]bool{"description": true, "default": true, "required": true}

	out := pipelineLobsterMigrate(t, pipelineLobsterFixture)
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("migrated output is not parseable YAML: %v\n%s", err, out)
	}

	plans, params := 0, 0
	var walk func(n *yaml.Node, path string)
	walk = func(n *yaml.Node, path string) {
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				k, v := n.Content[i], n.Content[i+1]
				switch {
				case k.Value == "plan" && v.Kind == yaml.SequenceNode:
					for idx, elemNode := range v.Content {
						plans++
						var elem map[string]any
						if err := elemNode.Decode(&elem); err != nil {
							t.Fatalf("%s.plan[%d] is not a mapping: %v", path, idx, err)
						}
						pe := pipelinePlanVerb(elem)
						if pe.intents != 1 {
							t.Errorf("%s.plan[%d] carries %d intent keywords, want exactly 1 (the loader rejects this with \"no intent keyword\") — keys %v",
								path, idx, pe.intents, pe.verbKeys)
						}
						if len(pe.verbKeys) != 1 {
							t.Errorf("%s.plan[%d] carries %d non-intent keys %v, want exactly 1 (the lifted verb)",
								path, idx, len(pe.verbKeys), pe.verbKeys)
						}
					}
				case k.Value == "args" && v.Kind == yaml.MappingNode:
					for j := 0; j+1 < len(v.Content); j += 2 {
						spec := v.Content[j+1]
						if spec.Kind != yaml.MappingNode {
							continue
						}
						params++
						for m := 0; m+1 < len(spec.Content); m += 2 {
							field := spec.Content[m].Value
							if !taskParamSpecFields[field] {
								t.Errorf("%s.args.%s declares %q, which #TaskParamSpec does not admit (description/default/required) — the loader rejects this with \"field not allowed\"",
									path, v.Content[j].Value, field)
							}
						}
					}
				}
				walk(v, path+"."+k.Value)
			}
			return
		}
		for _, c := range n.Content {
			walk(c, path)
		}
	}
	walk(&doc, "")

	if plans == 0 {
		t.Fatal("no plan: sequences found in the migrated fixture — the control asserted nothing")
	}
	if params == 0 {
		t.Fatal("no args: entries found in the migrated fixture — the #TaskParamSpec control asserted nothing")
	}
}

// TestPipelineLobsterIdempotent — a second apply on the migrated output is a NO-OP: the
// body no longer carries `stages:`, which is the structural idempotency guard.
func TestPipelineLobsterIdempotent(t *testing.T) {
	first := pipelineLobsterMigrate(t, pipelineLobsterFixture)
	_, changed := applyTransform(t, migration{Name: "t", Apply: "reshapePipelineLobster"}, first)
	if changed {
		t.Errorf("second migration reported a change — not idempotent:\n%s", first)
	}
}

// TestMigrationTable_PipelineLobster: pipeline-lobster-syntax is a project-only apply:
// goHook (a pipeline body is never authored on the per-host deploy overlay, so no
// touches_host). The entry is found BY NAME, not as the last element: the table is
// append-only and a newer cutover IS the new last entry — a positional lookup reports a
// false failure the moment one lands (it did when retire-empty-group-node was appended).
func TestMigrationTable_PipelineLobster(t *testing.T) {
	var m migration
	found := false
	for _, e := range migrationTable {
		if e.Name == "pipeline-lobster-syntax" {
			m, found = e, true
			break
		}
	}
	if !found {
		t.Fatalf("pipeline-lobster-syntax is not in the migration table")
	}
	if m.Apply != "reshapePipelineLobster" || m.TouchesHost {
		t.Errorf("unexpected pipeline-lobster-syntax table entry: %+v", m)
	}
	if _, ok := goHooks[m.Apply]; !ok {
		t.Errorf("hook %q not registered in goHooks", m.Apply)
	}
}

// applyPipelinePanic runs the transform and returns the recovered panic message.
func applyPipelinePanic(t *testing.T, in string) string {
	t.Helper()
	var msg string
	func() {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatalf("expected a hard error (panic), got none for:\n%s", in)
			}
			msg = fmt.Sprint(r)
		}()
		_, _ = applyTransform(t, migration{Name: "t", Apply: "reshapePipelineLobster"}, in)
	}()
	return msg
}

// TestPipelineLobsterUnknownKindHardError — an unrecognized `kind:` is NEVER guessed.
func TestPipelineLobsterUnknownKindHardError(t *testing.T) {
	msg := applyPipelinePanic(t, `weird:
    pipeline:
        stages:
            - id: x
              kind: mystery
              command: echo hi
`)
	if !strings.Contains(msg, "unknown `kind: mystery`") {
		t.Errorf("panic must name the unknown kind, got: %s", msg)
	}
}

// TestPipelineLobsterCommandExtraFieldHardError — a `kind: command` stage carrying a field
// a `run:` step cannot hold is a HARD ERROR naming the fix, never a silent drop.
func TestPipelineLobsterCommandExtraFieldHardError(t *testing.T) {
	msg := applyPipelinePanic(t, `weird:
    pipeline:
        stages:
            - id: x
              kind: command
              command: echo hi
              redo: {max: 2}
`)
	if !strings.Contains(msg, "`redo:`") || !strings.Contains(msg, "plan: [{command:") {
		t.Errorf("panic must name the field and the fix, got: %s", msg)
	}
}

// TestPipelineLobsterMissingKindHardError — a stage with no `kind:` cannot be inferred.
func TestPipelineLobsterMissingKindHardError(t *testing.T) {
	msg := applyPipelinePanic(t, `weird:
    pipeline:
        stages:
            - id: x
              command: echo hi
`)
	if !strings.Contains(msg, "no `kind:`") {
		t.Errorf("panic must name the missing kind, got: %s", msg)
	}
}

// pipelineDataLines drops full-line YAML comments before a property grep. Author prose is
// preserved VERBATIM by the migration (every hook in this engine is comment-preserving), so
// a comment may legitimately still spell `$pr` / `@stage`; the grammar properties hold over
// DATA lines. (Block-scalar lines that start with `#` are prose too and are dropped the
// same way — they never carry a legacy grammar token after the rewrite.)
func pipelineDataLines(s string) string {
	var b strings.Builder
	for _, ln := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "#") {
			continue
		}
		b.WriteString(ln)
		b.WriteByte('\n')
	}
	return b.String()
}

// TestPipelineLobsterRealFixture — the REAL legacy entity text copied from
// eval-omarchy/charly.yml (testdata/eval-pr-plan.yml: the matrix, suite, and eval-pr-plan
// pipelines) migrates through the FULL engine chain without panic, satisfies the same
// property set the check-migrate-local bed asserts, and is idempotent on the second run.
// This is the offline proof of the bed; the bed re-proves it live.
func TestPipelineLobsterRealFixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "eval-pr-plan.yml"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "charly.yml")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := runMigrations(&MigrateContext{Dir: dir, Out: io.Discard}, true)
	if err != nil {
		t.Fatalf("runMigrations: %v", err)
	}
	if !changed {
		t.Fatal("runMigrations reported no change on the legacy fixture")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := string(got)
	dataText := pipelineDataLines(out)

	// OLD grammar gone from every DATA line.
	for _, gone := range []string{
		"stages:", "skip_when", "kind:", "expect_exit",
		"@oracle", "@gate", "@report", "$env.",
	} {
		if strings.Contains(dataText, gone) {
			t.Errorf("legacy token %q remains on a data line", gone)
		}
	}
	// `$pr` / `$calver` need the BARE form asserted, not the substring: the new grammar
	// spells the arg `${pr}` and the lane item `$pr.stdout`, and both contain `$pr`.
	for _, tc := range []struct{ name, re string }{
		{"$pr", `\$(pr)([^A-Za-z0-9_.-]|$)`},
		{"$calver", `\$(calver)([^A-Za-z0-9_.-]|$)`},
	} {
		if regexp.MustCompile(tc.re).MatchString(dataText) {
			t.Errorf("the retired BARE scalar ref %q remains on a data line", tc.name)
		}
	}
	// NEW grammar present.
	for _, want := range []string{
		"steps:",
		"description: Migrated pipeline check-omarchy-accept-matrix.",
		"description: Migrated pipeline check-omarchy-accept-suite.",
		"description: Migrated pipeline eval-pr-plan.",
		"config:",
		"$oracle.json.class",
		"$oracle.json.golden",
		"$eval.json.verdict",
		"$cold-read.json.verdict",
		"$report.json.tests", // @report.tests — `report` IS a stage id
		"${pr}",
		"${calver}",
		"${env.PR_HEAD_SHA}",
		"eval/pr-${pr}/media",
		"${tests_body:indent}",
		"${suggestions:bullets}",
		"${checks:negate}",
		"${what}",
	} {
		if !strings.Contains(dataText, want) {
			t.Errorf("expected %q after migration", want)
		}
	}
	// The `$report.` KNOB refs (dollar form) must survive — only the `@report.tests` stage
	// ref (at form) is rewritten. A naive `[@$]<id>\.` regex would have corrupted these.
	for _, knob := range []string{"$report.bed_template", "$report.template"} {
		if !strings.Contains(dataText, knob) {
			t.Errorf("knob ref %q must survive untouched", knob)
		}
	}
	// Shell vars survive; @github candy refs are byte-identical.
	for _, shell := range []string{"$latest", "$b", "$f"} {
		if !strings.Contains(dataText, shell) {
			t.Errorf("shell var %q must survive untouched", shell)
		}
	}
	pipelineAssertGithubRefsPreserved(t, string(data), out)
	// Every lifted verb body carries a copy of the entity-level knobs its verb reads, so the
	// migrated entity stays self-contained on a plan that has no pipeline `config:`.
	if n := assertVerbKnobsSelfContained(t, out); n == 0 {
		t.Error("no verb-sugar steps found in the migrated real fixture")
	}
	// Second run is a no-op.
	changed2, err := runMigrations(&MigrateContext{Dir: dir, Out: io.Discard}, true)
	if err != nil {
		t.Fatalf("second runMigrations: %v", err)
	}
	if changed2 {
		t.Error("second run reported a change — the real fixture is not idempotent")
	}
}

// assertVerbKnobsSelfContained walks a MIGRATED document and asserts, for every verb-sugar
// step, that the verb body carries a COPY of every entity-level knob that verb reads
// (reshapePipelineVerbKnobs) whenever the entity declared it. The table IS the oracle, so the
// test and the migrator cannot drift. It returns the number of verb bodies it checked.
//
// The assertion is one-directional on purpose: a body MAY legitimately carry a key the STAGE
// itself declared even when the table has no row for that kind (the real eval-omarchy ade
// stages declare their own `repo:`), and this walk cannot tell an authored key from a copied
// one. That the no-reader kinds receive NOTHING is proved separately, on a fixture whose
// stages declare no knobs (TestPipelineLobsterAdeGateBodiesUnchanged).
func assertVerbKnobsSelfContained(t *testing.T, doc string) int {
	t.Helper()
	var root map[string]any
	if err := yaml.Unmarshal([]byte(doc), &root); err != nil {
		t.Fatalf("unmarshal migrated document: %v", err)
	}
	verbs := 0
	for name, rawEntity := range root {
		entity, ok := rawEntity.(map[string]any)
		if !ok {
			continue
		}
		pl, ok := entity["pipeline"].(map[string]any)
		if !ok {
			continue
		}
		// The entity's knob values now live under the consolidated `config:` map.
		declared := map[string]bool{}
		if cfg, ok := pl["config"].(map[string]any); ok {
			for knob := range reshapePipelineKnobNames {
				if _, ok := cfg[knob]; ok {
					declared[knob] = true
				}
			}
		}
		steps, _ := pl["steps"].([]any)
		for _, rawStep := range steps {
			step, ok := rawStep.(map[string]any)
			if !ok {
				continue
			}
			plan, _ := step["plan"].([]any)
			if len(plan) != 1 {
				continue // a `run:` step has no verb body
			}
			elem, ok := plan[0].(map[string]any)
			if !ok {
				t.Errorf("%s: step %v: plan element is not a mapping: %v", name, step["id"], plan[0])
				continue
			}
			pe := pipelinePlanVerb(elem)
			if !pe.wellFormed() {
				t.Errorf("%s: step %v: plan element must carry exactly one intent keyword and one verb, got %v", name, step["id"], plan[0])
				continue
			}
			kind, body := pe.verbKind, pe.verbBody
			verbs++
			for _, knob := range reshapePipelineVerbKnobs[kind] {
				if !declared[knob] {
					continue // the entity declared no value — nothing to copy
				}
				if _, ok := body[knob]; !ok {
					t.Errorf("%s: step %v (%s): verb body is missing the entity-level knob %q, so the lifted verb is NOT self-contained",
						name, step["id"], kind, knob)
				}
			}
		}
	}
	return verbs
}

// pipelineVerbBody returns the verb body of `stepID` in `entity` from a migrated document.
func pipelineVerbBody(t *testing.T, doc, entity, stepID string) (string, map[string]any) {
	t.Helper()
	var root map[string]any
	if err := yaml.Unmarshal([]byte(doc), &root); err != nil {
		t.Fatalf("unmarshal migrated document: %v", err)
	}
	ent, ok := root[entity].(map[string]any)
	if !ok {
		t.Fatalf("entity %q not found", entity)
	}
	pl, _ := ent["pipeline"].(map[string]any)
	steps, _ := pl["steps"].([]any)
	for _, rawStep := range steps {
		step, _ := rawStep.(map[string]any)
		if step["id"] != stepID {
			continue
		}
		plan, _ := step["plan"].([]any)
		if len(plan) != 1 {
			t.Fatalf("step %q is not a verb-sugar step", stepID)
		}
		elem, _ := plan[0].(map[string]any)
		pe := pipelinePlanVerb(elem)
		if !pe.wellFormed() {
			t.Fatalf("step %q: plan element is not `{<intent>: <label>, <verb>: {…}}`: %v", stepID, plan[0])
		}
		return pe.verbKind, pe.verbBody
	}
	t.Fatalf("step %q not found in entity %q", stepID, entity)
	return "", nil
}

// TestPipelineLobsterVerbKnobsDuplicated — the compact fixture exercises EVERY kind (all five
// table rows plus the two no-reader kinds), and each lifted verb body carries a copy of the
// entity-level knobs its verb reads.
func TestPipelineLobsterVerbKnobsDuplicated(t *testing.T) {
	out := pipelineLobsterMigrate(t, pipelineLobsterFixture)
	if n := assertVerbKnobsSelfContained(t, out); n == 0 {
		t.Fatal("no verb-sugar steps found")
	}
	// Coverage guard: this fixture must exercise every kind the table names, and both kinds
	// it deliberately leaves out — otherwise the test silently stops proving the table.
	seen := map[string]bool{}
	var root map[string]any
	if err := yaml.Unmarshal([]byte(out), &root); err != nil {
		t.Fatal(err)
	}
	pl := root["demo-pipeline"].(map[string]any)["pipeline"].(map[string]any)
	for _, rawStep := range pl["steps"].([]any) {
		plan, _ := rawStep.(map[string]any)["plan"].([]any)
		if len(plan) == 1 {
			if elem, ok := plan[0].(map[string]any); ok {
				if pe := pipelinePlanVerb(elem); pe.wellFormed() {
					seen[pe.verbKind] = true
				}
			}
		}
	}
	for kind := range reshapePipelineVerbKnobs {
		if !seen[kind] {
			t.Errorf("fixture does not exercise verb kind %q from the knob table", kind)
		}
	}
	for _, kind := range []string{"ade", "gate"} {
		if !seen[kind] {
			t.Errorf("fixture does not exercise the no-reader kind %q", kind)
		}
	}
}

// TestPipelineLobsterVerbKnobStageLocalWins — the precedence rule, both directions: a stage
// that ALREADY declared a knob keeps its OWN value (never clobbered by the entity's), and a
// stage that did not gets the entity's value.
func TestPipelineLobsterVerbKnobStageLocalWins(t *testing.T) {
	out := pipelineLobsterMigrate(t, `local-override:
    pipeline:
        repo: acme/widget
        media:
            dir: "entity-dir"
            min: {png: 1}
        stages:
            - id: local
              kind: probe
              verbs: [ledger_gate]
              media:
                dir: "stage-dir"
            - id: gap
              kind: probe
              verbs: [ledger_gate]
`)
	// The entity value survives untouched under `config:` (the duplication is purely additive).
	var root map[string]any
	if err := yaml.Unmarshal([]byte(out), &root); err != nil {
		t.Fatal(err)
	}
	cfg := root["local-override"].(map[string]any)["pipeline"].(map[string]any)["config"].(map[string]any)
	if dir := cfg["media"].(map[string]any)["dir"]; dir != "entity-dir" {
		t.Errorf("config.media.dir = %v, want entity-dir", dir)
	}
	// The stage-local value WINS: an appended entity copy would make the later key win instead.
	_, local := pipelineVerbBody(t, out, "local-override", "local")
	if got := local["media"].(map[string]any)["dir"]; got != "stage-dir" {
		t.Errorf("stage-local media was clobbered by the entity value: media.dir = %v, want stage-dir", got)
	}
	// The gap is filled from the entity.
	_, gap := pipelineVerbBody(t, out, "local-override", "gap")
	if got := gap["media"].(map[string]any)["dir"]; got != "entity-dir" {
		t.Errorf("the entity value did not fill the gap: media.dir = %v, want entity-dir", got)
	}
}

// TestPipelineLobsterAdeGateBodiesUnchanged — `ade` and `gate` have NO entity-level knob
// reader, so their lifted verb bodies must carry exactly their own stage fields and nothing
// added. This is the "the absence is the finding" proof, on a fixture whose ade/gate stages
// declare no knob of their own.
func TestPipelineLobsterAdeGateBodiesUnchanged(t *testing.T) {
	out := pipelineLobsterMigrate(t, pipelineLobsterFixture)
	for _, tc := range []struct {
		stepID string
		kind   string
		own    []string
	}{
		{"audit", "ade", []string{"validate"}},
		{"publish", "gate", []string{"condition"}},
	} {
		kind, body := pipelineVerbBody(t, out, "demo-pipeline", tc.stepID)
		if kind != tc.kind {
			t.Errorf("step %q: verb kind = %q, want %q", tc.stepID, kind, tc.kind)
		}
		if len(body) != len(tc.own) {
			t.Errorf("step %q (%s): body carries %d keys, want only its own %v — got %v",
				tc.stepID, tc.kind, len(body), tc.own, sortedKeys(body))
		}
		for _, k := range tc.own {
			if _, ok := body[k]; !ok {
				t.Errorf("step %q (%s): own field %q missing", tc.stepID, tc.kind, k)
			}
		}
		for _, knob := range []string{"skills", "llm", "repo", "media", "report"} {
			if _, ok := body[knob]; ok {
				t.Errorf("step %q (%s): entity-level knob %q was copied although the verb has no reader for it",
					tc.stepID, tc.kind, knob)
			}
		}
	}
}

// sortedKeys renders a mapping's keys for a failure message.
func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestPipelineLobsterRunMigrationsChain — the FULL engine chain (every table step in
// declaration order, then the universal version-stamp strip) runs without panic on a
// pipeline document and actually rewrites the file on disk. This pins the ordering hazard
// (an earlier hook, e.g. compactNodeForm, must not choke on a `pipeline:` entity).
func TestPipelineLobsterRunMigrationsChain(t *testing.T) {
	dir := t.TempDir()
	src := `discover:
    - path: candy
      recursive: true
demo-pipeline:
    pipeline:
        concurrency: {lanes: 4}
        stages:
            - id: build
              kind: command
              command: 'echo $pr'
              expect_exit: 0
`
	path := filepath.Join(dir, "charly.yml")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := runMigrations(&MigrateContext{Dir: dir, Out: io.Discard}, true)
	if err != nil {
		t.Fatalf("runMigrations: %v", err)
	}
	if !changed {
		t.Fatal("runMigrations reported no change on a legacy pipeline file")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if strings.Contains(s, "stages:") || strings.Contains(s, "kind: command") || strings.Contains(s, "expect_exit") {
		t.Errorf("legacy grammar survived the full chain:\n%s", s)
	}
	for _, want := range []string{"steps:", "description: Migrated pipeline demo-pipeline.", "batch_size: 4", "item_var: pr", "echo $pr.stdout", "args:"} {
		if !strings.Contains(s, want) {
			t.Errorf("expected %q after the full chain:\n%s", want, s)
		}
	}
}

// ---------------------------------------------------------------------------
// the pooled fan-out arm
// ---------------------------------------------------------------------------

// pipelineLanesFixture is a retired pipeline whose stages are REF-INDEPENDENT — the shape the
// fan-out arm can be emitted for. Two `kind: command` stages, no `@stage.out` between them, so
// the migrated lane body satisfies the target grammar's loop-body rule (a loop body's sub-step
// references are validated against the pipeline's TOP-LEVEL step ids only).
const pipelineLanesFixture = `lanes-pipeline:
    pipeline:
        repo: acme/widget
        concurrency: {lanes: 3}
        stages:
            - id: run-bed
              kind: command
              command: 'charly check run bed-$pr'
              expect_exit: 0
            - id: collect
              kind: command
              command: 'ls -t .check/bed-$pr | head -1'
              expect_exit: 0
`

// pipelineLaneArms and pipelineSubStepArms are the TARGET grammar's exec arms, FROZEN here
// for the reason pipelinePlanIntents is frozen (see the vocabulary note in the reshape): a
// migration unit replays against arbitrarily old configs and must not read the live schema.
// The FIRST set is the mutually exclusive arm list a step carries; the SECOND is the
// NON-RECURSIVE subset a loop body's sub-step may carry (upstream lobster's own rule: no
// nested parallel/for_each/lanes/input/approval/workflow inside a loop or branch).
var (
	pipelineLaneArms     = []string{"run", "pipeline", "workflow", "parallel", "for_each", "lanes", "input", "plan", "charly", "approval"}
	pipelineSubStepArms  = map[string]bool{"run": true, "pipeline": true, "plan": true, "charly": true}
	pipelineLoopKeys     = []string{"steps", "item_var", "index_var", "batch_size", "pause_ms"}
	pipelineStepRefWhole = regexp.MustCompile(`^\$([A-Za-z0-9_-]+)\.([A-Za-z0-9_]+(?:\.[A-Za-z0-9_]+)*)$`)
)

// pipelineLaneArmErrors is the LOAD GATE for the emitted fan-out arm: it applies the target
// grammar's own rules to the EMITTED document and returns every violation. The rules are the
// ones sdk/workflowkit.ValidatePipeline enforces for a `lanes:` step — exactly one exec arm,
// a non-empty collection, a REQUIRED positive `batch_size`, no `pause_ms`, `item_var` ≠
// `index_var`, a non-empty `steps:` body of non-recursive sub-steps, loop companions only
// beside a loop arm, and a collection that names an EARLIER top-level step — expressed as a
// frozen snapshot so this unit keeps compiling while a consumer's pinned sdk is older.
//
// It returns an error when the document carries NO lane arm at all: a reshape that goes back
// to flattening the retired knob into `config:` must fail this gate, not silently pass it.
func pipelineLaneArmErrors(t *testing.T, doc string) []string {
	t.Helper()
	var root map[string]any
	if err := yaml.Unmarshal([]byte(doc), &root); err != nil {
		t.Fatalf("emitted document is not parseable YAML: %v", err)
	}
	var errs []string
	lanes := 0
	for name, rawEntity := range root {
		entity, ok := rawEntity.(map[string]any)
		if !ok {
			continue
		}
		pl, ok := entity["pipeline"].(map[string]any)
		if !ok {
			continue
		}
		steps, _ := pl["steps"].([]any)
		declared := map[string]bool{}
		for _, rawStep := range steps {
			step, ok := rawStep.(map[string]any)
			if !ok {
				continue
			}
			id, _ := step["id"].(string)
			where := fmt.Sprintf("%s: step %q", name, id)
			var arms []string
			for _, arm := range pipelineLaneArms {
				if _, set := step[arm]; set {
					arms = append(arms, arm)
				}
			}
			collection, hasLanes := step["lanes"].(string)
			if !hasLanes {
				// companions without a loop arm are the stray-companion rule
				for _, key := range pipelineLoopKeys {
					if _, set := step[key]; set {
						errs = append(errs, fmt.Sprintf("%s: %q only applies alongside for_each/lanes", where, key))
					}
				}
				declared[id] = true
				continue
			}
			lanes++
			if len(arms) != 1 {
				errs = append(errs, fmt.Sprintf("%s: %d execution arms set (%v), want exactly one", where, len(arms), arms))
			}
			if collection == "" {
				errs = append(errs, fmt.Sprintf("%s: lanes is empty", where))
			}
			switch width := step["batch_size"].(type) {
			case int:
				if width < 1 {
					errs = append(errs, fmt.Sprintf("%s: lanes requires batch_size >= 1 (the pool width)", where))
				}
			default:
				errs = append(errs, fmt.Sprintf("%s: lanes requires batch_size >= 1 (the pool width), got %v", where, step["batch_size"]))
			}
			if _, set := step["pause_ms"]; set {
				errs = append(errs, fmt.Sprintf("%s: lanes cannot define pause_ms", where))
			}
			if iv, _ := step["item_var"].(string); iv != "" {
				if iv == fmt.Sprint(step["index_var"]) {
					errs = append(errs, fmt.Sprintf("%s: item_var and index_var must differ", where))
				}
			}
			if m := pipelineStepRefWhole.FindStringSubmatch(collection); m != nil && !declared[m[1]] {
				errs = append(errs, fmt.Sprintf("%s: reference $%s.* names a step that is not declared earlier", where, m[1]))
			}
			body, _ := step["steps"].([]any)
			if len(body) == 0 {
				errs = append(errs, fmt.Sprintf("%s: for_each/lanes requires a non-empty steps list", where))
			}
			for _, rawSub := range body {
				sub, ok := rawSub.(map[string]any)
				if !ok {
					errs = append(errs, fmt.Sprintf("%s: a sub-step is not a mapping", where))
					continue
				}
				subArms := 0
				for arm := range sub {
					if arm == "id" || arm == "when" || arm == "stdin" || arm == "env" || arm == "cwd" || arm == "timeout_ms" || arm == "on_error" || arm == "retry" {
						continue
					}
					if !pipelineSubStepArms[arm] {
						errs = append(errs, fmt.Sprintf("%s: sub-step %v carries %q, which a loop body cannot hold", where, sub["id"], arm))
					}
					subArms++
				}
				if subArms != 1 {
					errs = append(errs, fmt.Sprintf("%s: sub-step %v carries %d execution arms, want exactly one", where, sub["id"], subArms))
				}
			}
			declared[id] = true
		}
	}
	if lanes == 0 {
		errs = append(errs, "the emitted document carries NO fan-out arm")
	}
	return errs
}

// pipelineEmittedMutation applies a textual edit to an emitted document. Each control below
// must be a MUTATION of the emitted shape, so the gate is proved to measure the emission.
func pipelineEmittedMutation(t *testing.T, doc, old, new string) string {
	t.Helper()
	if n := strings.Count(doc, old); n != 1 {
		t.Fatalf("control mutation anchor %q occurs %d times in the emitted document, want exactly 1:\n%s", old, n, doc)
	}
	return strings.Replace(doc, old, new, 1)
}

// TestPipelineLobsterLanesArmEmitted — a retired `concurrency: {lanes: N}` becomes the pooled
// fan-out arm: ONE lane step wrapping the whole migrated step list, the collection taken from
// the run's `prs` arg, the width from the retired knob, and every retired `$pr` read rebound
// to the lane's own item (never the arg template, which reads ARGS ONLY).
func TestPipelineLobsterLanesArmEmitted(t *testing.T) {
	out := pipelineLobsterMigrate(t, pipelineLanesFixture)

	for _, want := range []string{
		"- id: lane",
		"lanes: ${prs}", // the collection is the run's argument list
		"batch_size: 3", // the retired knob IS the pool width
		"item_var: pr",  // the per-lane binding keeps the retired name
		"$pr.stdout",    // the lane item, read through its step-result field
		"charly check run bed-$pr.stdout",
		"args:",
		"the pr values this fan-out run is invoked with, as a JSON list (--args-json)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected the fan-out arm token %q in the emitted document:\n%s", want, out)
		}
	}
	if strings.Contains(pipelineDataLines(out), "concurrency:") {
		t.Errorf("the retired `concurrency:` key survived the migration:\n%s", out)
	}
	if strings.Contains(pipelineDataLines(out), "lanes: 3") {
		t.Errorf("the width was flattened into `config:` instead of carried by the arm:\n%s", out)
	}
	// the arm wraps ONE step list: two top-level steps would run the body twice
	var root map[string]any
	if err := yaml.Unmarshal([]byte(out), &root); err != nil {
		t.Fatal(err)
	}
	pl := root["lanes-pipeline"].(map[string]any)["pipeline"].(map[string]any)
	steps := pl["steps"].([]any)
	if len(steps) != 1 {
		t.Fatalf("the emitted pipeline carries %d top-level steps, want 1 (the lane): %v", len(steps), steps)
	}
	lane := steps[0].(map[string]any)
	if body, _ := lane["steps"].([]any); len(body) != 2 {
		t.Errorf("the lane body carries %d stages, want the 2 migrated stages", len(body))
	}
}

// TestPipelineLobsterLanesArmIsLoadable — THE load gate for the arm, with the controls that
// make it a gate rather than a pass-through:
//
//   - the EMITTED document satisfies the target grammar's fan-out rules (the frozen
//     snapshot of what sdk/workflowkit.ValidatePipeline enforces);
//   - the PRE-CHANGE emission (the retired knob flattened into `config:`, no arm) FAILS the
//     same gate — so a future reshape that stops emitting the arm fails here, not in a
//     consumer;
//   - two MUTATIONS of the emitted arm fail it — a lane step with no pool width, and a
//     collection naming a step that is not declared earlier — so the gate is measured
//     against the emission, not against "it parses".
func TestPipelineLobsterLanesArmIsLoadable(t *testing.T) {
	out := pipelineLobsterMigrate(t, pipelineLanesFixture)

	if errs := pipelineLaneArmErrors(t, out); len(errs) != 0 {
		t.Fatalf("the emitted fan-out document is NOT loadable: %v\n%s", errs, out)
	}

	// CONTROL 1 — the pre-change emission must FAIL. That shape still parses and still
	// carries the knob, which is exactly why a gate keyed on "the document loads" would be
	// vacuous; this gate is keyed on the arm being present and well-formed.
	preChange := pipelineEmittedMutation(t, out, "batch_size: 3", "")
	preChange = pipelineEmittedMutation(t, preChange, "lanes: ${prs}", "config:\n            lanes: 3")
	if errs := pipelineLaneArmErrors(t, preChange); len(errs) == 0 {
		t.Fatalf("the OLD emitted shape (knob flattened into `config:`, no arm) passed the load gate — the gate is not measuring the emission:\n%s", preChange)
	}

	// CONTROL 2 — mutations of the emitted arm must FAIL.
	for _, tc := range []struct{ name, doc, want string }{
		{"no pool width", pipelineEmittedMutation(t, out, "batch_size: 3", ""), "lanes requires batch_size >= 1"},
		{
			"collection names a later step",
			pipelineEmittedMutation(t, out, "lanes: ${prs}", "lanes: $later.json.items"),
			"reference $later.* names a step that is not declared earlier",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := pipelineLaneArmErrors(t, tc.doc)
			if len(errs) == 0 {
				t.Fatalf("the mutated arm passed the load gate — the gate is not measuring the emission")
			}
			joined := strings.Join(errs, "\n")
			if !strings.Contains(joined, tc.want) {
				t.Errorf("gate rejected the mutation for the wrong reason: %v (want %q)", errs, tc.want)
			}
		})
	}
}

// TestPipelineLobsterLanesChainedBodyStaysLoadable — a stage graph whose stages reference
// each other CANNOT be one lane body today: the target grammar validates a loop body's
// sub-step refs against the pipeline's TOP-LEVEL ids, so the wrapper would be refused at load
// (opencharly/sdk#441). The knob then keeps the one loadable home it has — the inert
// `config:` map — and the document must still LOAD, because half a migration is worse than a
// documented gap. plugin-migrate#23 records the un-restored fan-out.
func TestPipelineLobsterLanesChainedBodyStaysLoadable(t *testing.T) {
	const chained = `chained-pipeline:
    pipeline:
        concurrency: {lanes: 16}
        stages:
            - id: oracle
              kind: command
              command: 'echo $pr'
            - id: render
              kind: command
              command: 'echo @oracle.out'
`
	out := pipelineLobsterMigrate(t, chained)
	if strings.Contains(pipelineDataLines(out), "concurrency:") {
		t.Errorf("the retired `concurrency:` key survived:\n%s", out)
	}
	if !strings.Contains(out, "lanes: 16") {
		t.Errorf("a chained stage graph must keep the knob in `config:`, got:\n%s", out)
	}
	if strings.Contains(out, "item_var:") {
		t.Errorf("a chained stage graph must NOT be wrapped in a lane step (the loader refuses it):\n%s", out)
	}
	// the whole point of the fallback: it LOADS. No arm, so the arm gate reports exactly that
	// and nothing else.
	for _, err := range pipelineLaneArmErrors(t, out) {
		if !strings.Contains(err, "carries NO fan-out arm") {
			t.Errorf("the chained-body fallback is not loadable: %s", err)
		}
	}
}

// TestPipelineLobsterLanesRealMatrixWraps — the REAL check-omarchy-accept-matrix entity (two
// ref-independent command stages) is the one eval-omarchy pipeline whose fan-out translates
// mechanically; eval-pr-plan is the one that cannot (its twelve stages chain). This pins
// which half of that file the arm reaches, on the fixture copied verbatim from the consumer.
func TestPipelineLobsterLanesRealMatrixWraps(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "eval-pr-plan.yml"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "charly.yml")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runMigrations(&MigrateContext{Dir: dir, Out: io.Discard}, true); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := string(got)

	var root map[string]any
	if err := yaml.Unmarshal([]byte(out), &root); err != nil {
		t.Fatal(err)
	}
	stepsOf := func(entity string) []any {
		pl, ok := root[entity].(map[string]any)["pipeline"].(map[string]any)
		if !ok {
			t.Fatalf("entity %q missing", entity)
		}
		s, _ := pl["steps"].([]any)
		return s
	}
	matrix := stepsOf("check-omarchy-accept-matrix")
	if len(matrix) != 1 {
		t.Fatalf("check-omarchy-accept-matrix carries %d top-level steps, want 1 (the lane)", len(matrix))
	}
	lane := matrix[0].(map[string]any)
	if lane["lanes"] == nil || fmt.Sprint(lane["batch_size"]) != "16" {
		t.Fatalf("check-omarchy-accept-matrix did not wrap: lanes=%v batch_size=%v", lane["lanes"], lane["batch_size"])
	}
	plan := stepsOf("eval-pr-plan")
	if len(plan) < 2 || plan[0].(map[string]any)["lanes"] != nil {
		t.Fatalf("eval-pr-plan must NOT be wrapped (its stages chain): %d top-level steps", len(plan))
	}
}
