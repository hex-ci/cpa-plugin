//go:build live

// Opt-in device-login helper: prints the authorization URL for each realm,
// then polls until the user finishes. On success it writes the auth file the
// host would have written, so the account can be dropped into an auth dir.
// Never runs in CI or `make test`.
//
//	QODER_LOGIN_OUT=/opt/tmp/qoder-sandbox/auth \
//	  go test -tags live -run TestLiveDeviceLogin -v -timeout 20m .

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLiveDeviceLogin(t *testing.T) {
	outDir := os.Getenv("QODER_LOGIN_OUT")
	if outDir == "" {
		t.Skip("QODER_LOGIN_OUT not set")
	}
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	realms := []realmSpec{realmCNCfg, realmGlobalCfg}

	type session struct {
		realm    realmSpec
		nonce    string
		verifier string
	}
	var sessions []session
	for _, r := range realms {
		verifier, challenge := makePKCE()
		nonce := uuid.NewString()
		params := makeAuthParams(challenge, nonce, r)
		t.Logf("REALM %s\n  AUTH URL: %s", r.ID, r.selectAccountsURL(params))
		sessions = append(sessions, session{realm: r, nonce: nonce, verifier: verifier})
	}
	t.Logf("open one of the URLs above, authorize, and this test will finish on its own")

	deadline := time.Now().Add(15 * time.Minute)
	for time.Now().Before(deadline) {
		for i := range sessions {
			s := sessions[i]
			tok, pending, err := pollDeviceToken(s.realm, s.nonce, s.verifier)
			if err != nil {
				t.Logf("[%s] poll error: %v", s.realm.ID, err)
				continue
			}
			if pending || tok == nil {
				continue
			}
			at := tok.accessToken()
			if at == "" {
				continue
			}
			ui, uiErr := fetchUserInfo(s.realm, at)
			if uiErr != nil {
				t.Logf("[%s] userinfo failed: %v", s.realm.ID, uiErr)
			}
			sa := buildStoredAuthFromDeviceToken(s.realm, tok, ui)
			raw, buildErr := buildAuthFileJSON(nil, sa, false, "", nil)
			if buildErr != nil {
				t.Fatalf("[%s] build auth json: %v", s.realm.ID, buildErr)
			}
			name := authFileNameFor(sa)
			path := filepath.Join(outDir, name)
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatalf("[%s] write auth: %v", s.realm.ID, err)
			}
			org := ""
			if ui != nil {
				org = ui.OrganizationName
			}
			t.Logf("LOGIN OK realm=%s uid=%s nickname=%s org=%s enterprise=%s",
				s.realm.ID, sa.Account.UID, sa.Account.Nickname, org, sa.Account.EnterpriseID)
			t.Logf("auth file: %s", path)
			return
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatal("timed out waiting for authorization")
}

// makeAuthParams mirrors the query handleStartLogin builds for the web page.
func makeAuthParams(challenge, nonce string, r realmSpec) map[string][]string {
	return map[string][]string{
		"challenge":        {challenge},
		"challenge_method": {"S256"},
		"nonce":            {nonce},
		"machine_id":       {loginMachineID()},
		"client_id":        {r.ClientID},
	}
}
