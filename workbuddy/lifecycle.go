// lifecycle.go implements credit-based auth lifecycle for WorkBuddy:
//   - CN exhausted  → disable auth file (disabled:true), re-enable after check-in when credits return
//   - Global exhausted → delete auth file (one-shot quota)
//   - Unknown credits → no-op (never mis-kill)
//   - Hard credit errors from executor → recheck credits then apply policy
//   - Soft rate limits → do not delete Global
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var (
	lifecycleState             sync.Map // auth_id (auth.ID) -> lifecycleStateEntry
	lifecycleSaveTTL           = 30 * time.Second
	reconcileHostAuthGetBundle = hostAuthGetBundle
)

type lifecycleStateEntry struct {
	disabled bool
	note     string
	at       time.Time
}

func lifecycleStateUnchanged(authID string, disabled bool, note string) bool {
	v, ok := lifecycleState.Load(authID)
	if !ok {
		return false
	}
	e := v.(*lifecycleStateEntry)
	if e.disabled != disabled || e.note != note {
		return false
	}
	return time.Since(e.at) < lifecycleSaveTTL
}

func rememberLifecycleState(authID string, disabled bool, note string) {
	lifecycleState.Store(authID, &lifecycleStateEntry{disabled: disabled, note: note, at: time.Now()})
}

// pruneLifecycleState removes entries for auth indices that no longer exist
// or whose TTL has expired. Called from dashboard prune to prevent unbounded growth.
func pruneLifecycleState() {
	files, err := hostAuthList()
	if err != nil {
		return
	}
	live := make(map[string]struct{}, len(files))
	for _, f := range files {
		live[f.ID] = struct{}{}
	}
	lifecycleState.Range(func(key, value any) bool {
		idx, _ := key.(string)
		if _, ok := live[idx]; !ok {
			lifecycleState.Delete(key)
			return true
		}
		if e, ok := value.(*lifecycleStateEntry); ok && time.Since(e.at) > 10*time.Minute {
			lifecycleState.Delete(key)
		}
		return true
	})
}

// disableAuth writes disabled:true for a CN (or fallback) account.
func disableAuth(authIndex, authID string, sa *storedAuth, cr *creditsSummary, reason string) error {
	mu := checkinLockFor(authIndex)
	mu.Lock()
	defer mu.Unlock()

	note := displayNote(sa, cr, true)
	if reason != "" {
		// A fault reason is the whole story: the credit wording next to it is
		// either unknown or misleading ("积分未知"), and the panel keys the
		// re-login hint off this text.
		note = reason
	}
	if lifecycleStateUnchanged(authID, true, note) {
		return nil
	}
	// Prefer live physical file to preserve any extra fields if present.
	phys, err := hostAuthGetPhysical(authIndex)
	if err == nil && parseDisabledFromAuthJSON(phys.JSON) {
		// already disabled; still refresh note if needed
		if lifecycleStateUnchanged(authID, true, note) {
			return nil
		}
	}
	name := authFileNameFor(sa)
	path := ""
	legacyPath := ""
	if phys != nil {
		name, path, legacyPath = resolveAuthFileTarget(sa, phys)
	}
	raw, err := buildAuthFileJSON(physJSON(phys), sa, true, note, nil)
	if err != nil {
		return err
	}
	if err := hostAuthPersistMigrate(name, path, legacyPath, raw); err != nil {
		return err
	}
	rememberLifecycleState(authID, true, note)
	accountCache.Delete(authID)
	return nil
}

// reenableAuth writes disabled:false when CN has credits again.
func reenableAuth(authIndex, authID string, sa *storedAuth, cr *creditsSummary) error {
	mu := checkinLockFor(authIndex)
	mu.Lock()
	defer mu.Unlock()

	if !shouldReenableCN(true, cr) {
		return nil
	}
	note := displayNote(sa, cr, false)
	if lifecycleStateUnchanged(authID, false, note) {
		return nil
	}
	phys, err := hostAuthGetPhysical(authIndex)
	name := authFileNameFor(sa)
	path := ""
	legacyPath := ""
	if err == nil {
		name, path, legacyPath = resolveAuthFileTarget(sa, phys)
	}
	raw, err := buildAuthFileJSON(physJSON(phys), sa, false, note, nil)
	if err != nil {
		return err
	}
	if err := hostAuthPersistMigrate(name, path, legacyPath, raw); err != nil {
		return err
	}
	rememberLifecycleState(authID, false, note)
	accountCache.Delete(authID)
	return nil
}

