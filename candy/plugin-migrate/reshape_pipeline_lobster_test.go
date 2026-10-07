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
		"- agent:",
		"- probe:",
		"- emit:",
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
		"the value bound per lane by --prs",
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
		"@oracle", "@gate", "@report", "$env.", "$pr", "$calver",
	} {
		if strings.Contains(dataText, gone) {
			t.Errorf("legacy token %q remains on a data line", gone)
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
			if !ok || len(elem) != 1 {
				t.Errorf("%s: step %v: plan element must carry exactly one verb, got %v", name, step["id"], plan[0])
				continue
			}
			for kind, rawBody := range elem {
				body, ok := rawBody.(map[string]any)
				if !ok {
					t.Errorf("%s: step %v: verb %q body is not a mapping", name, step["id"], kind)
					continue
				}
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
		for kind, rawBody := range plan[0].(map[string]any) {
			body, _ := rawBody.(map[string]any)
			return kind, body
		}
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
			for kind := range plan[0].(map[string]any) {
				seen[kind] = true
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
	for _, want := range []string{"steps:", "description: Migrated pipeline demo-pipeline.", "lanes: 4", "${pr}", "args:"} {
		if !strings.Contains(s, want) {
			t.Errorf("expected %q after the full chain:\n%s", want, s)
		}
	}
}
