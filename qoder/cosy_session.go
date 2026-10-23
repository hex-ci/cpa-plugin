// cosy_session.go manages per-auth cosySession instances. Each Qoder
// account gets one session (RSA-wrapped AES key + identity) reused across
// requests until the jobToken is refreshed (which changes security_oauth_token
// and forces a session rebuild).
package main

import (
	"sync"
)

// cosySessionCache maps authUID → *cosySession. Lazy-built on first inference
// call; invalidated when the underlying storedAuth.AccessToken changes (i.e.
// after a successful jobToken refresh).
var cosySessionCache sync.Map // map[string]*cosySessionCacheEntry

type cosySessionCacheEntry struct {
	session     *cosySession
	accessToken string // token the session was built for
}

// cosySessionFor returns the cached session for sa, rebuilding if the access
// token has rotated since the session was created.
func cosySessionFor(sa *storedAuth) (*cosySession, error) {
	if sa == nil || sa.Auth.AccessToken == "" {
		return nil, errEmptyToken
	}
	key := sa.Account.UID + "|" + sa.Account.EnterpriseID + "|" + sa.Account.UserType
	if sa.Account.UID == "" {
		n := len(sa.Auth.AccessToken)
		if n > 16 {
			n = 16
		}
		key = sa.Auth.AccessToken[:n]
	}
	if v, ok := cosySessionCache.Load(key); ok {
		if e, ok2 := v.(*cosySessionCacheEntry); ok2 && e.accessToken == sa.Auth.AccessToken {
			return e.session, nil
		}
	}
	// The identity body must describe the real account: an enterprise account
	// sent as a personal one gets routed to a node that cannot serve it, and
	// upstream only reports a generic "unsupported model format".
	id := cosyIdentity{
		Name:               sa.Account.Nickname,
		AID:                sa.Account.UID,
		UID:                sa.Account.UID,
		YxUID:              "",
		OrganizationID:     sa.Account.EnterpriseID,
		OrganizationName:   sa.Account.OrganizationName,
		UserType:           accountUserType(sa),
		SecurityOauthToken: sa.Auth.AccessToken,
		RefreshToken:       sa.Auth.RefreshToken,
	}
	sess, err := newCosySession(id)
	if err != nil {
		return nil, err
	}
	cosySessionCache.Store(key, &cosySessionCacheEntry{
		session:     sess,
		accessToken: sa.Auth.AccessToken,
	})
	return sess, nil
}

// invalidateCosySession drops the cached session for an authUID. Called after
// a successful jobToken refresh so the next inference call rebuilds identity
// with the new access token.
func invalidateCosySession(authUID string) {
	cosySessionCache.Delete(authUID)
}

type cosyError string

func (e cosyError) Error() string { return string(e) }

const errEmptyToken = cosyError("cosy: empty access token")
