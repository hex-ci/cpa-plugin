// migrate.go adopts credentials written under this plugin's previous name.
//
// A rename is not just a directory move: the host classifies an auth file by
// its top-level `type` field and by filename prefix. A file that still says
// `type: qoderwork` is not routed to this plugin at all, so it would silently
// disappear from the panel. Adoption runs once per configure(): it rewrites
// such files under the current name/type and removes the stale copy.
//
// Only files that parse as this plugin's credential shape are touched, so a
// foreign provider's file can never be rewritten by mistake.
package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// migrateLegacyAuthFiles rewrites legacy auth files to the current identity.
// Best-effort: a failure is logged and never blocks registration.
//
// The host owns the files, so this goes through host.auth.save plus a confined
// delete of the old path — never a direct write.
func migrateLegacyAuthFiles() {
	files, err := hostAuthListFiles()
	if err != nil || len(files) == 0 {
		return
	}
	adopted := 0
	for _, f := range files {
		lowerName := strings.ToLower(strings.TrimSpace(f.Name))
		hasLegacyPrefix := strings.HasPrefix(lowerName, legacyAuthPrefix)
		isLegacySingle := lowerName == legacyProvider+".json"
		if !hasLegacyPrefix && !isLegacySingle {
			continue
		}
		phys, err := hostAuthGetPhysical(f.AuthIndex)
		if err != nil || phys == nil {
			continue
		}
		// Ownership proof: the file must carry our credential shape and the
		// legacy provider marker (or no marker at all, for pre-convention
		// files written by this plugin before the rename).
		var doc struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(phys.JSON, &doc)
		declared := strings.ToLower(strings.TrimSpace(doc.Type))
		if declared != "" && declared != legacyProvider {
			continue
		}
		sa, err := parseStored(phys.JSON)
		if err != nil || sa == nil || strings.TrimSpace(sa.Account.UID) == "" {
			continue
		}
		target := authFileNameFor(sa)
		if strings.EqualFold(filepath.Base(strings.TrimSpace(phys.Path)), target) && declared == providerName {
			continue // already migrated
		}
		note := displayNote(sa, nil, parseDisabledFromAuthJSON(phys.JSON))
		raw, err := buildAuthFileJSON(phys.JSON, sa, parseDisabledFromAuthJSON(phys.JSON), note, nil)
		if err != nil {
			hostLogf("warn", fmt.Sprintf("legacy auth adopt: build %s: %v", target, err))
			continue
		}
		if err := hostAuthPersist(target, "", raw); err != nil {
			hostLogf("warn", fmt.Sprintf("legacy auth adopt: save %s: %v", target, err))
			continue
		}
		if path := strings.TrimSpace(phys.Path); path != "" && !strings.EqualFold(filepath.Base(path), target) {
			if err := deleteAuthFileInDir(path, filepath.Dir(path)); err != nil {
				hostLogf("warn", fmt.Sprintf("legacy auth adopt: remove %s: %v", filepath.Base(path), err))
			}
		}
		adopted++
	}
	if adopted > 0 {
		hostLogf("info", fmt.Sprintf("adopted %d legacy auth file(s) under the qoder identity", adopted))
	}
}
