// policy.go is the pure decision layer for credit-driven lifecycle actions:
// given an account's region and current credits, decide whether to disable
// (CN), delete (Global), re-enable (CN after check-in restores credits), or
// leave it alone. No I/O happens here — reconcileOneAccount consumes these
// decisions and applies them via the lifecycle.go authfile helpers.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
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

// lifecycleAuto gates automatic disable/delete/reenable. Default true.
var (
	lifecycleAuto   = true
	lifecycleAutoMu sync.RWMutex
)

func lifecycleEnabled() bool {
	lifecycleAutoMu.RLock()
	defer lifecycleAutoMu.RUnlock()
	return lifecycleAuto
}

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
	"额度已用尽",
	"没有积分",
	"credit not enough",
	"not enough credit",
}

// isHardCreditError reports business "out of credits" style failures.
// 402 is treated as payment/credit. A 429 never qualifies: upstream attaches
// quota/credit wording to throttling bodies, and the status code is the
// stronger signal (see classifyUpstreamError, which probes 429 first).
func isHardCreditError(status int, body string) bool {
	if status == httpStatusPaymentRequired {
		return true
	}
	if status == 429 {
		return false
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
// Kept for compatibility with callers/tests that only need the coarse
// question; new code asks classifyUpstreamError, which separates model-level
// 6004 throttling (account healthy) and account-level faults from this kind.
func isSoftRateLimit(status int, body string) bool {
	if isHardCreditError(status, body) {
		return false
	}
	if status == 429 {
		return true
	}
	return bodyHasMarker(body, softRateMarkers)
}

// -----------------------------------------------------------------------------
// Precise upstream classification
// -----------------------------------------------------------------------------

// upstreamErrKind is the precise class of one upstream failure.
//
// The plugin runs inside the host executor: there is no "try the next account
// inside the same request" loop, so a misclassification cannot be corrected by
// rotation — its only effects are the auth lifecycle (disable / delete /
// re-enable) and what the panel reports. That asymmetry is why the class must
// distinguish "this account is broken" from "this request / this model is
// broken": only the former may touch the stored credential.
type upstreamErrKind int

const (
	upstreamErrNone upstreamErrKind = iota
	// upstreamErrHardCredit: credits/quota spent — the account cannot serve
	// again until a top-up or a check-in refill.
	upstreamErrHardCredit
	// upstreamErrSessionDead: the server-side offline session is gone; only a
	// fresh OAuth login revives the credential.
	upstreamErrSessionDead
	// upstreamErrAccountBanned: upstream refuses this account itself (11140
	// "request illegal", an authorization risk-control verdict). It does not
	// heal on its own — the operator must log in again.
	upstreamErrAccountBanned
	// upstreamErrAccountNotActivated: 14017 "trial not activated" — the
	// registration flow was never finished. Completing it can still fix the
	// account, so this must never disable the credential.
	upstreamErrAccountNotActivated
	// upstreamErrModelRateLimit: 6004 — this *model* is throttled until the
	// reset time in the body. The account is healthy; switching models works.
	upstreamErrModelRateLimit
	// upstreamErrSoftRate: request-rate/usage throttling without a model code,
	// including bodies that carry the throttle wording on non-429 statuses.
	upstreamErrSoftRate
	// upstreamErrContentBlocked: the content firewall rejected the payload
	// (11128 "unapproved channel" / "blocked by security policy"). A false
	// positive on legitimate traffic — never an account problem.
	upstreamErrContentBlocked
	// upstreamErrBadParams: the outbound body itself is malformed
	// (11101 / "Unmarshal chat params failed"). Re-sending it to another
	// account changes nothing.
	upstreamErrBadParams
	// upstreamErrModelBlocked: 11102 "service info not found" — this backend
	// does not serve that model. The account is healthy; only the (account,
	// model) pair is unusable, so retrying the same model on the same account
	// just repeats the failure.
	upstreamErrModelBlocked
	// upstreamErrPromptTooLong: 11115 "prompt is too long" — the request
	// exceeds the model's context window. A request-level problem: any account
	// rejects the same body, so rotating or punishing the credential would
	// discard healthy accounts for nothing.
	upstreamErrPromptTooLong
	// upstreamErrServer: 5xx — upstream fault.
	upstreamErrServer
	// upstreamErrOther: any other 4xx / business error.
	upstreamErrOther
)

func (k upstreamErrKind) String() string {
	switch k {
	case upstreamErrHardCredit:
		return "hard_credit"
	case upstreamErrSessionDead:
		return "session_dead"
	case upstreamErrAccountBanned:
		return "account_banned"
	case upstreamErrAccountNotActivated:
		return "account_not_activated"
	case upstreamErrModelRateLimit:
		return "model_rate_limit"
	case upstreamErrSoftRate:
		return "soft_rate"
	case upstreamErrContentBlocked:
		return "content_blocked"
	case upstreamErrBadParams:
		return "bad_params"
	case upstreamErrModelBlocked:
		return "model_blocked"
	case upstreamErrPromptTooLong:
		return "prompt_too_long"
	case upstreamErrServer:
		return "server"
	case upstreamErrOther:
		return "other"
	default:
		return "none"
	}
}

// bodyHasMarker matches markers case-insensitively (lowercased body + raw body
// channels, so Chinese markers that do not lower-map still match). Lists stay
// short and phrase-shaped on purpose: a bare word like "limit" or a status
// code alone is too weak to decide whether an account is at fault.
func bodyHasMarker(body string, markers []string) bool {
	if body == "" {
		return false
	}
	lower := strings.ToLower(body)
	for _, m := range markers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return true
		}
	}
	return false
}

