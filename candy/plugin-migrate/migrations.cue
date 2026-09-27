// migrations.cue — the declarative migration table: the ORDERED DATA the
// `charly migrate` engine interprets (embedded via //go:embed in engine.go).
// Each entry is validated at process start against #Migration (schema/migration.cue,
// beside this file in the plugin). Both the table DATA and the #Migration schema live
// HERE in candy/plugin-migrate, OUTSIDE the sdk schema, so neither enters the spec
// codegen / vocab concatenation — engine data + a plugin-only validation schema, not
// ingress.
//
// There is no schema version and no version stamp: the table is a plain ordered list,
// steps run in declaration order, and each step MUST be idempotent (a no-op when the
// old shape is absent). Add a future migration by appending ONE entry here. Common
// ops need zero new Go:
//
//   migrations: [
//     {name: "widget-rename",
//      ops: [{op: "rename_key", from: "widget", to: "gadget", scope: "any"}]},
//   ]
//
// A structural reshape the ops can't express sets `apply: "<hook>"` and registers
// one Go hook in goHooks. See /charly-build:migrate.
migrations: [
	{
		name:         "compact-node-form"
		touches_host: true
		apply:        "compactNodeForm"
	},
	{
		name: "strip-candy-libvirt-field"
		// candy-level libvirt: is a candy-body field, never authored on the
		// per-host deploy overlay — no touches_host needed.
		apply: "stripCandyLibvirtField"
	},
	{
		name: "strip-deploy-shell-overlay"
		// the deploy-scope shell: overlay is authorable on a per-host
		// charly.yml deploy entry too (as well as a project charly.yml) —
		// touches_host so the per-host config is swept as well.
		touches_host: true
		apply:        "stripDeployShellOverlay"
	},
	{
		name: "k8s-to-kubernetes"
		// the deploy substrate kind `k8s:` → `kubernetes:` (full naming cleanup) and the
		// inner deploy-knobs block `kubernetes:` → `deploy:` (the outer-node rename
		// collides with the inner block, so the inner block becomes `deploy:`).
		// op 1 renames every `k8s:` discriminator key (deploy nodes + cluster templates);
		// op 2 renames the inner deploy-knobs block, scoped by under_kind: "kubernetes" so
		// it only matches `kubernetes:` keys nested inside a `kubernetes:` entity, never
		// the outer node. Order matters: op 2 needs the `kubernetes` kind to scope under.
		ops: [
			{op: "rename_key", from: "k8s", to: "kubernetes", scope: "any"},
			{op: "rename_key", from: "kubernetes", to: "deploy", scope: "any", under_kind: "kubernetes"},
		]
	},
	{
		name: "remove-candy-localpkg"
		// the candy-body `localpkg:` map (the OS-tracked package install) is REMOVED —
		// replaced by the `packaging:` section (the nFPM cutover). A candy carrying the
		// old field is a hard schema violation, so migrate deletes it. Candy-body field,
		// never authored on the per-host deploy overlay — no touches_host needed.
		ops: [
			{op: "delete_key", key: "localpkg", scope: "any", under_kind: "candy"},
		]
	},
	{
		name: "install-template-to-phases"
		// the legacy top-level `#Format.install_template` / `#Builder.install_template`
		// fields (the (install, container) fallback) are REMOVED — their content
		// migrates into `format.<fmt>.phase.install.container` / the builder equivalent,
		// the phase: block's single source of truth (strict-cleanup cutover, Unit 3b).
		// The nested move can't be expressed as rename_key/move_key ops, so a Go
		// reshaper hook moves it. A project charly.yml carrying
		// the old field is a hard schema violation; the embedded build vocabulary is
		// migrated in-tree — no touches_host (the format/builder vocab is a project
		// charly.yml section, never a per-host deploy-overlay field).
		apply: "installTemplateToPhases"
	},
	{
		name: "reshape-graphics-gl"
		// vm `libvirt.devices.graphics[].gl` changes SHAPE: the bare scalar (`gl: "yes"`,
		// which could only ever reach spice's enable= attribute) becomes
		// #LibvirtGraphicsGL{enable?, render_node?}, so that rendernode= — the attribute
		// that points virtio-gpu at a specific host DRM node — is expressible at all
		// (the GPU-configuration-surface cutover).
		//
		// None of the four ops can do this: they rename keys and rewrite scalar VALUES,
		// but cannot replace a scalar node with a MAPPING node — and the field sits inside
		// a LIST element (graphics is a sequence), which under_kind scoping cannot address
		// on its own. So a Go reshaper hook does it.
		//
		// A vm-kind entity's libvirt: block is a project charly.yml / vm.yml section, never
		// a per-host deploy-overlay field — no touches_host.
		apply: "reshapeGraphicsGL"
	},
	{
		name: "record-field-to-instrument"
		// the deploy-level whole-run recording wrap `record:` field (the G5 bed-level
		// recording) is HARVESTED into the instrument: entry of the new capture model
		// (Cutover A): record_name becomes the instrument id, the #RecordWrap value
		// fields pass through into the record verb input (method: session), and the
		// runner owns the session lifecycle. A record: carrying method: is a plan-step
		// verb sugar, never converted. Deploy-node field, never a per-host deploy-overlay
		// key — no touches_host.
		apply: "recordFieldToInstrument"
	},
	{
		name: "unroll-group-deploy"
		// the targetless deploy kind `group:` (C2-group) is REMOVED from #ResourceKind
		// (spec #105, the Cutover C task 1 contract half) — the member-tree bed shape
		// makes its dual representation forbidden at R10. This step rewrites the
		// authored shape to the post-migrate spelling: the FIRST member becomes the
		// deploy primary (the entity keeps its name and gains the member's substrate
		// discriminator; the member key is consumed), the group scalars
		// (disposable/lifecycle/description/iterate) MOVE onto that primary's kind
		// body (the member's own keys win on collision), the REMAINING members stay
		// deploy-level siblings, and nested groups rewrite recursively (post-order).
		// A degenerate group (no promotable member) is left for the load-time gate to
		// name. None of the four key-transform ops can replace two sibling keys with
		// one promoted pair, so a Go reshaper hook does it. Deploy-node surface, never
		// a per-host deploy-overlay field — no touches_host.
		apply: "unrollGroupDeploy"
	},
	{
		name: "deploy-cpu-spelling"
		// the deploy node's per-deploy VM-shape override CPU field is renamed from the
		// outlier `cpus:` (plural) to `cpu:`, matching #Vm (the template it overrides)
		// and #VmVariant. The field was DEAD until this cutover's plugin-vm reader
		// landed, so nothing authored has live meaning to preserve — the rename is for
		// wire-surface correctness.
		//
		// WHY NOT the declarative `rename_key` op: scoped `under_kind: vm` it would also
		// rewrite the LIVE `security:` block's own `cpus:` (a string CPU quota — a
		// different field with a different type) nested inside the same vm body, because
		// the op-walker's under_kind matches every mapping nested WITHIN the entity, not
		// just the kind body's direct children. A short key word cannot be scoped by
		// under_kind alone; the reshaper targets the exact direct-child position (the
		// reshapeGraphicsGL precedent). Deploy-node field, authorable on the per-host
		// deploy overlay too — touches_host.
		touches_host: true
		apply:        "reshapeDeployCPU"
	},
	{
		name: "rekey-legacy-vm-overlay"
		// the per-host overlay's legacy `vm:<VmDomainIdentity>` deploy keys are a
		// REMOVED convention, and `charly migrate` is the ONE and ONLY place that
		// handles it. For each `^vm:` key: DROP it when a dotted sibling already
		// carries the same domain identity under the new spelling (no information
		// lost), else HARD-ERROR — the lossy key cannot be reversed back to a
		// deploy identity. The step operates on the overlay's top-level `deploy:`
		// mapping only; a project charly.yml carries no such mapping, so the
		// generic project-file sweep is a no-op. touches_host so the per-host
		// overlay is swept.
		touches_host: true
		apply:        "rekeyLegacyVMOverlay"
	},
]