// deleteAuth removes Global exhausted credentials from disk.
func deleteAuth(authIndex, authID string, sa *storedAuth) error {
	mu := checkinLockFor(authIndex)
	mu.Lock()
	defer mu.Unlock()

	phys, err := hostAuthGetPhysical(authIndex)
	if err != nil {
		return err
	}
	path := strings.TrimSpace(phys.Path)
	if path == "" {
		// Try to reconstruct path from peer WorkBuddy files' directory + canonical name.
		name := authFileNameFor(sa)
		if phys.Name != "" && !isLegacyWorkbuddyAuthName(phys.Name) {
			name = phys.Name
		} else if strings.TrimSpace(phys.Name) != "" && isLegacyWorkbuddyAuthName(phys.Name) {
			name = authFileNameFor(sa)
		}
		if dir := peerAuthDir(); dir != "" && name != "" {
			candidate := filepath.Join(dir, name)
			if isSafeWorkbuddyAuthPath(candidate) {
				path = candidate
			}
		}
	}
	if path == "" {
		// Last resort: disable instead of silent no-op (never invent a random path).
		note := displayNote(sa, nil, true) + " · 应删除但无 path"
		raw, berr := buildAuthFileJSON(physJSON(phys), sa, true, note, nil)
		if berr != nil {
			return fmt.Errorf("no path and build failed: %w", berr)
		}
		name := authFileNameFor(sa)
		if phys.Name != "" && !isLegacyWorkbuddyAuthName(phys.Name) {
			name = phys.Name
		}
		if err := hostAuthPersistMigrate(name, "", "", raw); err != nil {
			return err
		}
		rememberLifecycleState(authID, true, note)
		accountCache.Delete(authID)
		clearActiveAuthIfMatch(authID)
		return nil
	}
	if err := deleteAuthFileInDir(path, filepath.Dir(path)); err != nil {
		return err
	}
	// Also remove legacy workbuddy.json if this UID was dual-named historically.
	if sa != nil && strings.TrimSpace(sa.Account.UID) != "" {
		if dir := filepath.Dir(path); dir != "" {
			legacy := filepath.Join(dir, authFileName)
			if legacyRaw, readErr := os.ReadFile(legacy); readErr == nil && shouldDeleteLegacyForUID(legacyRaw, sa.Account.UID) {
				_ = deleteAuthFileInDir(legacy, dir)
			}
		}
	}
	lifecycleState.Delete(authID)
	accountCache.Delete(authID)
	clearActiveAuthIfMatch(authID)
	return nil
}

// peerAuthDir returns the directory of any WorkBuddy auth file known to the host.
// Uses HostAuthFileEntry.Path from the list response (A-38: was N+1 — list + getPhysical per file).
func peerAuthDir() string {
	files, err := hostAuthList()
	if err != nil {
		return ""
	}
	for _, f := range files {
		p := strings.TrimSpace(f.Path)
		if p != "" {
			return filepath.Dir(p)
		}
	}
	return ""
}

// syncAuthNote writes note without changing disabled state.
//
// A note written for a fault reason (session dead / 11140) is preserved: the
// credit-driven refresh must not overwrite it, because reenableBlockedByNote
// reads that wording back to keep the account disabled. Without this, an
// account disabled for a fault would have its note replaced by an ordinary
// credits line on the next reconcile tick, and the credit-driven re-enable
// would then put a credential upstream still rejects back into rotation.
func syncAuthNote(authIndex, authID string, sa *storedAuth, cr *creditsSummary, disabled bool) error {
	if sa == nil {
		return nil
	}
	note := displayNote(sa, cr, disabled)
	if lifecycleStateUnchanged(authID, disabled, note) {
		return nil
	}
	mu := checkinLockFor(authIndex)
	mu.Lock()
	defer mu.Unlock()
	phys, err := hostAuthGetPhysical(authIndex)
	name := authFileNameFor(sa)
	path := ""
	legacyPath := ""
	if err == nil {
		name, path, legacyPath = resolveAuthFileTarget(sa, phys)
		// re-read disabled from disk as source of truth
		disabled = parseDisabledFromAuthJSON(phys.JSON)
		note = displayNote(sa, cr, disabled)
		if existing := parseNoteFromAuthJSON(phys.JSON); reenableBlockedByNote(existing) {
			// The stored note is a fault note; keep it as-is.
			note = existing
		}
	}
	if lifecycleStateUnchanged(authID, disabled, note) {
		return nil
	}
	raw, err := buildAuthFileJSON(physJSON(phys), sa, disabled, note, nil)
	if err != nil {
		return err
	}
	if err := hostAuthPersistMigrate(name, path, legacyPath, raw); err != nil {
		return err
	}
	rememberLifecycleState(authID, disabled, note)
	return nil
}

