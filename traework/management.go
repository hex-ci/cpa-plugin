// management.go exposes the plugin's Management API routes and the
// browser-facing panel resource to the host.
package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type managementRequestWire struct {
	pluginapi.ManagementRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type managementRegistrationResponse struct {
	Routes    []managementRoute `json:"routes,omitempty"`
	Resources []resourceRoute   `json:"resources,omitempty"`
}

type managementRoute struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
}

type resourceRoute struct {
	Path        string `json:"path"`
	Menu        string `json:"menu,omitempty"`
	Description string `json:"description,omitempty"`
}

// loginCallbackBody is the panel's paste payload: the attempt it belongs to and
// the callback URL or auth code the operator pasted.
type loginCallbackBody struct {
	State string `json:"state"`
	Input string `json:"input"`
}

func managementRegistration() managementRegistrationResponse {
	base := "/plugins/" + providerName
	return managementRegistrationResponse{
		Routes: []managementRoute{
			{Method: http.MethodGet, Path: base + "/accounts", Description: "List TraeWork accounts with quota snapshots."},
			{Method: http.MethodGet, Path: base + "/credits", Description: "Read quota and daily-bonus state for one (auth_index) or all accounts."},
			{Method: http.MethodPost, Path: base + "/checkin", Description: "Claim the daily bonus for one (auth_index) or all accounts."},
			{Method: http.MethodPost, Path: base + "/checkin/config", Description: "Toggle the automatic daily claim (enabled: true/false)."},
			{Method: http.MethodPost, Path: base + "/login/start", Description: "Start a login attempt the panel drives."},
			{Method: http.MethodGet, Path: base + "/login/status", Description: "Report one login attempt."},
			{Method: http.MethodGet, Path: base + "/login/current", Description: "Report the attempt still waiting for a code."},
			{Method: http.MethodPost, Path: base + "/login/callback", Description: "Accept a pasted callback URL or auth code."},
			{Method: http.MethodPost, Path: base + "/login/cancel", Description: "Drop a login attempt."},
		},
		Resources: []resourceRoute{
			{Path: "/panel", Menu: displayName, Description: "TraeWork dashboard: account overview."},
			// The OAuth callback target for deployments where the callback cannot
			// be the browser's own loopback: this route is already reachable
			// wherever the panel is.
			{Path: "/authorize", Description: "TraeWork OAuth callback target (no menu entry)."},
		},
	}
}

func handleManagement(raw []byte) ([]byte, error) {
	var req managementRequestWire
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	path := strings.TrimRight(req.Path, "/")

	// The panel is a browser resource: the host serves it without management
	// authentication so it can render its own key prompt.
	if req.Method == http.MethodGet && strings.HasPrefix(path, loadedResourceBasePath()) {
		sub := strings.TrimPrefix(path, loadedResourceBasePath())
		if strings.TrimRight(sub, "/") == "/authorize" {
			return okEnvelope(mgmtHTMLResponse([]byte(handleCallbackResource(req.Query, req.Headers)))), nil
		}
		return okEnvelope(mgmtHTMLResponse(servePanel(sub))), nil
	}
	if !managementAuthorized(req) {
		return okEnvelope(mgmtJSONResponse(http.StatusUnauthorized, map[string]any{"error": "unauthorized"})), nil
	}
	switch {
	case req.Method == http.MethodGet && strings.HasSuffix(path, "/accounts"):
		payload, err := accountsPayload()
		if err != nil {
			return okEnvelope(mgmtJSONResponse(http.StatusInternalServerError, map[string]any{"error": err.Error()})), nil
		}
		return okEnvelope(mgmtJSONResponse(http.StatusOK, payload)), nil
	case req.Method == http.MethodGet && strings.HasSuffix(path, "/credits"):
		payload, err := creditsPayload(req.Query.Get("auth_index"), req.Query.Get("fresh") != "")
		if err != nil {
			return okEnvelope(mgmtJSONResponse(http.StatusBadRequest, map[string]any{"error": err.Error()})), nil
		}
		return okEnvelope(mgmtJSONResponse(http.StatusOK, payload)), nil
	case req.Method == http.MethodPost && strings.HasSuffix(path, "/checkin"):
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleCheckinClaim(req.ManagementRequest))), nil
	case req.Method == http.MethodPost && strings.HasSuffix(path, "/checkin/config"):
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleCheckinConfig(req.ManagementRequest))), nil
	case req.Method == http.MethodGet && strings.HasSuffix(path, "/login/current"):
		return okEnvelope(mgmtJSONResponse(http.StatusOK, panelLoginCurrent())), nil
	case req.Method == http.MethodPost && strings.HasSuffix(path, "/login/start"):
		payload, err := panelLoginStart()
		if err != nil {
			return okEnvelope(mgmtJSONResponse(http.StatusInternalServerError, map[string]any{"error": err.Error()})), nil
		}
		return okEnvelope(mgmtJSONResponse(http.StatusOK, payload)), nil
	case req.Method == http.MethodGet && strings.HasSuffix(path, "/login/status"):
		payload, err := panelLoginStatus(req.Query.Get("state"))
		if err != nil {
			return okEnvelope(mgmtJSONResponse(http.StatusBadRequest, map[string]any{"error": err.Error()})), nil
		}
		return okEnvelope(mgmtJSONResponse(http.StatusOK, payload)), nil
	case req.Method == http.MethodPost && strings.HasSuffix(path, "/login/callback"):
		var body loginCallbackBody
		_ = json.Unmarshal(req.Body, &body)
		payload, err := panelLoginSubmit(body.State, body.Input)
		if err != nil {
			return okEnvelope(mgmtJSONResponse(http.StatusBadRequest, map[string]any{"error": err.Error()})), nil
		}
		return okEnvelope(mgmtJSONResponse(http.StatusOK, payload)), nil
	case req.Method == http.MethodPost && strings.HasSuffix(path, "/login/cancel"):
		var body loginCallbackBody
		_ = json.Unmarshal(req.Body, &body)
		payload, err := panelLoginCancel(body.State)
		if err != nil {
			return okEnvelope(mgmtJSONResponse(http.StatusBadRequest, map[string]any{"error": err.Error()})), nil
		}
		return okEnvelope(mgmtJSONResponse(http.StatusOK, payload)), nil
	}
	return okEnvelope(mgmtJSONResponse(http.StatusNotFound, map[string]any{"error": "not found"})), nil
}