// softRateMarkers: throttle wording. Hyphenated forms are listed separately
// because Contains does not cross '-'. "too many" also catches client-side
// "too many tokens", which in this plugin costs nothing — soft rate maps to a
// log only, never to a credential change.
var softRateMarkers = []string{
	"rate limit",
	"rate-limiting",
	"rate-limited",
	"too many requests",
	"too many",
	"usage limit",
	"throttl",
	"请求过于频繁",
	"限流",
}

// sessionDead detection reuses keepalive.go's sessionDeadMarkers/isSessionDeadError
// on purpose: the data plane and the daily refresh path must agree on what a
// revoked offline session looks like, or one path re-enables what the other
// just disabled.

// accountBannedMarkers: 11140 is overloaded — it also carries the "model
// provider is rate-limiting requests" text, which means the account is fine
// and only that model is throttled. Matching the code alone would ban a
// healthy credential, so only the authorization wording counts.
var accountBannedMarkers = []string{
	"request illegal",
}

// accountNotActivatedMarkers: 14017 wording is unique (no throttling
// ambiguity), so text plus the business code are both safe to match.
var accountNotActivatedMarkers = []string{
	"trial not activated",
	"trial version is not yet activated",
}

// contentBlockedMarkers: upstream audits payloads by literal fingerprint, so
// ordinary templates (Claude Code / Codex injected instructions) get rejected
// as "illegal api invocation". 11128 is the audit business code seen alongside
// these messages.
var contentBlockedMarkers = []string{
	"unapproved channel",
	"blocked by security policy",
	"illegal api invocation",
	"11128",
}

// badParamsMarkers: the request body the plugin sent is malformed.
var badParamsMarkers = []string{
	"Unmarshal chat params failed",
}

// upstreamBizCodeRe matches a business code in an envelope. JSON spacing and a
// quoted value both occur in real responses ("code":6004 / "code": 6004 /
// "code":"6004"), and the code carries no whitespace tolerance from the host
// bridge (bodies are forwarded verbatim).
func upstreamBizCodeRe(code string) *regexp.Regexp {
	return regexp.MustCompile(`"code"\s*:\s*"?` + code + `"?`)
}

var (
	modelRateLimitCodeRe  = upstreamBizCodeRe(modelRateLimitCode)
	badParamsCodeRe       = upstreamBizCodeRe("11101")
	accountNotActivatedRe = upstreamBizCodeRe("14017")
	// upstreamBusyCodeRe matches the "model provider is temporarily
	// unavailable" envelope. It is matched by CODE, not by its wording: the
	// wording is the tail of the body, and the host truncates bodies (the
	// production error table stores this one cut mid-word at
	// "rate_limit_exceede"), so a wording match classifies the same failure
	// differently depending on where the cut lands — measured: the full body
	// scores soft_rate, the truncated one scores server. The short code
	// survives truncation because it sits near the head.
	upstreamBusyCodeRe = upstreamBizCodeRe(upstreamBusyCode)
	// capacityQueueCodeRe matches the capacity-queue codes the upstream emits
	// when a model has no free slot. The client treats them as a waiting
	// state, not a failure: it polls /v2/chat/queue/status and resends the
	// same request once the slot frees. They therefore belong to the
	// throttling class — the credential and the account are healthy.
	capacityQueueCodeRe = regexp.MustCompile(`"code"\s*:\s*"?602[012]\b`)
)

