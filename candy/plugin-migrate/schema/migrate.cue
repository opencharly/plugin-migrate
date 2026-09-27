// plugin-migrate's OWN self-contained CUE schema — the SINGLE SOURCE for this plugin's
// declaration surface, served over the Describe channel (there is no schema-less
// plugin). SELF-CONTAINED: it references no base def, so it compiles STANDALONE (the
// property the SDK's serve-side compile and `cue exp gengotypes` both need).
//
// This is the schema SERVED over Describe. The engine-internal migration TABLE schema
// (schema/migration.cue, #Migration / #MigrationOp) is deliberately NOT served: it pins
// the SDK-owned #CanonCalVer and is embedded SEPARATELY by engine.go to validate the
// declarative table at process start. Serving it here would drag a base reference into
// a schema that must compile standalone.
//
// `command:migrate`'s authored input is its pass-through CLI grammar (`migrate
// --dry-run` etc.), not a structured plugin_input, so this schema DOCUMENTS the command
// contract.
#MigratePlugin: {
	// The command word the plugin serves.
	command: "migrate"

	// What the command does, in one line (the public-docs surface).
	contract: string & !=""
}
