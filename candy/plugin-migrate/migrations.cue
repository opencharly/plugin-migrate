// version note: pinned at the CURRENT schema head (2026.248.1030) - the spec repo module
		// TAG v0.2026248.1043 is the tag-on-merge CalVer, a DIFFERENT numbering domain from
		// #SchemaVersion; the engine validates entries within [SchemaFloor, SchemaHead],
		// and this entry targets the head (the record: field dies with this migration).
		// migrations.cue — the declarative migration table: the DATA the `charly migrate`
// engine interprets (embedded via //go:embed in engine.go). Each entry is validated
// at process start against #Migration (schema/migration.cue, beside this file in the
// plugin). Both the table DATA and the #Migration schema live HERE in
// candy/plugin-migrate, OUTSIDE the sdk schema, so neither enters the spec codegen /
// vocab concatenation — engine data + a plugin-only validation schema, not ingress.
//
// At the current migration-baseline reset the table is EMPTY: no config below the
// current schema HEAD is migratable (`charly migrate` stamps a current-format
// config to HEAD and refuses anything below #SchemaFloor). Add a future migration
// by appending ONE entry here and bumping #SchemaVersion in
// sdk/schema/version.cue, then `task cue:gen`. Common ops need zero new Go:
//
//   migrations: [
//     {version: "2026.200.0800", name: "widget-rename",
//      ops: [{op: "rename_key", from: "widget", to: "gadget", scope: "any"}]},
//   ]
//
// A structural reshape the ops can't express sets `apply: "<hook>"` and registers
// one Go hook in goHooks. See /charly-build:migrate.
migrations: [
	{
		version:      "2026.186.2323"
		name:         "compact-node-form"
		touches_host: true
		apply:        "compactNodeForm"
	},
	{
		version: "2026.202.0105"
		name:    "strip-candy-libvirt-field"
		// candy-level libvirt: is a candy-body field, never authored on the
		// per-host deploy overlay — no touches_host needed.
		apply: "stripCandyLibvirtField"
	},
	{
		version: "2026.204.1223"
		name:    "strip-deploy-shell-overlay"
		// the deploy-scope shell: overlay is authorable on a per-host
		// charly.yml deploy entry too (as well as a project charly.yml) —
		// touches_host so the per-host config is swept as well.
		touches_host: true
		apply:        "stripDeployShellOverlay"
	},
	{
		version: "2026.223.1018"
		name:    "k8s-to-kubernetes"
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
		version: "2026.225.1508"
		name:    "remove-candy-localpkg"
		// the candy-body `localpkg:` map (the OS-tracked package install) is REMOVED —
		// replaced by the `packaging:` section (the nFPM cutover). A candy carrying the
		// old field is a hard schema violation, so migrate deletes it. Candy-body field,
		// never authored on the per-host deploy overlay — no touches_host needed.
		ops: [
			{op: "delete_key", key: "localpkg", scope: "any", under_kind: "candy"},
		]
	},
	{
		version: "2026.232.0520"
		name:    "install-template-to-phases"
		// the legacy top-level `#Format.install_template` / `#Builder.install_template`
		// fields (the (install, container) fallback) are REMOVED — their content
		// migrates into `format.<fmt>.phase.install.container` / the builder equivalent,
		// the phase: block's single source of truth (strict-cleanup cutover, Unit 3b).
		// The nested move can't be expressed as rename_key/move_key ops (spec-side
		// version.cue), so a Go reshaper hook moves it. A project charly.yml carrying
		// the old field is a hard schema violation; the embedded build vocabulary is
		// migrated in-tree — no touches_host (the format/builder vocab is a project
		// charly.yml section, never a per-host deploy-overlay field).
		apply: "installTemplateToPhases"
	},
	{
		version: "2026.240.1943"
		name:    "reshape-graphics-gl"
		// vm `libvirt.devices.graphics[].gl` changes SHAPE: the bare scalar (`gl: "yes"`,
		// which could only ever reach spice's enable= attribute) becomes
		// #LibvirtGraphicsGL{enable?, render_node?}, so that rendernode= — the attribute
		// that points virtio-gpu at a specific host DRM node — is expressible at all
		// (the GPU-configuration-surface cutover, spec-side version.cue).
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
		version: "2026.248.1030"
		name:    "record-field-to-instrument"
		// the deploy-level whole-run recording wrap `record:` field (the G5 bed-level
		// recording) is HARVESTED into the instrument: entry of the new capture model
		// (Cutover A): record_name becomes the instrument id, the #RecordWrap value
		// fields pass through into the record verb input (method: session), and the
		// runner owns the session lifecycle. A record: carrying method: is a plan-step
		// verb sugar, never converted. Deploy-node field, never a per-host deploy-overlay
		// key — no touches_host.
		//
		// version note: pinned at the schema head of its own cutover (2026.248.1030);
		// the entry stays within [floor, head] under every later head bump.
		apply: "recordFieldToInstrument"
	},
	{
		version: "2026.249.2125"
		name:    "unroll-group-deploy"
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
		//
		// version note: pinned at the group-cutover head (2026.249.2125, spec's
		// #SchemaVersion bump that widens the migratable window for this row — the
		// previous row already sat AT 2026.248.1030 and the table is strictly
		// ascending within [floor, head]).
		apply: "unrollGroupDeploy"
	},
]
