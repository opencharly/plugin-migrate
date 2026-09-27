package migrate

import (
	"strings"
	"testing"
)

// reshape_compact_test.go — idempotency of the compact-node-form reshaper. The
// engine runs every migration step unconditionally (no version gate), so the
// reshaper is replayed against documents already in the compact grammar. The
// regression this file pins is the panic:
//
//	compact-node-form migration: entity "check-k8s-deploy":
//	  two discriminators "vm" and "check-k8s-deploy-workload"
//
// — a compact entity whose second child is a member sub-entity whose substrate
// discriminator (`kubernetes`) is a POST-compaction spelling the frozen
// pre-cutover snapshot does not know, so reshapeClassify falls through to "disc".

// currentCheckK8sDeploy is the real post-cutover shape of the check-k8s-deploy
// bed: the primary `vm:` body already carries folded data + `plan:`, and the
// workload is a sibling member whose discriminator is `kubernetes`.
const currentCheckK8sDeploy = `check-k8s-deploy:
    vm:
        from: k3s-vm
        add_candy:
            - k3s-server
        env:
            K3S_SERVER_HOSTNAME: 127.0.0.1
        install_opts:
            with_service: true
        plan:
            - check: the k3s control plane reports a Ready node
              id: kd-wait-nodes
              kube:
                method: wait-nodes
                cluster: check-k8s-deploy-cluster-ctx
              timeout: "300s"
              stdout: {contains: "Ready"}
              context: [runtime]
    check-k8s-deploy-workload:
        kubernetes:
            image: check-k8s-deploy-app
            from: check-k8s-deploy-cluster-ctx
            deploy:
                helm_charts:
                    - repo: https://example.invalid/charts
                      chart: c
                      release: r
                      namespace: web
`

// TestCompactNodeForm_AlreadyCompactNoOp: re-running the reshaper on the current
// shape must report no change and must NOT panic on the member's `kubernetes`
// discriminator.
func TestCompactNodeForm_AlreadyCompactNoOp(t *testing.T) {
	m := migration{Name: "t", Apply: "compactNodeForm"}
	out, changed := applyTransform(t, m, currentCheckK8sDeploy)
	if changed {
		t.Fatalf("already-compact entity reported changed=true:\n%s", out)
	}
	for _, want := range []string{"vm:", "kubernetes:", "check-k8s-deploy-workload:"} {
		if !strings.Contains(out, want) {
			t.Errorf("compact input was altered — missing %q:\n%s", want, out)
		}
	}
}

// TestCompactNodeForm_CompactCandyNoOp: a compact candy whose kind body already
// carries inline data (`distro`/`candy`) and a `plan:` is a no-op too.
func TestCompactNodeForm_CompactCandyNoOp(t *testing.T) {
	in := `check-k8s-deploy-app:
    candy:
        base: quay.io/fedora/fedora:43
        description: d
        distro:
            - fedora:43
        candy:
            - '@github.com/opencharly/pod-check-keepalive:v1'
        plan:
            - check: a marker exists
              file: /x
`
	if _, changed := applyTransform(t, migration{Name: "t", Apply: "compactNodeForm"}, in); changed {
		t.Error("already-compact candy reported changed=true")
	}
}

// TestCompactNodeForm_PreCutoverShapeStillFolds: the guard must not over-skip —
// the genuine pre-compaction group shape (named data/step child wrappers) is
// still folded, and a second pass is then a no-op.
func TestCompactNodeForm_PreCutoverShapeStillFolds(t *testing.T) {
	in := `check-k8s-deploy:
    group:
        disposable: true
    check-k8s-deploy-cluster:
        vm:
            from: k3s-vm
        check-k8s-deploy-cluster-add_candy:
            add_candy:
                - k3s-server
        check-k8s-deploy-cluster-env:
            env:
                - K3S_SERVER_HOSTNAME=127.0.0.1
        kd-wait-nodes:
            check: the k3s control plane reports a Ready node
            id: kd-wait-nodes
            kube: wait-nodes
            cluster: ctx
            context: [runtime]
    check-k8s-deploy-workload:
        k8s:
            image: check-k8s-deploy-app
            from: ctx
`
	m := migration{Name: "t", Apply: "compactNodeForm"}
	out, changed := applyTransform(t, m, in)
	if !changed {
		t.Fatal("pre-cutover shape reported no change")
	}
	if strings.Contains(out, "check-k8s-deploy-cluster-add_candy:") {
		t.Errorf("the data child wrapper was not folded:\n%s", out)
	}
	if !strings.Contains(out, "add_candy:") {
		t.Errorf("the folded add_candy data key is missing:\n%s", out)
	}
	if !strings.Contains(out, "plan:") {
		t.Errorf("the step children were not folded into plan::\n%s", out)
	}
	// The member's substrate discriminator survives compaction.
	if !strings.Contains(out, "k8s:") {
		t.Errorf("member discriminator lost:\n%s", out)
	}
	// Second pass — now compact — is a no-op.
	if _, changed2 := applyTransform(t, m, out); changed2 {
		t.Errorf("second pass on the compacted doc changed it:\n%s", out)
	}
}
