package main

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The reported symptom is "some accounts check in automatically, others don't,
// and a manual check-in works". A silent skip looks identical to "the scheduler
// never ran for this account", so every branch must leave a log line carrying
// the account label. These tests pin the label shape, which is what makes a log
// line traceable back to one credential.
func TestCheckinAccountLabelIsTraceable(t *testing.T) {
	cases := []struct {
		name string
		file pluginapi.HostAuthFileEntry
		auth *storedAuth
		want string
	}{
		{
			name: "nickname plus short uid",
			file: pluginapi.HostAuthFileEntry{AuthIndex: "idx", Label: "file-label", ID: "file-id"},
			auth: &storedAuth{},
			want: "file-label/file-id",
		},
		{
			name: "empty everything falls back to nothing",
			file: pluginapi.HostAuthFileEntry{},
			auth: nil,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.auth != nil {
				tc.auth.Account.Nickname = ""
				tc.auth.Account.UID = ""
			}
			got := checkinAccountLabel(tc.file, tc.auth)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("label = %q, want empty", got)
				}
				return
			}
			if !strings.Contains(got, "/") {
				t.Fatalf("label %q is not traceable (needs name/uid)", got)
			}
		})
	}
}

// A nickname shared by two accounts must still produce distinct labels, or the
// log cannot say which credential skipped its check-in.
func TestCheckinAccountLabelDistinguishesSameNickname(t *testing.T) {
	a := &storedAuth{}
	a.Account.Nickname = "Hex"
	a.Account.UID = "01a1111111111111111111111111aaaa"
	b := &storedAuth{}
	b.Account.Nickname = "Hex"
	b.Account.UID = "01a2222222222222222222222222bbbb"

	l1 := checkinAccountLabel(pluginapi.HostAuthFileEntry{ID: "x"}, a)
	l2 := checkinAccountLabel(pluginapi.HostAuthFileEntry{ID: "x"}, b)
	if l1 == l2 {
		t.Fatalf("labels collide: %q", l1)
	}
	if !strings.HasPrefix(l1, "Hex/") || !strings.HasPrefix(l2, "Hex/") {
		t.Fatalf("labels lost the nickname: %q %q", l1, l2)
	}
}
