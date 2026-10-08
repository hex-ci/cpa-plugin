// authfile.go owns the credential record this plugin persists: how it is
// parsed, how it maps onto the host's AuthData and how the auth file is named.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// storedAuth is both the auth file body and the StorageJSON the host hands
// back on refresh. Field names are snake_case to match the other providers.
type storedAuth struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"`
	APIHost      string `json:"api_host,omitempty"`
	UID          string `json:"uid,omitempty"`
	Nickname     string `json:"nickname,omitempty"`
	AvatarURL    string `json:"avatar_url,omitempty"`
	Region       string `json:"region,omitempty"`
	MachineID    string `json:"machine_id,omitempty"`
	DeviceID     string `json:"device_id,omitempty"`
	ClientID     string `json:"client_id,omitempty"`
	// PublicKey is the device key registered with the server at login;
	// PrivateKey signs the device proof every refresh must carry (the server
	// binds a refresh token to the device that created it).
	PublicKey  string `json:"device_public_key,omitempty"`
	PrivateKey string `json:"device_private_key,omitempty"`
}

func parseStored(raw []byte) (*storedAuth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var sa storedAuth
	if err := json.Unmarshal(raw, &sa); err != nil {
		return nil, fmt.Errorf("invalid auth storage: %w", err)
	}
	if strings.TrimSpace(sa.RefreshToken) == "" && strings.TrimSpace(sa.AccessToken) == "" {
		return nil, fmt.Errorf("auth storage has no token")
	}
	return &sa, nil
}

// sanitizeUIDForFileName keeps file-name-safe characters only; the UID is
// server-supplied and becomes part of an auth file name.
func sanitizeUIDForFileName(uid string) string {
	uid = strings.TrimSpace(uid)
	var b strings.Builder
	for _, r := range uid {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		}
	}
	return b.String()
}

func labelForAuth(sa *storedAuth) string {
	if sa == nil {
		return displayName
	}
	if name := strings.TrimSpace(sa.Nickname); name != "" {
		return name
	}
	if uid := sanitizeUIDForFileName(sa.UID); uid != "" {
		return displayName + " " + uid
	}
	return displayName
}

// displayNote is the account's auth-row subtitle: identity, realm and token
// validity, all local (no network).
func displayNote(sa *storedAuth) string {
	if sa == nil {
		return ""
	}
	parts := []string{displayName}
	if region := strings.TrimSpace(sa.Region); region != "" {
		parts = append(parts, region)
	}
	if sa.UID != "" {
		parts = append(parts, "uid "+sa.UID)
	}
	if sa.ExpiresAt > 0 {
		parts = append(parts, "token "+time.Unix(sa.ExpiresAt, 0).Format("2006-01-02 15:04"))
	}
	return strings.Join(parts, " · ")
}

// toAuthData maps the stored credential onto the host's auth record. carrier
// carries user-owned metadata (weight, priority, prefix, note…) the host
// already holds; host-owned keys win, everything else is passed through.
func toAuthData(sa *storedAuth, carrier map[string]any) pluginapi.AuthData {
	storage, err := json.Marshal(sa)
	if err != nil {
		storage = nil
	}
	id := providerName
	fileName := authFileNameFor(sa)
	if uid := sanitizeUIDForFileName(sa.UID); uid != "" {
		id = uid
	}
	meta := map[string]any{
		"type": providerName,
		"logo": pluginLogoURL,
	}
	if sa.Nickname != "" {
		meta["name"] = sa.Nickname
	}
	if sa.AvatarURL != "" {
		meta["avatar"] = sa.AvatarURL
	}
	meta["note"] = displayNote(sa)
	for key, value := range carrier {
		if _, owned := meta[key]; owned {
			continue
		}
		meta[key] = value
	}
	return pluginapi.AuthData{
		Provider:    providerName,
		ID:          id,
		FileName:    fileName,
		Label:       labelForAuth(sa),
		StorageJSON: storage,
		Metadata:    meta,
	}
}

// authFileNameFor is the auth file name for one account: the uid keeps several
// logins of the same provider apart, matching the host's own naming.
func authFileNameFor(sa *storedAuth) string {
	if sa == nil {
		return authFileName
	}
	if uid := sanitizeUIDForFileName(sa.UID); uid != "" {
		return providerName + "-" + uid + ".json"
	}
	return authFileName
}

// authFileJSON is the on-disk record for one account: the storage keys plus the
// metadata the host merges in when it persists a login itself. Writing the same
// shape keeps a panel-side save identical to a host-side one.
func authFileJSON(sa *storedAuth, carrier map[string]any) ([]byte, error) {
	record := toAuthData(sa, carrier)
	out := map[string]any{}
	if len(record.StorageJSON) > 0 {
		if err := json.Unmarshal(record.StorageJSON, &out); err != nil {
			return nil, err
		}
	}
	for key, value := range record.Metadata {
		out[key] = value
	}
	out["type"] = providerName
	return json.Marshal(out)
}

// authSaveFn is the seam the tests use: no host exists outside the gateway.
var authSaveFn = hostAuthSave

// saveAuthRecord writes one account's auth file through the host.
func saveAuthRecord(sa *storedAuth, carrier map[string]any) (string, error) {
	payload, err := authFileJSON(sa, carrier)
	if err != nil {
		return "", err
	}
	return authSaveFn(authFileNameFor(sa), payload)
}

// parseDisabledFromAuthJSON reads the host's `disabled` flag out of an auth file.
func parseDisabledFromAuthJSON(raw []byte) bool {
	var probe struct {
		Disabled bool `json:"disabled"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	return probe.Disabled
}

// tokenFingerprint is a non-reversible token marker for logs; never log a
// token, not even partially.
func tokenFingerprint(token string) string {
	if token == "" {
		return "-"
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:4])
}