// modelRateLimitCode is the business code for "this model hit its usage cap",
// i.e. the message carrying「将在 … 重置」.
const modelRateLimitCode = "6004"

// upstreamBusyCode is the business code for "the model provider is temporarily
// unavailable, please retry later" — an upstream 500 whose extError says
// rate_limit_exceeded. Semantically it is throttling (the account and the
// credential are fine; the model is busy), so it belongs to the rate-limit
// class rather than the anonymous 5xx class, which would otherwise be the
// fallthrough for status >= 500.
const upstreamBusyCode = "11134"

// isModelRateLimit reports whether the body names code 6004 — model-level
// throttling rather than an account-level limit. Used to keep 6004 out of the
// account lifecycle: the credential is healthy and the reset time in the body
// says exactly when the model frees up.
func isModelRateLimit(body string) bool {
	return modelRateLimitCodeRe.MatchString(body)
}

// softRateResetLoc: the reset time in upstream 429/6004 bodies is wall-clock
// UTC+8 regardless of the host's TZ. Parsing it in the local zone would shift
// the instant on any non-UTC+8 host, so the zone is fixed here on purpose.
var softRateResetLoc = time.FixedZone("UTC+8", 8*60*60)

// softRateResetRe captures the time in 「将在 <时间> 重置」.
var softRateResetRe = regexp.MustCompile(`将在 (.+?) 重置`)

// softRateResetReEN captures the English reset form. The timestamp shape is
// anchored on purpose: global-realm bodies say "reset at the end of the day"
// in prose too, and a loose match would turn that into a bogus instant.
var softRateResetReEN = regexp.MustCompile(`(?i)reset at (\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})`)

// softRateTimeLayout is the upstream reset-time format (no zone suffix; the
// zone is fixed to UTC+8 above).
const softRateTimeLayout = "2006-01-02 15:04:05"

