// realm.go resolves the upstream region (CN vs Global) for one credential and
// carries the per-region base URLs and OAuth client ids.
//
// Both regions expose the same API shape; only hosts and the OAuth client id
// differ. The region is stored on the credential's `auth.domain` field, so an
// auth file is self-describing and survives a plugin config change.
package main

import (
	"net/url"
	"strings"
)

const (
	realmCN     = "cn"
	realmGlobal = "global"
	// cliClientID is the OAuth client id the official Qoder CLI presents in
	// production (both regions). Anything else makes the authorization appear
	// under a different app name in the user's Qoder account.
	cliClientID = "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb"
)

// realmSpec is one upstream region.
type realmSpec struct {
	ID       string
	Website  string // device-authorization page host
	Openapi  string // account plane: auth, userinfo, quota, plan, check-in
	Gateway  string // inference plane: chat, model catalog
	ClientID string // device-flow client id
}

var (
	realmCNCfg = realmSpec{
		ID:      realmCN,
		Website: "https://qoder.com.cn",
		Openapi: "https://openapi.qoder.com.cn",
		Gateway: "https://gateway.qoder.com.cn",
		// The official Qoder CLI uses one prod OAuth client id for both regions
		// (qodercli bundle: MXc for prod, a different id only for staging), so
		// CN must present the same client or the authorization shows up in the
		// user's app list as the other Qoder product.
		ClientID: cliClientID,
	}
	realmGlobalCfg = realmSpec{
		ID:       realmGlobal,
		Website:  "https://qoder.com",
		Openapi:  "https://openapi.qoder.sh",
		Gateway:  "https://api3.qoder.sh",
		ClientID: cliClientID,
	}
)

// realmByName normalises a realm identifier or a stored domain into a realm.
// Unknown values fall back to CN, which is where every legacy credential of
// this plugin lives.
func realmByName(name string) realmSpec {
	switch normalizeRealmName(name) {
	case realmGlobal, "qoder.com", "www.qoder.com", "api.qoder.com", "openapi.qoder.sh", "api3.qoder.sh", "qoder.sh":
		return realmGlobalCfg
	default:
		return realmCNCfg
	}
}

// normalizeRealmName accepts whatever the credential recorded (realm id, bare
// host, or the full website URL that OAuth login writes) and reduces it to a
// bare host. Without this a Global credential stored as "https://qoder.com"
// silently resolves to CN and its calls go to the wrong region.
func normalizeRealmName(name string) string {
	v := strings.ToLower(strings.TrimSpace(name))
	v = strings.TrimPrefix(v, "https://")
	v = strings.TrimPrefix(v, "http://")
	if i := strings.IndexByte(v, '/'); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSuffix(v, ".")
}

// realmOf returns the realm of one credential. The `domain` field is written by
// the OAuth/PAT import paths; older files only carry CN.
func realmOf(sa *storedAuth) realmSpec {
	if sa == nil {
		return realmCNCfg
	}
	return realmByName(sa.Auth.Domain)
}

func (r realmSpec) pollURL(nonce, verifier string) string {
	q := url.Values{}
	q.Set("nonce", nonce)
	q.Set("verifier", verifier)
	q.Set("challenge_method", "S256")
	return r.Openapi + "/api/v1/deviceToken/poll?" + q.Encode()
}

func (r realmSpec) selectAccountsURL(params url.Values) string {
	return r.Website + "/device/selectAccounts?" + params.Encode()
}

func (r realmSpec) deviceRefreshURL() string { return r.Openapi + "/api/v1/deviceToken/refresh" }
func (r realmSpec) jobExchangeURL() string   { return r.Openapi + "/api/v1/jobToken/exchange" }
func (r realmSpec) jobRefreshURL() string    { return r.Openapi + "/api/v1/jobToken/refresh" }
func (r realmSpec) userInfoURL() string      { return r.Openapi + "/api/v1/userinfo" }
func (r realmSpec) quotaURL() string         { return r.Openapi + "/api/v2/quota/usage" }
func (r realmSpec) planURL() string          { return r.Openapi + "/api/v2/user/plan" }
func (r realmSpec) checkinStatusURL() string {
	return r.Openapi + "/sash/api/v1/me/daily-check-in/status"
}
func (r realmSpec) checkinClaimURL() string {
	return r.Openapi + "/sash/api/v1/me/daily-check-in/claim"
}
func (r realmSpec) proEligibleURL() string {
	return r.Openapi + "/sash/api/v1/me/pro-upgrade/eligibility"
}
func (r realmSpec) proClaimURL() string    { return r.Openapi + "/sash/api/v1/me/pro-upgrade/claim" }
func (r realmSpec) queueStatusURL() string { return r.Openapi + "/api/v2/service/ask/queue/status" }

// chatURL is the SSE inference endpoint.
func (r realmSpec) chatURL() string {
	return r.Gateway + "/algo/api/v2/service/pro/sse/agent_chat_generation" +
		"?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"
}

// modelsURL is the model catalog endpoint.
func (r realmSpec) modelsURL() string { return r.Gateway + "/algo/api/v2/model/list?Encode=1" }

// accountUserAgent identifies the plugin as the official CLI on the account
// plane. The desktop shells send their own product UA; the CLI (which this
// plugin mirrors on the inference plane) sends qoder/<version>.
func (r realmSpec) accountUserAgent(cosyVersion string) string {
	return "qoder/" + cosyVersion
}