func creditsErrorsBlockLifecycle(errs []string) bool {
	for _, message := range errs {
		if strings.HasPrefix(message, "credits:") {
			return true
		}
	}
	return false
}

// reenableBlockedByFaultNote reports whether a disabled auth carries a fault
// note written by the executor-error path (session dead / 11140). The file name
// (workbuddy-<uid>.json) is deliberately not part of the check: keepalive.go's
// markSessionDead writes the same note wording on the daily refresh path, and
// both mean "re-login required", so both must block the credit-driven re-enable.
//
// phys is the caller's already-fetched record. Tests stub only the bundle hook
// (see enterprise_test), so a nil record is re-read through that same seam
// rather than a second host RPC path.
func reenableBlockedByFaultNote(phys *hostAuthPhysical, authIndex string) bool {
	raw := physJSON(phys)
	if len(raw) == 0 {
		_, fresh, err := reconcileHostAuthGetBundle(authIndex)
		if err != nil {
			return false
		}
		raw = physJSON(fresh)
	}
	return reenableBlockedByNote(parseNoteFromAuthJSON(raw))
}

// reconcileOneAccount refreshes credits and applies lifecycle for one auth.
// authIndex is used for host RPC (host.auth.get), authID (auth.ID) is used
// for cache keys (accountCache/lifecycleState) so it matches the scheduler's
// SchedulerAuthCandidate.ID.
// force ignores short-circuit only for credit fetch (uses force on cache via caller).
func reconcileOneAccount(authIndex, authID string, force bool) (action lifecycleAction, err error) {
	return reconcileOneAccountWithCallback(authIndex, authID, force, "")
}

func reconcileOneAccountWithCallback(authIndex, authID string, force bool, callbackID string) (action lifecycleAction, err error) {
	if !lifecycleEnabled() {
		return lifecycleNone, nil
	}
	// Single host.auth.get (A-19): previous hostAuthGet + hostAuthGetPhysical
	// doubled RPC on every reconcile tick (21 accounts × 2).
	sa, phys, err := reconcileHostAuthGetBundle(authIndex)
	if err != nil {
		return lifecycleNone, err
	}
	disabled := false
	if phys != nil {
		disabled = phys.Disabled
	}

	// Credits: use force path via fetchUserResource always when force,
	// else try cache first.
	var cr *creditsSummary
	if !force {
		if v, ok := accountCache.Load(authID); ok {
			if e, ok2 := v.(*accountCacheEntry); ok2 && e.credits != nil && time.Since(e.fetched) < accountCacheTTL {
				cr = e.credits
			}
		}
	}
	if cr == nil {
		// Route credits fetch through cachedAccountDetails so singleflight
		// serializes concurrent writers for the same authID (P0-2 fix: the
		// previous Load→Store sequence here had a check-then-act window
		// where a concurrent dashboard cachedAccountDetails write could
		// overwrite our merge with newer plan/checkin values).
		_, _, cr2, errs := cachedAccountDetailsWithCallback(authID, sa, true, callbackID)
		if creditsErrorsBlockLifecycle(errs) {
			return lifecycleNone, nil
		}
		cr = cr2
		if cr == nil {
			return lifecycleNone, nil
		}
	}

	region := accountRegion(sa)
	// Enterprise quota resets per cycle and is admin-managed; lifecycle
	// actions would strand a working account. Refresh the note only.
	if isEnterpriseAccount(sa) {
		_ = syncAuthNote(authIndex, authID, sa, cr, disabled)
		return lifecycleNone, nil
	}
	if region == "cn" && disabled {
		// A fault disable (session dead / 11140 ban) must not be undone just
		// because credits look healthy: upstream still rejects requests until
		// the user logs in again. Credit-driven re-enable only applies to
		// disables this plugin made for exhaustion.
		if reenableBlockedByFaultNote(phys, authIndex) {
			_ = syncAuthNote(authIndex, authID, sa, cr, true)
			return lifecycleNone, nil
		}
		if shouldReenableCN(true, cr) {
			if err := reenableAuth(authIndex, authID, sa, cr); err != nil {
				return lifecycleReenable, err
			}
			return lifecycleReenable, nil
		}
		// still disabled: refresh note
		_ = syncAuthNote(authIndex, authID, sa, cr, true)
		return lifecycleNone, nil
	}

	act := lifecycleActionFor(region, cr)
	switch act {
	case lifecycleDelete:
		// P1-4: confirm before deleting a Global account — a transient 402
		// from the upstream billing API could otherwise cause an irreversible
		// delete. Re-fetch credits once more; only proceed if still exhausted.
		cr2, err2 := fetchUserResourceWithCallback(sa, callbackID)
		if err2 != nil || !isCreditsExhausted(cr2) {
			// Credits may have recovered (or fetch failed) — don't delete.
			return lifecycleNone, nil
		}
		return lifecycleDelete, deleteAuth(authIndex, authID, sa)
	case lifecycleDisable:
		return lifecycleDisable, disableAuth(authIndex, authID, sa, cr, "耗尽")
	default:
		// healthy: keep note fresh (throttled)
		_ = syncAuthNote(authIndex, authID, sa, cr, false)
		return lifecycleNone, nil
	}
}

