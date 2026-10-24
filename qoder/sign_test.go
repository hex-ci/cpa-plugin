package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

// The COSY body encoding is a custom base64 variant; these vectors come from
// the protocol spec and pin the alphabet, the three-segment rotation and the
// pad character. A wrong value here breaks every signed request.
func TestQoderEncodeVectors(t *testing.T) {
	cases := []struct{ in, want string }{
		{"{}", "$kwm"},
		{`{"a":1}`, "ep$$BMn%mYKi"},
		{"hello", "q$FruHPH"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := qoderEncode([]byte(tc.in)); got != tc.want {
			t.Fatalf("qoderEncode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Machine identity must be a pure function of the uid: the same account always
// looks like the same device, and two accounts never share one.
func TestMachineIdentityIsStablePerAccount(t *testing.T) {
	build := func(uid string) *cosySession {
		sess, err := newCosySession(cosyIdentity{UID: uid, AID: uid, SecurityOauthToken: "dt-x"})
		if err != nil {
			t.Fatalf("newCosySession(%s): %v", uid, err)
		}
		return sess
	}

	a1, a2 := build("uid-aaa"), build("uid-aaa")
	if a1.MachineID != a2.MachineID {
		t.Fatalf("machine id drifted between sessions: %q vs %q", a1.MachineID, a2.MachineID)
	}
	if a1.MachineToken != a2.MachineToken || a1.MachineType != a2.MachineType || a1.Hostname != a2.Hostname {
		t.Fatal("machine token/type/hostname drifted between sessions for the same account")
	}
	// The per-session key material must NOT be stable: it is regenerated on
	// every session and wrapped with the server key.
	if a1.TempKey == a2.TempKey {
		t.Fatal("temporary AES key was reused across sessions")
	}

	b := build("uid-bbb")
	if b.MachineID == a1.MachineID || b.MachineToken == a1.MachineToken {
		t.Fatal("two accounts share one machine identity")
	}
	if !strings.HasPrefix(a1.Hostname, "DESKTOP-") || len(a1.Hostname) != len("DESKTOP-")+7 {
		t.Fatalf("hostname = %q, want DESKTOP-XXXXXXX", a1.Hostname)
	}
}

// The inference header set is what upstream fingerprints. Product identity,
// version and the machine headers must all be present, and the host name only
// on the streaming call.
func TestCosyHeadersCarryCLIIdentity(t *testing.T) {
	sess, err := newCosySession(cosyIdentity{UID: "uid-headers", AID: "uid-headers", SecurityOauthToken: "dt-x"})
	if err != nil {
		t.Fatalf("newCosySession: %v", err)
	}
	chatURL := realmCNCfg.chatURL()
	h, err := sess.headers("uid-headers", "body", chatURL, "text/event-stream", true)
	if err != nil {
		t.Fatalf("headers: %v", err)
	}
	want := map[string]string{
		"cosy-business-product": "cli",
		"cosy-business-type":    "agent",
		"cosy-scene":            "assistant",
		"cosy-clienttype":       "5",
		"login-version":         "v2",
		"cosy-version":          cosyVersion(),
		"user-agent":            cosyUserAgent,
		"accept-encoding":       "identity",
		"cosy-data-policy":      "agree",
	}
	for k, v := range want {
		if h[k] != v {
			t.Fatalf("header %s = %q, want %q", k, h[k], v)
		}
	}
	if h["cosy-machineos"] == "" {
		t.Fatal("cosy-machineos missing")
	}
	if h["cosy-machinehostname"] != sess.Hostname {
		t.Fatalf("cosy-machinehostname = %q, want %q", h["cosy-machinehostname"], sess.Hostname)
	}
	if !strings.HasPrefix(h["authorization"], "Bearer COSY.") {
		t.Fatalf("authorization = %q", h["authorization"])
	}

	// The catalog request is not a stream: no host name, no cache-control.
	h2, err := sess.headers("uid-headers", qoderEncode([]byte("{}")), realmCNCfg.modelsURL(), "application/json", false)
	if err != nil {
		t.Fatalf("headers: %v", err)
	}
	if _, ok := h2["cosy-machinehostname"]; ok {
		t.Fatal("non-stream request must not carry cosy-machinehostname")
	}
	if _, ok := h2["cache-control"]; ok {
		t.Fatal("non-stream request must not carry cache-control")
	}
}

// cosy-version is configurable: upstream bumps it with each CLI release and
// validates it against its version gate.
func TestCosyVersionFollowsConfig(t *testing.T) {
	cfgMu.Lock()
	old := cfg.CosyVersion
	cfg.CosyVersion = "9.9.9"
	cfgMu.Unlock()
	t.Cleanup(func() {
		cfgMu.Lock()
		cfg.CosyVersion = old
		cfgMu.Unlock()
	})

	sess, err := newCosySession(cosyIdentity{UID: "uid-version", SecurityOauthToken: "dt-x"})
	if err != nil {
		t.Fatalf("newCosySession: %v", err)
	}
	h, err := sess.headers("uid-version", "body", realmCNCfg.chatURL(), "text/event-stream", true)
	if err != nil {
		t.Fatalf("headers: %v", err)
	}
	if h["cosy-version"] != "9.9.9" {
		t.Fatalf("cosy-version = %q, want the configured value", h["cosy-version"])
	}
	// The payload carries the same version, and the request id differs per
	// call while the signature stays verifiable.
	payload, _, _, err := sess.buildBearer("body", realmCNCfg.chatURL())
	if err != nil {
		t.Fatalf("buildBearer: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("payload decode: %v", err)
	}
	decoded := string(raw)
	if !strings.Contains(decoded, `"cosyVersion":"9.9.9"`) {
		t.Fatalf("payload = %s, want the configured cosyVersion", decoded)
	}
	if !strings.Contains(decoded, `"ideVersion":""`) || !strings.Contains(decoded, `"version":"v1"`) {
		t.Fatalf("payload = %s, want ideVersion/version keys", decoded)
	}
}
