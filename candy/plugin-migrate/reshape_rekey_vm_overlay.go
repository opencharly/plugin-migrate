package migrate

// reshape_rekey_vm_overlay.go — the per-host-overlay legacy `vm:` key migration.
//
// The per-host overlay (~/.config/charly/charly.yml) is a DeployConfig whose
// `deploy:` map is keyed by the deploy IDENTITY (the dotted tree path, e.g.
// `charly.check-k3s-vm`). It once keyed VM deploys by the LOSSY
// `vm:<VmDomainIdentity>` projection — e.g. `vm:charly-check-k3s-vm` — which
// flattened the instance (`/`) and nested-path (`.`) separators to `-`. That
// convention is REMOVED, and `charly migrate` is the ONE and ONLY place that
// handles it: every other path is clean with ZERO backward compat.
//
// This hook targets the overlay's top-level `deploy:` mapping. A project
// charly.yml authors its entities as top-level name-first nodes and carries no
// top-level `deploy:` mapping, so the hook is a genuine no-op on the project-file
// sweep the engine's generic apply path also performs.
//
// For every `^vm:` key it either DROPS the key — when a dotted sibling already
// carries the same domain identity under the new spelling (no information is lost)
// — or it PANICS with an actionable re-key message. The panic is the engine's
// existing hard-error channel for a `apply:` hook (see compactNodeForm's doc
// comment): a migration that cannot faithfully convert the config must fail
// loudly, never silently drop and never guess.

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/opencharly/spec/spec"
)

// legacyVMOverlayKey matches the removed per-host deploy-key convention. Only a
// `^vm:`-prefixed key is ever a legacy entry; a valid identity key that merely
// contains `-` or `.` (e.g. `charly.check-k3s-vm`, `versa/ecovoyage`) is never
// touched.
var legacyVMOverlayKey = regexp.MustCompile(`^vm:`)

// rekeyLegacyVMOverlay is the goHooks entry for the per-host overlay's legacy
// `vm:` deploy keys. It returns whether the `deploy:` mapping changed; it is
// idempotent (a migrated overlay carries no `^vm:` key). An orphan `vm:` key with
// no dotted twin panics with the actionable re-key message — the engine treats a
// panic as the migration failing loudly.
func rekeyLegacyVMOverlay(doc *yaml.Node) bool {
	root := doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return false
	}
	deploy := childMapping(root, "deploy")
	if deploy == nil {
		return false
	}
	changed := false
	for i := 0; i+1 < len(deploy.Content); {
		key := deploy.Content[i].Value
		if !legacyVMOverlayKey.MatchString(key) {
			i += 2
			continue
		}
		if twinDeployKey(deploy, key) {
			// carry the dropped key's head comment onto the following key so a
			// section banner is not lost (mirrors the delete_key op).
			if i+2 < len(deploy.Content) && deploy.Content[i].HeadComment != "" && deploy.Content[i+2].HeadComment == "" {
				deploy.Content[i+2].HeadComment = deploy.Content[i].HeadComment
			}
			deploy.Content = append(deploy.Content[:i], deploy.Content[i+2:]...)
			changed = true
			continue
		}
		// No dotted twin: the deploy identity is not recoverable from the lossy
		// key. Fail loudly (the engine's panic channel) — never silently drop.
		panic(fmt.Sprintf("cannot re-key legacy per-host entry %q: the VM domain identity is lossy (`/`→`-`, `.`→`-`), so the deploy identity is not recoverable from the key. Re-key the entry to its deploy identity (e.g. `charly.check-k3s-vm`) and re-run `charly migrate`.", key))
	}
	return changed
}

// twinDeployKey reports whether deploy carries a sibling key that already holds
// the same VM domain identity as legacyKey under the NEW spelling: a dotted
// (contains `.`), non-`vm:` identity key. In that case the legacy key is
// redundant and may be dropped with no information lost.
func twinDeployKey(deploy *yaml.Node, legacyKey string) bool {
	d := spec.VmDomainIdentity(legacyKey)
	for i := 0; i+1 < len(deploy.Content); i += 2 {
		y := deploy.Content[i].Value
		if y == legacyKey || strings.HasPrefix(y, "vm:") || !strings.Contains(y, ".") {
			continue
		}
		if spec.VmDomainIdentity(y) == d {
			return true
		}
	}
	return false
}
