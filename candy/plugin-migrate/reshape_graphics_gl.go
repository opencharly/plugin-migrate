package migrate

import "gopkg.in/yaml.v3"

// reshape_graphics_gl.go — the vm-surface `libvirt.devices.graphics[].gl` SHAPE change
// (the GPU-configuration-surface cutover). `gl` was a bare scalar, which could only ever
// reach spice's `enable=` attribute and had no way to express `rendernode=` — the attribute
// that points virtio-gpu at a specific host DRM node. It becomes #LibvirtGraphicsGL
// {enable?: bool, render_node?: string}, so an authored `gl: "yes"` must become
// `gl: {enable: true}`.
//
// This is a DECLARATIVE-OP-UNSAFE reshape twice over. The four key-transform ops
// (rename_key / delete_key / remap_scalar / move_key) can rename a key or rewrite a scalar
// VALUE in place; none of them can replace a scalar node with a MAPPING node. And the field
// sits inside a LIST element (graphics is a sequence), which `under_kind` scoping cannot
// address on its own.
//
// Scoping mirrors stripCandyLibvirtField's lesson: `gl` is a short word, so the walk targets
// ONLY the exact path libvirt.devices.graphics[].gl and never a `gl:` key anywhere else.
func reshapeGraphicsGL(doc *yaml.Node) bool {
	root := doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	return reshapeGraphicsGLRec(root)
}

// reshapeGraphicsGLRec walks the whole tree looking for a mapping with a direct `libvirt:`
// key whose value is a mapping (the vm-kind domain config — NOT the `libvirt:` check-verb
// step, whose value is a verb input, nor the retired candy-level list of XML strings). From
// there it descends the fixed path devices → graphics → each element → gl.
func reshapeGraphicsGLRec(n *yaml.Node) bool {
	if n == nil {
		return false
	}
	changed := false
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			if reshapeGraphicsGLRec(c) {
				changed = true
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, val := n.Content[i], n.Content[i+1]
			if key.Value == "libvirt" && val.Kind == yaml.MappingNode {
				if reshapeGraphicsGLUnderLibvirt(val) {
					changed = true
				}
			}
			if reshapeGraphicsGLRec(val) {
				changed = true
			}
		}
	}
	return changed
}

// reshapeGraphicsGLUnderLibvirt walks libvirt → devices → graphics[] and reshapes each
// element's scalar `gl`.
func reshapeGraphicsGLUnderLibvirt(libvirt *yaml.Node) bool {
	// reshapeMapValue (reshape_compact.go) is the package's existing direct-child
	// lookup — R3: no second copy of it here.
	devices := reshapeMapValue(libvirt, "devices")
	if devices == nil || devices.Kind != yaml.MappingNode {
		return false
	}
	graphics := reshapeMapValue(devices, "graphics")
	if graphics == nil || graphics.Kind != yaml.SequenceNode {
		return false
	}
	changed := false
	for _, el := range graphics.Content {
		if el.Kind == yaml.MappingNode && reshapeOneGraphicsGL(el) {
			changed = true
		}
	}
	return changed
}

// reshapeOneGraphicsGL replaces a graphics element's SCALAR gl with {enable: <bool>}.
//
// Idempotent by construction: a gl that is already a MappingNode — the new shape, or a
// document migrated once already — is left exactly as it is, which is what lets `charly
// migrate` run twice with no second effect.
//
// An unrecognised scalar is left UNTOUCHED ON PURPOSE. A migration hook has no error
// channel (the goHooks signature returns only "changed"), so the alternatives are to guess a
// boolean or to leave the value for the schema gate to reject by name. Guessing would write a
// value the author never authored; leaving it means `charly migrate` is silent about that one
// field and the load-time CUE error names it precisely. Only the second is honest.
func reshapeOneGraphicsGL(el *yaml.Node) bool {
	for i := 0; i+1 < len(el.Content); i += 2 {
		if el.Content[i].Value != "gl" {
			continue
		}
		gl := el.Content[i+1]
		if gl.Kind != yaml.ScalarNode {
			return false // already the mapping shape — nothing to do
		}
		enable, ok := scalarAsBool(gl.Value)
		if !ok {
			return false
		}
		val := "false"
		if enable {
			val = "true"
		}
		el.Content[i+1] = &yaml.Node{
			Kind: yaml.MappingNode,
			Tag:  "!!map",
			// Flow style keeps the migrated line looking like the scalar it replaced
			// (`gl: {enable: true}`) rather than exploding a one-field block.
			Style:       yaml.FlowStyle,
			HeadComment: gl.HeadComment,
			LineComment: gl.LineComment,
			FootComment: gl.FootComment,
			Content: []*yaml.Node{
				{Kind: yaml.ScalarNode, Tag: "!!str", Value: "enable"},
				{Kind: yaml.ScalarNode, Tag: "!!bool", Value: val},
			},
		}
		return true
	}
	return false
}

// scalarAsBool accepts the spellings libvirt's own yes/no attributes and YAML 1.1 booleans
// between them produce. Anything else is not a boolean this hook may invent.
func scalarAsBool(s string) (bool, bool) {
	switch s {
	case "yes", "true", "on", "True", "TRUE", "Yes", "YES", "y":
		return true, true
	case "no", "false", "off", "False", "FALSE", "No", "NO", "n":
		return false, true
	}
	return false, false
}
