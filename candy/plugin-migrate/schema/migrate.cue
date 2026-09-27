// plugin-migrate's OWN self-contained CUE schema — the SINGLE SOURCE for this plugin's
// served declaration surface (there is no schema-less plugin: every plugin ships a
// non-empty schema over Describe).
//
// SELF-CONTAINED and PACKAGE-LESS: it references no base def and carries no package
// clause, so it compiles STANDALONE — the property the SDK's serve-side compile needs
// and the property that lets the host splice `base ++ plugin` at the load gate
// (registerPluginUnitSchema); a self-contained schema that will not splice is a LOUD
// load failure.
//
// NO GO CONSUMER: the plugin declares no typed `plugin_input` (its authored input is
// its pass-through CLI grammar), so this schema generates NO `params` package and has
// NO `cue exp gengotypes` artifact — it is the SERVED documentation/config surface,
// not a code-generation source.
//
// It DOCUMENTS the `command: migrate` contract. The engine-internal migration TABLE schema (`schema/migration.cue`, `#Migration` / `#MigrationOp`) is embedded SEPARATELY by `engine.go` and deliberately NOT served here.
#MigratePlugin: {
	// The command word the plugin serves.
	command: "migrate"

	// What the command does, in one line (the public-docs surface).
	contract: string & !=""
}