// reconcileAllAccountsWithCallback walks WorkBuddy auths and applies lifecycle.
func reconcileAllAccountsWithCallback(force bool, callbackID string) []map[string]any {
	if !lifecycleEnabled() {
		return nil
	}
	files, err := hostAuthList()
	if err != nil {
		return []map[string]any{{"error": err.Error()}}
	}
	out := make([]map[string]any, 0, len(files))
	for _, f := range files {
		act, err := reconcileOneAccountWithCallback(f.AuthIndex, f.ID, force, callbackID)
		row := map[string]any{"auth_index": f.AuthIndex, "action": act.String()}
		if err != nil {
			row["error"] = err.Error()
		}
		if act != lifecycleNone || err != nil {
			out = append(out, row)
		}
	}
	return out
}

// reconcileAfterExecutorError applies the lifecycle effect of one upstream
// failure. AuthID from the executor may be the credential ID (UID) rather than
// runtime auth_index; we resolve via host.auth.list when direct get fails.
//
// The dispatch is driven by classifyUpstreamError, not by "is it 429 / is it
// credit" boolean tests: content-firewall false positives and malformed
// outbound bodies score as content_blocked / bad_params and are ignored here
// (they say nothing about the credential), model-level 6004 throttling is
// logged as model-scoped instead of being read as account exhaustion, and only
// hard credit plus account-level faults reach a reconcile. Both wrappers stay
// no-ops when lifecycle_auto is off.
func reconcileAfterExecutorError(authID string, status int, body string) {
	if !lifecycleEnabled() || strings.TrimSpace(authID) == "" {
		return
	}
	applyExecutorErrorEffect(authID, classifyUpstreamError(status, body), status, body)
}

// applyExecutorErrorEffect is the single dispatch point for classified upstream
// failures. Split out from reconcileAfterExecutorError so tests can drive each
// class directly — the classification is pure, the effect is what must not
// regress.
func applyExecutorErrorEffect(authID string, kind upstreamErrKind, status int, body string) {
	effect := executorErrorEffectFor(kind)
	if effect == executorEffectIgnore {
		// Logged (not silent) so the panel-side story for "why is this account
		// still in rotation after an error" is answerable without a debugger.
		// The level is info, not debug: production runs with debug off, so a
		// debug line the comment promises would be invisible exactly when a
		// request-level error needs explaining. Throttled to one line per
		// (status, class) so a client retrying a too-long prompt cannot flood.
		logUpstreamClassification(status, kind, authID)
		return
	}
	if upstreamErrKindIsCredits(kind) {
		// Credit-driven path keeps the original semantics exactly: no log
		// line here, so an upstream 402 storm cannot turn into a log storm,
		// and reconcileOneAccount re-fetches credits itself (force=true).
		go executorErrorDispatchHooks.reconcile(authID)
		return
	}
	go executorErrorDispatchHooks.fault(authID, kind, status, body)
}

// upstreamErrKindIsCredits reports whether a kind is handled by the unchanged
// credit-driven reconcile path.
func upstreamErrKindIsCredits(kind upstreamErrKind) bool {
	return kind == upstreamErrHardCredit
}

// classificationLogDedup throttles the ignore-class log to one line per
// (status, class) pair. A request-level error (too-long prompt, blocked
// content) is per-request and a retrying client repeats it; the interesting
// fact is which class it maps to and that the account was left alone — not how
// many times it happened.
var classificationLogDedup sync.Map

