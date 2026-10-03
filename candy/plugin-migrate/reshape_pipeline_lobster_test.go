package migrate

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
//   - the two HARD-ERROR paths (unknown `kind:`, `kind: command` with an extra field)
//   - idempotency (a second apply reports NO change)
const pipelineLobsterFixture = `demo-pipeline:
    pipeline:
        repo: acme/widget
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
	if c := strings.Count(out, "@github.com"); c != 2 {
		t.Errorf("expected 2 @github.com refs preserved, got %d:\n%s", c, out)
	}
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

// TestMigrationTable_PipelineLobster: pipeline-lobster-syntax is the LAST (newest) table
// entry — a project-only apply: goHook (a pipeline body is never authored on the per-host
// deploy overlay, so no touches_host).
func TestMigrationTable_PipelineLobster(t *testing.T) {
	m := migrationTable[len(migrationTable)-1]
	if m.Name != "pipeline-lobster-syntax" || m.Apply != "reshapePipelineLobster" || m.TouchesHost {
		t.Errorf("unexpected last table entry: %+v", m)
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
	if c := strings.Count(out, "@github.com/opencharly/"); c != 3 {
		t.Errorf("expected 3 @github.com/opencharly candy refs preserved, got %d", c)
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