// managementAuthorized mirrors the other plugins: no configured key means the
// host's own management auth is the only gate; a configured key must match the
// Bearer header or the panel's ?key= query parameter.
func managementAuthorized(req managementRequestWire) bool {
	want := loadString(managementKey)
	if want == "" {
		want = strings.TrimSpace(os.Getenv("TW_MANAGEMENT_KEY"))
	}
	if want == "" {
		return true
	}
	got := strings.TrimSpace(strings.TrimPrefix(req.Headers.Get("Authorization"), "Bearer "))
	if got == "" {
		got = strings.TrimSpace(req.Query.Get("key"))
	}
	return got == want
}

type accountView struct {
	AuthIndex string `json:"auth_index"`
	FileName  string `json:"file_name"`
	Label     string `json:"label"`
	UID       string `json:"uid,omitempty"`
	Region    string `json:"region,omitempty"`
	Nickname  string `json:"nickname,omitempty"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
	ExpiresIn int64  `json:"expires_in,omitempty"`
	Expired   bool   `json:"expired"`
	Disabled  bool   `json:"disabled"`
	Note      string `json:"note,omitempty"`
	// Credits and Checkin come from the cache only: listing accounts stays one
	// round trip and the panel fills each card through /credits.
	Credits *creditsView `json:"credits,omitempty"`
	Checkin *checkinView `json:"checkin,omitempty"`
}

// creditsPayload reads quota snapshots. With an auth_index it answers for that
// one account (what a card's refresh button asks for); without one it answers
// for every account, so a headless caller can inspect the whole pool.
func creditsPayload(authIndex string, fresh bool) (map[string]any, error) {
	if authIndex != "" {
		sa, _, err := hostAuthGetBundle(authIndex)
		if err != nil {
			return nil, err
		}
		credits, checkin, err := cachedCredentials(sa, authIndex, fresh)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"auth_index": authIndex,
			"credits":    credits,
			"checkin":    checkin,
		}, nil
	}

	files, err := hostAuthList()
	if err != nil {
		return nil, err
	}
	out := make(map[string]any, len(files))
	for _, f := range files {
		sa, _, err := hostAuthGetBundle(f.AuthIndex)
		if err != nil {
			continue
		}
		credits, checkin, err := cachedCredentials(sa, f.AuthIndex, fresh)
		if err != nil {
			out[f.AuthIndex] = map[string]any{"error": err.Error()}
			continue
		}
		out[f.AuthIndex] = map[string]any{"credits": credits, "checkin": checkin}
	}
	return map[string]any{"accounts": out}, nil
}

func accountsPayload() (map[string]any, error) {
	files, err := hostAuthList()
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	out := make([]accountView, 0, len(files))
	for _, f := range files {
		sa, phys, err := hostAuthGetBundle(f.AuthIndex)
		if err != nil {
			// A file we cannot read is still worth listing: the user needs to
			// see that the host holds a broken credential.
			out = append(out, accountView{
				AuthIndex: f.AuthIndex,
				FileName:  f.Name,
				Label:     displayName,
				Note:      "凭据无法解析：" + err.Error(),
			})
			continue
		}
		view := accountView{
			AuthIndex: phys.AuthIndex,
			FileName:  phys.Name,
			Label:     labelForAuth(sa),
			UID:       sa.UID,
			Region:    sa.Region,
			Nickname:  sa.Nickname,
			ExpiresAt: sa.ExpiresAt,
			Disabled:  phys.Disabled,
		}
		view.Credits, view.Checkin = peekCredentials(phys.AuthIndex)
		view.Expired = sa.ExpiresAt > 0 && sa.ExpiresAt <= now
		if sa.ExpiresAt > 0 {
			view.ExpiresIn = sa.ExpiresAt - now
		}
		out = append(out, view)
	}
	return map[string]any{
		"provider":     providerName,
		"count":        len(out),
		"accounts":     out,
		"checkin_auto": checkinAutoEnabled(),
	}, nil
}

func mgmtJSONResponse(status int, payload any) pluginapi.ManagementResponse {
	body, err := json.Marshal(payload)
	if err != nil {
		status = http.StatusInternalServerError
		body = []byte(`{"error":"encode failed"}`)
	}
	h := http.Header{}
	h.Set("Content-Type", "application/json; charset=utf-8")
	return pluginapi.ManagementResponse{StatusCode: status, Headers: h, Body: body}
}

func mgmtHTMLResponse(body []byte) pluginapi.ManagementResponse {
	h := http.Header{}
	h.Set("Content-Type", "text/html; charset=utf-8")
	return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: h, Body: body}
}
