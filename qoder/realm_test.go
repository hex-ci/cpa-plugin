package main

import "testing"

// The realm decides hosts, the OAuth client id and the account-plane UA. A
// credential stores its realm in auth.domain, so an old CN file keeps working
// after the plugin learns about Global.
func TestRealmResolution(t *testing.T) {
	cases := map[string]string{
		"":                     realmCN,
		"cn":                   realmCN,
		"qoder.com.cn":         realmCN,
		"https://qoder.com.cn": realmCN,
		"global":               realmGlobal,
		"qoder.com":            realmGlobal,
		"openapi.qoder.sh":     realmGlobal,
		"anything-else":        realmCN,
	}
	for in, want := range cases {
		if got := realmByName(in).ID; got != want {
			t.Fatalf("realmByName(%q) = %q, want %q", in, got, want)
		}
	}

	cn := &storedAuth{}
	cn.Auth.Domain = "qoder.com.cn"
	if realmOf(cn).ID != realmCN {
		t.Fatal("legacy CN credential did not resolve to cn")
	}
	gl := &storedAuth{}
	gl.Auth.Domain = "qoder.com"
	if realmOf(gl).ID != realmGlobal {
		t.Fatal("global credential did not resolve to global")
	}
	if realmOf(nil).ID != realmCN {
		t.Fatal("nil credential must default to cn")
	}
}

func TestRealmEndpoints(t *testing.T) {
	cn, gl := realmCNCfg, realmGlobalCfg
	if cn.Openapi != "https://openapi.qoder.com.cn" || cn.Gateway != "https://gateway.qoder.com.cn" {
		t.Fatalf("cn hosts = %+v", cn)
	}
	if gl.Openapi != "https://openapi.qoder.sh" || gl.Gateway != "https://api3.qoder.sh" {
		t.Fatalf("global hosts = %+v", gl)
	}
	// Both regions must present the official CLI's prod client id: using the
	// other Qoder product's id makes the authorization appear under that
	// product's name in the user's account.
	for _, r := range []realmSpec{cn, gl} {
		if r.ClientID != cliClientID {
			t.Fatalf("%s client id = %q, want the CLI id", r.ID, r.ClientID)
		}
	}
	if cn.chatURL() == gl.chatURL() {
		t.Fatal("chat URL is region-independent")
	}
	if got := cn.chatURL(); got[:len(cn.Gateway)] != cn.Gateway {
		t.Fatalf("chat URL %q is not on the gateway host", got)
	}
	for _, r := range []realmSpec{cn, gl} {
		for name, url := range map[string]string{
			"quota":     r.quotaURL(),
			"plan":      r.planURL(),
			"userinfo":  r.userInfoURL(),
			"checkin":   r.checkinStatusURL(),
			"poll":      r.pollURL("n", "v"),
			"models":    r.modelsURL(),
			"queue":     r.queueStatusURL(),
			"refreshDt": r.deviceRefreshURL(),
			"refreshJt": r.jobRefreshURL(),
		} {
			if len(url) < len(r.Openapi) && len(url) < len(r.Gateway) {
				t.Fatalf("%s.%s = %q", r.ID, name, url)
			}
		}
	}
	// The device flow must not carry a redirect_uri: it is a poll-based flow.
	if u := cn.selectAccountsURL(nil); u != cn.Website+"/device/selectAccounts?" {
		t.Fatalf("selectAccounts URL = %q", u)
	}
}

// OAuth login records the website URL while PAT import records a realm id.
// Both must resolve to the realm the credential actually belongs to, or a
// Global account gets CN endpoints and every call 401s.
func TestRealmOfResolvesStoredForms(t *testing.T) {
	cases := map[string]string{
		"":                      realmCN,
		"cn":                    realmCN,
		"https://qoder.com.cn":  realmCN,
		"https://qoder.com.cn/": realmCN,
		"global":                realmGlobal,
		"https://qoder.com":     realmGlobal,
		"https://qoder.com/":    realmGlobal,
		"HTTP://Qoder.com/path": realmGlobal,
		"qoder.com":             realmGlobal,
	}
	for stored, want := range cases {
		sa := &storedAuth{Auth: storedTokens{Domain: stored}}
		if got := realmOf(sa).ID; got != want {
			t.Fatalf("domain %q resolved to %s, want %s", stored, got, want)
		}
	}
	if realmOf(nil).ID != realmCN {
		t.Fatal("nil auth must fall back to CN")
	}
}