func logUpstreamClassification(status int, kind upstreamErrKind, authID string) {
	key := fmt.Sprintf("%d|%s", status, kind.String())
	if _, seen := classificationLogDedup.LoadOrStore(key, struct{}{}); seen {
		return
	}
	hostLogf("info", fmt.Sprintf("workbuddy upstream %d classified as %s (account untouched, auth=%s)",
		status, kind.String(), shortUID(authID)))
}

// reconcileAfterExecutorErrorAsync resolves an executor AuthID to host indices
// and re-applies credit lifecycle.
func reconcileAfterExecutorErrorAsync(authID string) {
	idx, id := resolveAuthIndexAndID(authID)
	if idx == "" {
		return
	}
	_, _ = reconcileOneAccount(idx, id, true)
}

// applyFaultLifecycleEffect implements the non-credit effects:
//   - session dead / 11140: disable the credential so the host stops routing to
//     it. Neither self-heals (the offline session is revoked; a 11140 ban is an
//     authorization verdict), so leaving them enabled only burns requests. The
//     disabled note carries a re-login hint, which reenableBlockedByNote reads
//     back to stop the credits-driven re-enable from flapping the account.
//   - 14017: soft cooldown. Deliberately NO disable — completing the
//     registration can still activate the account, and a disable would leave it
//     unusable after the user fixed it.
//   - 6004: model-level throttle. The account is healthy, so this only records
//     which model was throttled and until when, for the log.
func applyFaultLifecycleEffect(authID string, kind upstreamErrKind, status int, body string) {
	idx, id := resolveAuthIndexAndID(authID)
	if idx == "" {
		hostLogf("warn", fmt.Sprintf("workbuddy %s: auth %q not resolvable, fault effect skipped", kind.String(), shortUID(authID)))
		return
	}
	switch kind {
	case upstreamErrSessionDead, upstreamErrAccountBanned, upstreamErrTokenRejected:
		sa, _, err := reconcileHostAuthGetBundle(idx)
		if err != nil || sa == nil {
			hostLogf("warn", fmt.Sprintf("workbuddy %s: auth bundle for %s unreadable: %v", kind.String(), shortUID(authID), err))
			return
		}
		// No early return for an already-disabled file: the host disables a
		// credential as soon as it sees the refusal, and returning here would
		// leave the host's generic note in place instead of the re-login hint
		// the panel and reenableBlockedByNote depend on.
		// Credits are intentionally NOT fetched: this is a fault disable, and
		// passing nil keeps the note free of credit wording that would read as
		// an exhaustion disable on the panel.
		//
		// Known gap: the reconcile tick's note refresh (syncAuthNote) rewrites
		// this note from credits state while the account stays disabled, which
		// drops the re-login hint and lets the credit-driven re-enable take it
		// back once credits look fine. Closing that needs a persisted fault
		// marker in the auth file, which is not part of this change.
		reason := upstreamFaultNote(kind)
		if err := disableAuth(idx, id, sa, nil, reason); err != nil {
			hostLogf("warn", fmt.Sprintf("workbuddy %s on %s: disable failed: %v", kind.String(), shortUID(authID), err))
			return
		}
		hostLogf("warn", fmt.Sprintf("workbuddy %s on auth=%s (http %d): disabled until re-login", kind.String(), shortUID(authID), status))
	case upstreamErrAccountNotActivated:
		hostLogf("debug", fmt.Sprintf("workbuddy 14017 trial not activated on auth=%s (http %d): soft cooldown, account left enabled",
			shortUID(authID), status))
	case upstreamErrModelRateLimit:
		message := fmt.Sprintf("workbuddy 6004 model rate limit on auth=%s (http %d): account healthy, model throttled",
			shortUID(authID), status)
		if resetAt, ok := parseSoftRateReset(body); ok {
			message += ", resets at " + resetAt.Format(softRateTimeLayout) + " UTC+8"
		}
		hostLogf("debug", message)
	}
}

// reconcileByUID finds WorkBuddy auth by account UID and applies executor-error lifecycle.
func reconcileByUID(uid string, status int, body string) {
	uid = strings.TrimSpace(uid)
	if uid == "" || !lifecycleEnabled() {
		return
	}
	// Same dispatch as the sync executor path: the streaming path sees the same
	// upstream bodies and must classify them identically.
	applyExecutorErrorEffect(uid, classifyUpstreamError(status, body), status, body)
}

