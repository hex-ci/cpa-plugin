// policy.go is the pure decision layer for credit-driven lifecycle actions:
// given an account's region and current credits, decide whether to disable
// CN: disable when exhausted, re-enable after check-in restores credits, or
// leave it alone. No I/O happens here — reconcileOneAccount consumes these
// decisions and applies them via the lifecycle.go authfile helpers.
package main

import (
	"fmt"
	"strings"
)

// lifecycleAction is the policy decision for one account.
type lifecycleAction int

const (
	lifecycleNone lifecycleAction = iota
	lifecycleDisable
	lifecycleDelete
	lifecycleReenable
)

func (a lifecycleAction) String() string {
	switch a {
	case lifecycleDisable:
		return "disable"
	case lifecycleDelete:
		return "delete"
	case lifecycleReenable:
		return "reenable"
	default:
		return "none"
	}
}

// lifecycleEnabled reports whether automatic disable/reenable is on
// (config key lifecycle_auto, default true).
func lifecycleEnabled() bool { return currentConfig().LifecycleAuto }

// shouldActOnCredits is true only when credits are *known* exhausted.
// nil / empty (no packages, no used) is unknown → false.
func shouldActOnCredits(cr *creditsSummary) bool {
	return isCreditsExhausted(cr)
}

// hardCreditMarkers are case-insensitive substrings in upstream error bodies.
var hardCreditMarkers = []string{
	"insufficient credit",
	"insufficient credits",
	"no credit",
	"no credits",
	"credit exhausted",
	"credits exhausted",
	"out of credit",
	"out of credits",
	"quota exceeded",
	"quota exhaust",
	"payment required",
	"积分不足",
	"额度不足",
	"余额不足",
	"积分用完",
	"额度用尽",
	"没有积分",
	"credit not enough",
	"not enough credit",
}

// isHardCreditError reports business "out of credits" style failures.
// 402 is treated as payment/credit. Pure 429 is not hard unless body has credit markers.
func isHardCreditError(status int, body string) bool {
	if status == httpStatusPaymentRequired {
		return true
	}
	lower := strings.ToLower(body)
	for _, m := range hardCreditMarkers {
		if strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	// Chinese markers may not lower-map usefully; also scan raw.
	for _, m := range hardCreditMarkers {
		if strings.Contains(body, m) {
			return true
		}
	}
	return false
}

const httpStatusPaymentRequired = 402

// isSoftRateLimit is pure throttling without hard-credit semantics.
func isSoftRateLimit(status int, body string) bool {
	if isHardCreditError(status, body) {
		return false
	}
	if status == 429 {
		return true
	}
	lower := strings.ToLower(body)
	return strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "too many requests") ||
		strings.Contains(lower, "throttl")
}

// lifecycleActionFor chooses disable/none from credits.
// Disable (not delete) so a check-in can restore credits without forcing the
// user to re-import a credential.
//
// An account that still has an enabled free model keeps serving traffic with
// zero credits — the normal state of a CN free account — so exhausting paid
// credits must not take it out of rotation.
func lifecycleActionFor(sa *storedAuth, cr *creditsSummary) lifecycleAction {
	if !shouldActOnCredits(cr) {
		return lifecycleNone
	}
	if hasEnabledFreeModel(sa) {
		return lifecycleNone
	}
	return lifecycleDisable
}

// hasEnabledFreeModel reports whether this credential's catalog offers at least
// one enabled, zero-price model. Unknown catalogs report false so a genuinely
// exhausted paid account is still disabled.
//
// It is per credential on purpose: entitlements differ inside one realm, and a
// realm-wide answer would keep an exhausted paid account in rotation just
// because a free account exists in the same region.
func hasEnabledFreeModel(sa *storedAuth) bool {
	entries, _ := catalogStateFor(sa)
	for _, e := range entries {
		if e.Enable && (e.IsFree || e.PriceFactor == 0) {
			return true
		}
	}
	return false
}

// shouldReenable is true when a disabled account has spendable credits again,
// or still owns an enabled free model.
func shouldReenable(disabled bool, cr *creditsSummary, sa *storedAuth) bool {
	if !disabled {
		return false
	}
	if cr == nil {
		return false
	}
	if hasEnabledFreeModel(sa) {
		return true
	}
	if isCreditsExhausted(cr) {
		return false
	}
	// Known positive remain, or non-exhausted with packages still having room.
	return cr.TotalRemain > 0
}

// displayNote builds a one-line note for CPAMP Auth cards.
// isPluginNote reports whether a stored note was written by displayNote. Only
// those are safe to carry through a parse-time refresh, which has no credits
// snapshot: carrying a host-written note would freeze a stale string, while
// regenerating one would replace a real balance with "积分未知".
func isPluginNote(note string) bool {
	note = strings.TrimSpace(note)
	return strings.HasPrefix(note, "CN · ") || strings.HasPrefix(note, "Global · ")
}

func displayNote(sa *storedAuth, cr *creditsSummary, disabled bool) string {
	region := "CN"
	if realmOf(sa).ID == realmGlobal {
		region = "Global"
	}
	parts := []string{region}
	if disabled {
		parts = append(parts, "已禁用")
	}
	switch {
	case cr == nil:
		parts = append(parts, "积分未知")
	case isCreditsExhausted(cr):
		parts = append(parts, fmt.Sprintf("耗尽 · 余%d 已用%d", cr.TotalRemain, cr.TotalUsed))
	default:
		// Show remain as primary (what you can still spend). Used is real cycle spend.
		// Size (capacity) grows with check-in packs — do not treat size↑ as usage↓.
		if cr.TotalSize > 0 {
			parts = append(parts, fmt.Sprintf("余%d 已用%d 池%d", cr.TotalRemain, cr.TotalUsed, cr.TotalSize))
		} else {
			parts = append(parts, fmt.Sprintf("余%d 已用%d", cr.TotalRemain, cr.TotalUsed))
		}
	}
	note := strings.Join(parts, " · ")
	if len(note) > 80 {
		note = note[:77] + "..."
	}
	return note
}

// labelForAuth adds [CN] for host labels.
func labelForAuth(sa *storedAuth) string {
	base := "Qoder"
	if sa != nil && strings.TrimSpace(sa.Account.Nickname) != "" {
		base = strings.TrimSpace(sa.Account.Nickname)
	}
	tag := "CN"
	if realmOf(sa).ID == realmGlobal {
		tag = "Global"
	}
	return base + " [" + tag + "]"
}