// parseSoftRateReset extracts the reset instant from a throttle body.
//
// The parse is deliberately NOT gated on the 6004 business code: upstream
// attaches "resets at <time>" to several throttle shapes (6004 and the 11140
// rate-limiting variant). Gating on 6004 left the others with no known
// self-heal instant, so callers fell back to an escalating cooldown that never
// aligned with the real reset. Whether the throttle is model-level is a
// separate question answered by isModelRateLimit, not by this parser.
//
// Both wording forms are accepted: the Chinese 「将在 … 重置」 and the English
// "reset at <timestamp>" used by the global realm.
func parseSoftRateReset(body string) (time.Time, bool) {
	m := softRateResetRe.FindStringSubmatch(body)
	if len(m) < 2 {
		m = softRateResetReEN.FindStringSubmatch(body)
	}
	if len(m) < 2 {
		return time.Time{}, false
	}
	ts := strings.TrimSuffix(strings.TrimSpace(m[1]), " UTC+8")
	t, err := time.ParseInLocation(softRateTimeLayout, ts, softRateResetLoc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// modelBlockedCodeRe matches 11102 "service info not found" — the backend does
// not serve that model.
//
// Matched by code or by the narrow phrase, never by a whole-body substring
// search: a body's requestId is a random hex string that can contain "11102",
// and treating that as a model block would blacklist a perfectly usable model
// for the rest of the session. Only the structured fields are read.
var modelBlockedCodeRe = upstreamBizCodeRe("11102")

var modelBlockedMsgRe = regexp.MustCompile(`(?i)"msg"\s*:\s*"[^"]*service info not found`)

// promptTooLongCodeRe matches 11115 "prompt is too long".
var promptTooLongCodeRe = upstreamBizCodeRe("11115")

var promptTooLongMsgRe = regexp.MustCompile(`(?i)prompt is too long`)

// isModelBlocked reports the 11102 shape.
func isModelBlocked(status int, body string) bool {
	if status != 400 && status != 404 {
		return false
	}
	return modelBlockedCodeRe.MatchString(body) || modelBlockedMsgRe.MatchString(body)
}

// isPromptTooLong reports the 11115 shape. Only request-level statuses count:
// a 429 carrying this text is still a rate limit, and a 5xx is an upstream
// fault — neither is evidence that the body is too long.
func isPromptTooLong(status int, body string) bool {
	if status != 400 && status != 404 && status != 413 {
		return false
	}
	return promptTooLongCodeRe.MatchString(body) || promptTooLongMsgRe.MatchString(body)
}

// classifyUpstreamError sorts one upstream failure into exactly one kind.
//
// Order runs strict → loose; each step is above the looser ones for a reason:
//  1. session dead — a terminal credential state. A body that also mentions
//     rate limiting must not be downgraded to soft rate: throttling expires,
//     a revoked offline session does not.
//  2. account fault (11140 "request illegal" / 14017 "trial not activated") —
//     account-determined rejections. This must sit before the status==429
//     fallback: 14017 arrives with a 429 status, and classifying it as soft
//     rate would wait forever for a cooldown that never heals.
//  3. model rate limit (6004) — an explicit, structured model-level signal, so
//     it outranks the loose wording checks below. The account is healthy.
//  4. 11134 — same throttling semantics as 6004 but arriving as a 500 whose
//     explanatory text sits at the tail (see upstreamBusyCodeRe). The
//     capacity-queue codes (6020/6021/6022) are the same story: an upstream
//     state, matched by code so a truncated body still classifies.
//  5. bare 429 — BEFORE the credit wording, deliberately. Upstream attaches
//     quota/credit phrasing to throttling bodies, so wording alone would park
//     a healthy account in a hard cooldown for a condition that clears itself;
//     the status code is the stronger signal.
//  6. soft rate wording on any status — upstream returns throttle semantics on
//     200/400/403 as well, and those responses would otherwise look like
//     health problems (or like nothing at all).
//  7. hard credit (402, or credit wording on a non-429 status) — the least
//     self-healing class and the only one that may delete a Global auth. Kept
//     after 429 so throttle bodies never reach it, and backed by the unchanged
//     marker set so no real credit body regresses.
//  8. 5xx — upstream fault, unrelated to the credential.
//  9. 4xx: content firewall and malformed outbound body first, so ordinary
//     audit false positives never fall through to a class that punishes the
//     account; then everything else as "other".
func classifyUpstreamError(status int, body string) upstreamErrKind {
	if bodyHasMarker(body, sessionDeadMarkers) {
		return upstreamErrSessionDead
	}
	if accountNotActivatedRe.MatchString(body) || bodyHasMarker(body, accountNotActivatedMarkers) {
		return upstreamErrAccountNotActivated
	}
	if bodyHasMarker(body, accountBannedMarkers) {
		return upstreamErrAccountBanned
	}
	if isModelRateLimit(body) {
		return upstreamErrModelRateLimit
	}
	// 11102 / 11115 are request-level verdicts: they must sit above the loose
	// wording probes so a body that also carries throttle-ish words is still
	// read as "this request/model is the problem", not as an account signal.
	if isModelBlocked(status, body) {
		return upstreamErrModelBlocked
	}
	if isPromptTooLong(status, body) {
		return upstreamErrPromptTooLong
	}
	if upstreamBusyCodeRe.MatchString(body) {
		return upstreamErrSoftRate
	}
	if capacityQueueCodeRe.MatchString(body) {
		return upstreamErrSoftRate
	}
	// 402 before the wording probes: "payment required" is unambiguous, and a
	// 402 body that also carries throttle wording (they overlap in practice)
	// must still read as a spent balance, not as something that self-heals.
	if status == httpStatusPaymentRequired {
		return upstreamErrHardCredit
	}
	if status == 429 {
		return upstreamErrSoftRate
	}
	if bodyHasMarker(body, softRateMarkers) {
		return upstreamErrSoftRate
	}
	if isHardCreditError(status, body) {
		return upstreamErrHardCredit
	}
	if status >= 500 {
		return upstreamErrServer
	}
	if status >= 400 {
		if bodyHasMarker(body, contentBlockedMarkers) {
			return upstreamErrContentBlocked
		}
		if bodyHasMarker(body, badParamsMarkers) || badParamsCodeRe.MatchString(body) {
			return upstreamErrBadParams
		}
		return upstreamErrOther
	}
	return upstreamErrNone
}

// executorErrorEffect is the only thing a classification is allowed to do to
// the host: nothing, re-check credits, record model throttling, or disable the
// credential. The plugin has no cooldown pool, so "soft cooldown" here means
// "log it and leave the account usable".
type executorErrorEffect int

const (
	executorEffectIgnore executorErrorEffect = iota
	executorEffectRecheckCredits
	executorEffectModelRateLimit
	executorEffectSoftCooldown
	executorEffectDisableUntilRelogin
)

func executorErrorEffectFor(kind upstreamErrKind) executorErrorEffect {
	switch kind {
	case upstreamErrHardCredit:
		return executorEffectRecheckCredits
	case upstreamErrSessionDead, upstreamErrAccountBanned:
		return executorEffectDisableUntilRelogin
	case upstreamErrAccountNotActivated:
		return executorEffectSoftCooldown
	case upstreamErrModelRateLimit:
		return executorEffectModelRateLimit
	default:
		// Includes content_blocked, bad_params, server, other and plain soft
		// rate: none of them is evidence about the credential, and a wrong
		// disable costs an account the user must re-login by hand.
		return executorEffectIgnore
	}
}

// upstreamFaultNote is the disabled-note reason for account-level faults. The
// wording matters beyond display: reenableBlockedByNote() reads it back to keep
// the credit-driven re-enable from undoing a fault disable.
func upstreamFaultNote(kind upstreamErrKind) string {
	switch kind {
	case upstreamErrSessionDead:
		return "Session dead (12153): re-login required"
	case upstreamErrAccountBanned:
		return "上游封禁 (11140): re-login required"
	default:
		return "upstream account fault: re-login required"
	}
}

// faultNoteMarkers identify a disabled note written for a non-credit reason.
// All of them contain a word no credit note uses ("re-login" / "重新登录"), so
// the credit wording ("耗尽", "积分未知") can never be mistaken for a fault.
var faultNoteMarkers = []string{
	"re-login",
	"重新登录",
	"session dead",
	"12153",
	"11140",
}

// reenableBlockedByNote reports whether a disabled auth must stay disabled.
// Its credits may look healthy while upstream still rejects every request
// (banned / session dead), so the credits-driven re-enable would just flap the
// account back into rotation.
func reenableBlockedByNote(note string) bool {
	return bodyHasMarker(note, faultNoteMarkers)
}

// lifecycleActionFor chooses disable/delete/none from region + credits.
// Does not consider reenable (that needs disabled flag).
func lifecycleActionFor(region string, cr *creditsSummary) lifecycleAction {
	if !shouldActOnCredits(cr) {
		return lifecycleNone
	}
	if region == "global" {
		return lifecycleDelete
	}
	return lifecycleDisable
}

// shouldReenableCN is true when a CN account is disabled but now has credits.
func shouldReenableCN(disabled bool, cr *creditsSummary) bool {
	if !disabled {
		return false
	}
	if cr == nil {
		return false
	}
	if isCreditsExhausted(cr) {
		return false
	}
	// Known positive remain, or non-exhausted with packages still having room.
	return cr.TotalRemain > 0
}

// displayNote builds a one-line note for CPAMP Auth cards.
func displayNote(sa *storedAuth, cr *creditsSummary, disabled bool) string {
	region := strings.ToUpper(accountRegion(sa))
	if region == "CN" {
		region = "CN"
	} else {
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

// displayNameFor: enterprise JWTs carry the real given_name, which beats the
// masked signup nickname. Personal tokens have no given_name → nickname.
func displayNameFor(sa *storedAuth) string {
	if sa == nil {
		return ""
	}
	nick := strings.TrimSpace(sa.Account.Nickname)
	if !isEnterpriseAccount(sa) {
		return nick
	}
	if gn := jwtGivenName(sa.Auth.AccessToken); gn != "" {
		return gn
	}
	return nick
}

// jwtGivenName decodes the given_name claim from the access token payload.
func jwtGivenName(accessToken string) string {
	parts := strings.Split(accessToken, ".")
	if len(parts) < 2 {
		return ""
	}
	payload := parts[1]
	if pad := len(payload) % 4; pad != 0 {
		payload += strings.Repeat("=", 4-pad)
	}
	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return ""
	}
	var claims struct {
		GivenName string `json:"given_name"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return ""
	}
	return strings.TrimSpace(claims.GivenName)
}

// labelForAuth adds [CN]/[Global] for host labels.
func labelForAuth(sa *storedAuth) string {
	base := "WorkBuddy"
	if sa != nil {
		if dn := displayNameFor(sa); dn != "" {
			base = dn
		}
	}
	tag := "CN"
	if accountRegion(sa) == "global" {
		tag = "Global"
	}
	return base + " [" + tag + "]"
}