// executorErrorDispatchHooks are indirections for the two goroutine bodies in
// applyExecutorErrorEffect. Tests swap them to observe which effect a class
// produced, since neither the credit reconcile nor the fault disable can run
// without a live host (host.auth.* needs the cgo function table).
var executorErrorDispatchHooks = struct {
	reconcile func(authID string)
	fault     func(authID string, kind upstreamErrKind, status int, body string)
}{
	reconcile: reconcileAfterExecutorErrorAsync,
	fault:     applyFaultLifecycleEffect,
}

// resolveAuthIndexAndID maps executor AuthID (index, file id, or account UID)
// to host auth_index AND auth.ID. Returns ("", "") if not found.
func resolveAuthIndexAndID(authID string) (string, string) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return "", ""
	}
	// Fast path: already an auth_index the host understands.
	if _, err := hostAuthGet(authID); err == nil {
		// Find the matching file entry to get auth.ID.
		if files, err := hostAuthList(); err == nil {
			for _, f := range files {
				if f.AuthIndex == authID {
					return authID, f.ID
				}
			}
		}
		return authID, ""
	}
	files, err := hostAuthList()
	if err != nil {
		return "", ""
	}
	// Prefer O(list) name/id match before per-account host.auth.get (A-22).
	// Multi-account files are workbuddy-<uid>.json; list Name/ID usually carry that.
	wantName := "workbuddy-" + authID + ".json"
	for _, f := range files {
		if f.AuthIndex == authID || f.ID == authID || f.Name == authID {
			return f.AuthIndex, f.ID
		}
		if listEntryMatchesUID(f, authID, wantName) {
			return f.AuthIndex, f.ID
		}
	}
	// Slow path: only when list metadata lacks uid (rare legacy shapes).
	for _, f := range files {
		sa, err := hostAuthGet(f.AuthIndex)
		if err != nil {
			continue
		}
		if strings.TrimSpace(sa.Account.UID) == authID {
			return f.AuthIndex, f.ID
		}
	}
	return "", ""
}

// invalidateAccountCredits drops cached credits so the next panel/reconcile
// fetch hits upstream. Call after a successful chat completion — otherwise a
// short TTL cache makes "used" look frozen while the user is burning credits.
func invalidateAccountCredits(authID, authUID string) {
	// Invalidate credits only — keep plan/checkin in cache.
	invalidateCredits := func(id string) {
		if v, ok := accountCache.Load(id); ok {
			if e, ok2 := v.(*accountCacheEntry); ok2 {
				fresh := *e
				fresh.credits = nil
				fresh.fetched = time.Now()
				accountCache.Store(id, &fresh)
			}
		}
	}
	if authID != "" {
		invalidateCredits(authID)
	}
	if authUID == "" || authUID == authID {
		return
	}
	// Also drop any cache keyed by auth_index that maps to this UID.
	files, err := hostAuthList()
	if err != nil {
		return
	}
	wantName := "workbuddy-" + authUID + ".json"
	matchedByName := false
	for _, f := range files {
		if f.AuthIndex == authID || f.ID == authID || f.Name == authID {
			invalidateCredits(f.ID)
			continue
		}
		if listEntryMatchesUID(f, authUID, wantName) {
			invalidateCredits(f.ID)
			matchedByName = true
		}
	}
	if matchedByName {
		return
	}
	// Slow path: legacy names without uid in list metadata.
	for _, f := range files {
		if f.AuthIndex == authID {
			continue
		}
		sa, err := hostAuthGet(f.AuthIndex)
		if err != nil {
			continue
		}
		if strings.TrimSpace(sa.Account.UID) == authUID {
			invalidateCredits(f.ID)
		}
	}
}

// listEntryMatchesUID reports whether host list metadata already encodes the UID
// (workbuddy-<uid>.json naming). Pure helper for O(list) cache invalidation.
func listEntryMatchesUID(f pluginapi.HostAuthFileEntry, uid, wantName string) bool {
	if uid == "" {
		return false
	}
	if strings.EqualFold(f.Name, wantName) || strings.EqualFold(f.ID, wantName) {
		return true
	}
	base := strings.TrimSuffix(f.Name, ".json")
	return strings.EqualFold(base, "workbuddy-"+uid)
}

// enrichAuthMetadata builds Metadata map for AuthData (type/logo/note/disabled).
func enrichAuthMetadata(sa *storedAuth, cr *creditsSummary, disabled bool) map[string]any {
	note := displayNote(sa, cr, disabled)
	return map[string]any{
		"type":     providerName,
		"provider": providerName,
		"logo":     pluginLogoURL,
		"note":     note,
		"disabled": disabled,
	}
}
