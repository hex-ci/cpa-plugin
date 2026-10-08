// panel.go embeds the browser dashboard. panel.html is served by the host from
// the plugin's resource route, so a change to it needs a rebuild of the .so.
package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"strings"
)

//go:embed panel.html
var panelHTMLSource []byte

// panelBasePathPlaceholder is substituted at serve time with the host's
// management base path, so the panel calls the endpoints the host actually
// serves instead of assuming the historical layout.
const panelBasePathPlaceholder = "__TW_MANAGEMENT_BASE_PATH_JSON__"

// servePanel renders the dashboard for a resource sub-path. Anything but the
// panel itself is a 404, matching how the host routes resources.
func servePanel(sub string) []byte {
	switch strings.TrimRight(sub, "/") {
	case "", "/panel", "/panel.html":
	default:
		return []byte("<h1>404</h1>")
	}
	base, err := json.Marshal(loadedManagementBasePath())
	if err != nil {
		base = []byte(`"/v0/management"`)
	}
	return bytes.ReplaceAll(panelHTMLSource, []byte(panelBasePathPlaceholder), base)
}
