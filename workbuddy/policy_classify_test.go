// policy_classify_test.go covers the precise upstream classification added on
// top of policy.go: model-level 6004 throttling, the 11140 / 14017 account-fault
// split, content-firewall false positives, and the mapping from a class to a
// lifecycle effect. The bodies below are copied from real upstream responses
// (JSON spacing variants included), because the whole point of the classifier is
// to tolerate what the upstream actually sends.
package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestClassifyUpstreamError(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   upstreamErrKind
	}{
		// --- hard credit: 402, or credit wording on a NON-429 status ---
		{"402 payment required", 402, `{"error":"payment required"}`, upstreamErrHardCredit},
		{"credit wording on 403", 403, `{"message":"insufficient credit"}`, upstreamErrHardCredit},
		{"chinese credit wording", 400, `{"message":"额度已用尽"}`, upstreamErrHardCredit},
		// 429 outranks the credit wording: upstream returns 429 bodies that
		// carry quota/credit phrasing while meaning "throttled, retry later".
		// Judging by the wording would park a healthy account in a ~12h hard
		// cooldown for a condition that clears itself — measured against the
		// live service, which treats the status code as the stronger signal.
		{"quota exceeded on 429 is throttle not hard", 429, `{"message":"quota exceeded"}`, upstreamErrSoftRate},
		{"credit wording on 429 is throttle not hard", 429, `{"message":"积分不足"}`, upstreamErrSoftRate},
		// 402 keeps its hard semantics regardless of any wording.
		{"402 with throttle wording stays hard", 402, `{"message":"rate limit"}`, upstreamErrHardCredit},

		// --- session dead ---
		{"offline session not found", 401, `{"msg":"Offline user session not found"}`, upstreamErrSessionDead},
		{"12153 business code", 401, `{"code":12153,"msg":"rejected"}`, upstreamErrSessionDead},
		// A gateway body that mixes session-dead with throttle wording must stay
		// session_dead: a cooldown would never revive a revoked session.
		{"session dead wins over rate wording", 401, `{"msg":"Offline user session not found","hint":"rate limit"}`, upstreamErrSessionDead},

		// --- account fault: 11140 (ban) vs 14017 (not activated) ---
		{"11140 request illegal", 403, `{"code":11140,"msg":"request illegal"}`, upstreamErrAccountBanned},
		// The 11140 envelope also carries model-level throttling; the code alone
		// must not ban a healthy account.
		{"11140 rate-limiting variant stays soft rate", 200, `{"code":11140,"msg":"The model provider is rate-limiting requests."}`, upstreamErrSoftRate},
		{"14017 by text on 429", 429, `{"code":0,"msg":"trial not activated"}`, upstreamErrAccountNotActivated},
		{"14017 by code on 429", 429, `{"code":14017,"msg":"The trial version is not yet activated"}`, upstreamErrAccountNotActivated},
		// The 14017 case that motivated the ordering rule: status 429 with the
		// trial wording must not fall through to the bare-429 soft-rate rule.
		{"14017 text beats 429 fallback", 429, `{"code":14017}`, upstreamErrAccountNotActivated},

		// --- model rate limit: 6004 ---
		{"6004 compact", 429, `{"code":6004,"msg":"将在 2026-09-15 10:00:00 重置"}`, upstreamErrModelRateLimit},
		{"6004 spaced json", 429, `{"code": 6004, "msg": "将在 2026-09-15 10:00:00 重置"}`, upstreamErrModelRateLimit},
		{"6004 quoted code", 429, `{"code":"6004","msg":"reset later"}`, upstreamErrModelRateLimit},
		// 6004 outranks the wording check even when the message says "rate limit".
		{"6004 with rate wording", 429, `{"code":6004,"msg":"rate limit until reset"}`, upstreamErrModelRateLimit},

		// --- soft rate ---
		{"bare 429", 429, `{"message":"slow down"}`, upstreamErrSoftRate},
		{"rate limit wording on 400", 400, `{"message":"rate limit exceeded"}`, upstreamErrSoftRate},
		{"throttled wording on 403", 403, `{"message":"request throttled"}`, upstreamErrSoftRate},
		{"hyphenated rate-limiting", 200, `{"msg":"The model provider is rate-limiting requests."}`, upstreamErrSoftRate},
		{"usage limit wording", 400, `{"msg":"model usage limit exceeded"}`, upstreamErrSoftRate},
		// 11134: an upstream 500 that is semantically throttling. Both captured
		// forms must land in the same class — the full body and the one the
		// host's error table stores, cut mid-word. A wording match would flip
		// the class depending on where the truncation fell; the code does not.
		{"11134 full body", 500, `upstream 500: {"code":11134,"msg":"the model provider is temporarily unavailable, please retry later or switch to another model","requestId":"75974c7804495c8c6ae79ba8fce288f9","extError":{"code":"rate_limit_exceeded","message":"too many requests"}}`, upstreamErrSoftRate},
		{"11134 truncated body", 500, `upstream 500: {"code":11134,"msg":"the model provider is temporarily unavailable, please retry later or switch to another model","requestId":"75974c7804495c8c6ae79ba8fce288f9","extError":{"code":"rate_limit_exceede`, upstreamErrSoftRate},
		{"11134 spaced json", 500, `{"code": 11134, "msg": "temporarily unavailable"}`, upstreamErrSoftRate},
		{"11134 bare code", 500, `{"code":11134}`, upstreamErrSoftRate},

		// --- capacity queue: 6020 waiting / 6021 full / 6022 per-user limit ---
		// The upstream state means "no free slot", which the client answers by
		// polling the queue and resending; the credential is untouched.
		{"6020 waiting", 200, `{"code":6020,"msg":"waiting in queue","data":{"queue_position":7,"queue_size":40,"estimated_wait":30,"retry_after":5}}`, upstreamErrSoftRate},
		{"6021 queue full", 200, `{"code":6021,"msg":"queue is full","data":{"queue_size":40}}`, upstreamErrSoftRate},
		{"6022 user limit", 503, `{"code":6022,"msg":"too many pending requests for this user"}`, upstreamErrSoftRate},
		{"6020 spaced json", 200, `{"code": 6020, "msg": "waiting"}`, upstreamErrSoftRate},
		// A queue code must not swallow longer codes that merely start the
		// same way, nor a 602x that is not part of the queue family.
		{"60201 is not a queue code", 500, `{"code":60201,"msg":"internal error"}`, upstreamErrServer},
		{"6023 is not a queue code", 500, `{"code":6023,"msg":"internal error"}`, upstreamErrServer},

		// --- content firewall: never an account problem ---
		{"11128 unapproved channel", 400, `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`, upstreamErrContentBlocked},
		{"security policy block", 400, `{"msg":"request blocked by security policy"}`, upstreamErrContentBlocked},
		{"11128 bare code", 400, `{"code":11128,"msg":"rejected"}`, upstreamErrContentBlocked},

		// --- bad params ---
		{"unmarshal chat params", 400, `{"msg":"Unmarshal chat params failed: unexpected end of JSON input"}`, upstreamErrBadParams},
		{"11101 code", 400, `{"code":11101,"msg":"bad body"}`, upstreamErrBadParams},

		// --- server / other / none ---
		{"500", 500, `{"message":"internal error"}`, upstreamErrServer},
		{"502", 502, "", upstreamErrServer},
		{"404", 404, `{"message":"not found"}`, upstreamErrOther},
		{"200 ok", 200, `{"code":0,"data":{}}`, upstreamErrNone},
		{"empty 400", 400, "", upstreamErrOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyUpstreamError(tc.status, tc.body); got != tc.want {
				t.Fatalf("classify(%d, %q) = %s, want %s", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

// TestClassifyKindStrings pins the wire/log names: they end up in host logs and
// are the only handle an operator has on a past failure.
func TestClassifyKindStrings(t *testing.T) {
	cases := []struct {
		kind upstreamErrKind
		want string
	}{
		{upstreamErrNone, "none"},
		{upstreamErrHardCredit, "hard_credit"},
		{upstreamErrSessionDead, "session_dead"},
		{upstreamErrAccountBanned, "account_banned"},
		{upstreamErrAccountNotActivated, "account_not_activated"},
		{upstreamErrModelRateLimit, "model_rate_limit"},
		{upstreamErrSoftRate, "soft_rate"},
		{upstreamErrContentBlocked, "content_blocked"},
		{upstreamErrBadParams, "bad_params"},
		{upstreamErrServer, "server"},
		{upstreamErrOther, "other"},
	}
	for _, tc := range cases {
		if got := tc.kind.String(); got != tc.want {
			t.Fatalf("kind %d String() = %q, want %q", int(tc.kind), got, tc.want)
		}
	}
}

func TestIsModelRateLimit(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"compact", `{"code":6004}`, true},
		{"spaced", `{"code": 6004}`, true},
		{"extra spaced", `{ "code" :  6004 , "msg":"x" }`, true},
		{"quoted value", `{"code":"6004"}`, true},
		{"quoted spaced", `{"code" : "6004" }`, true},
		{"other code", `{"code":11140}`, false},
		{"code as text", `code 6004 reset`, false},
		{"empty", "", false},
		{"6004 without code key", `{"msg":"6004"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isModelRateLimit(tc.body); got != tc.want {
				t.Fatalf("isModelRateLimit(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

func TestParseSoftRateReset(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantOK   bool
		wantWall string // same instant rendered in the fixed UTC+8 zone
	}{
		{
			name:     "6004 reset time",
			body:     `{"code":6004,"msg":"该模型使用量已达上限，将在 2026-09-15 10:00:00 重置"}`,
			wantOK:   true,
			wantWall: "2026-09-15 10:00:00",
		},
		{
			name:     "6004 spaced json",
			body:     `{"code": 6004, "msg": "将在 2026-09-15 18:30:05 重置"}`,
			wantOK:   true,
			wantWall: "2026-09-15 18:30:05",
		},
		{
			name:     "6004 with UTC+8 suffix",
			body:     `{"code":6004,"msg":"将在 2026-09-15 09:00:00 UTC+8 重置"}`,
			wantOK:   true,
			wantWall: "2026-09-15 09:00:00",
		},
		{
			// Reset wording is parsed regardless of the business code that
			// carries it: upstream attaches "resets at <time>" to several
			// throttle shapes (6004 and the 11140 rate-limiting variant), and
			// gating the parse on 6004 alone left the others with no known
			// self-heal instant — callers then fell back to an escalating
			// cooldown that never aligned with the real reset.
			name:     "11140 rate-limiting body also parses",
			body:     `{"code":11140,"msg":"请求过于频繁，将在 2026-09-15 10:00:00 重置"}`,
			wantOK:   true,
			wantWall: "2026-09-15 10:00:00",
		},
		{
			// English form (global realm bodies): anchored on the timestamp
			// shape so natural language like "reset at the end of the day"
			// cannot match.
			name:     "english reset form",
			body:     `{"code":6004,"msg":"usage exceeds frequency limit, reset at 2026-09-15 07:30:00 UTC+8, you can switch to the other models"}`,
			wantOK:   true,
			wantWall: "2026-09-15 07:30:00",
		},
		{
			name:   "english natural language does not match",
			body:   `{"code":6004,"msg":"usage exceeds frequency limit, reset at the end of the day"}`,
			wantOK: false,
		},
		{
			name:   "6004 without reset wording",
			body:   `{"code":6004,"msg":"model rate limited"}`,
			wantOK: false,
		},
		{
			name:   "6004 with malformed time",
			body:   `{"code":6004,"msg":"将在 明天 重置"}`,
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseSoftRateReset(tc.body)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				if !got.IsZero() {
					t.Fatalf("failed parse must return zero time, got %v", got)
				}
				return
			}
			// Fixed UTC+8: the wall clock in the message is reproduced whatever
			// the host's local timezone is, and the instant is +08:00.
			if wall := got.In(softRateResetLoc).Format(softRateTimeLayout); wall != tc.wantWall {
				t.Fatalf("reset wall clock = %q, want %q", wall, tc.wantWall)
			}
			if name, offset := got.Zone(); name != "UTC+8" || offset != 8*60*60 {
				t.Fatalf("zone = %q/%d, want UTC+8/28800", name, offset)
			}
			wantUTC := time.Date(
				mustAtoi(t, tc.wantWall[0:4]), time.Month(mustAtoi(t, tc.wantWall[5:7])), mustAtoi(t, tc.wantWall[8:10]),
				mustAtoi(t, tc.wantWall[11:13]), mustAtoi(t, tc.wantWall[14:16]), mustAtoi(t, tc.wantWall[17:19]),
				0, time.FixedZone("UTC+8", 8*60*60))
			if !got.Equal(wantUTC) {
				t.Fatalf("instant = %v, want %v", got.UTC(), wantUTC.UTC())
			}
		})
	}
}

// TestSoftRateResetLocIsFixedOffset guards the "do not depend on host TZ"
// constraint directly: the location must be a fixed offset, not time.Local.
func TestSoftRateResetLocIsFixedOffset(t *testing.T) {
	_, offset := time.Now().In(softRateResetLoc).Zone()
	if offset != 8*60*60 {
		t.Fatalf("softRateResetLoc offset = %d, want 28800", offset)
	}
	if softRateResetLoc == time.Local {
		t.Fatal("softRateResetLoc must not alias time.Local")
	}
}

func TestExecutorErrorEffectFor(t *testing.T) {
	cases := []struct {
		kind upstreamErrKind
		want executorErrorEffect
	}{
		{upstreamErrHardCredit, executorEffectRecheckCredits},
		{upstreamErrSessionDead, executorEffectDisableUntilRelogin},
		{upstreamErrAccountBanned, executorEffectDisableUntilRelogin},
		{upstreamErrAccountNotActivated, executorEffectSoftCooldown},
		{upstreamErrModelRateLimit, executorEffectModelRateLimit},
		// No effect class may touch the credential for these: content firewall
		// hits are false positives, bad params are ours, and a 5xx is upstream's.
		{upstreamErrContentBlocked, executorEffectIgnore},
		{upstreamErrBadParams, executorEffectIgnore},
		{upstreamErrSoftRate, executorEffectIgnore},
		{upstreamErrServer, executorEffectIgnore},
		{upstreamErrOther, executorEffectIgnore},
		{upstreamErrNone, executorEffectIgnore},
	}
	for _, tc := range cases {
		if got := executorErrorEffectFor(tc.kind); got != tc.want {
			t.Fatalf("%s effect = %d, want %d", tc.kind, int(got), int(tc.want))
		}
	}
}

func TestUpstreamFaultNoteAndReenableGate(t *testing.T) {
	sessionNote := upstreamFaultNote(upstreamErrSessionDead)
	banNote := upstreamFaultNote(upstreamErrAccountBanned)
	if !strings.Contains(sessionNote, "12153") {
		t.Fatalf("session-dead note must carry the upstream code: %q", sessionNote)
	}
	if !strings.Contains(banNote, "11140") {
		t.Fatalf("ban note must carry the upstream code: %q", banNote)
	}
	// The wording is also what keepalive.go writes, so a disable made by the
	// daily refresh blocks the credit-driven re-enable with the same rule.
	if !reenableBlockedByNote("Session dead (12153): re-login required") {
		t.Fatal("keepalive session-dead note must block re-enable")
	}
	cases := []struct {
		name string
		note string
		want bool
	}{
		{"session dead note", sessionNote, true},
		{"ban note", banNote, true},
		{"generic fault note", upstreamFaultNote(upstreamErrOther), true},
		{"chinese relogin hint", "上游封禁: 需要重新登录", true},
		// Credit notes must keep allowing the credit-driven re-enable.
		{"cn exhausted note", "CN · 已禁用 · 耗尽 · 余0 已用10", false},
		{"healthy note", "CN · 余12 已用8", false},
		{"unknown credits note", "Global · 积分未知", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reenableBlockedByNote(tc.note); got != tc.want {
				t.Fatalf("reenableBlockedByNote(%q) = %v, want %v", tc.note, got, tc.want)
			}
		})
	}
}

// TestHardCreditMarkersUnchanged pins the credit marker list: classifyUpstreamError
// delegates to isHardCreditError, so any edit here silently reclassifies real
// 402/credit bodies.
func TestHardCreditMarkersUnchanged(t *testing.T) {
	body := strings.Join(hardCreditMarkers, "|")
	for _, marker := range []string{
		"insufficient credit", "insufficient credits", "no credit", "no credits",
		"credit exhausted", "credits exhausted", "out of credit", "out of credits",
		"quota exceeded", "quota exhaust", "payment required",
		"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "额度已用尽", "没有积分",
		"credit not enough", "not enough credit",
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("hardCreditMarkers lost %q", marker)
		}
	}
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"402", 402, "", true},
		{"chinese", 400, "额度已用尽", true},
		{"pure 429 wording", 429, "too many requests", false},
		{"rate wording", 429, "rate limit exceeded", false},
		{"content block", 400, `{"code":11128,"msg":"blocked by security policy"}`, false},
		{"trial not activated", 429, `{"code":14017,"msg":"trial not activated"}`, false},
		{"model rate limit", 429, `{"code":6004,"msg":"将在 2026-09-15 10:00:00 重置"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isHardCreditError(tc.status, tc.body); got != tc.want {
				t.Fatalf("isHardCreditError(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

// TestIsSoftRateLimitKeepsLegacyBehaviour locks the coarse helper the panel and
// older callers still use, including its "hard credit is never soft" rule.
func TestIsSoftRateLimitKeepsLegacyBehaviour(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"429 bare", 429, "too many requests", true},
		{"429 rate limit", 429, "rate limit", true},
		{"throttled on 403", 403, "request throttled", true},
		{"credit on 403", 403, "insufficient credit", false},
		// A 429 body carrying credit wording is throttling, not a spent
		// balance: the status code wins (see TestClassifyUpstreamError).
		{"quota on 429 is soft", 429, "quota exceeded", true},
		{"500", 500, "error", false},
		{"200", 200, `{"code":0}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSoftRateLimit(tc.status, tc.body); got != tc.want {
				t.Fatalf("isSoftRateLimit(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

// TestReenableBlockedByFaultNoteReadsPhysicalJSON: with the bundle hook stubbed
// (the seam enterprise_test uses), phys arrives nil and the note must still be
// read back from the host so a fault disable survives the reconcile tick.
func TestReenableBlockedByFaultNoteReadsPhysicalJSON(t *testing.T) {
	const authIndex = "auth-index-fault-note"
	oldGetBundle := reconcileHostAuthGetBundle
	reconcileHostAuthGetBundle = func(idx string) (*storedAuth, *hostAuthPhysical, error) {
		if idx != authIndex {
			return nil, nil, errors.New("unknown auth")
		}
		return &storedAuth{}, &hostAuthPhysical{JSON: []byte(
			`{"type":"workbuddy","disabled":true,"note":"Session dead (12153): re-login required"}`)}, nil
	}
	t.Cleanup(func() { reconcileHostAuthGetBundle = oldGetBundle })

	if !reenableBlockedByFaultNote(nil, authIndex) {
		t.Fatal("nil physical record must fall back to host.auth.get and detect the fault note")
	}
	if reenableBlockedByFaultNote(&hostAuthPhysical{JSON: []byte(`{"disabled":true,"note":"CN · 已禁用 · 耗尽"}`)}, authIndex) {
		t.Fatal("exhaustion note must not block re-enable")
	}
	if reenableBlockedByFaultNote(nil, "auth-index-unknown") {
		t.Fatal("unknown auth must not block re-enable")
	}
}

// recordExecutorDispatch swaps the two dispatch hooks for recording stubs. The
// hooks are goroutine bodies, so both callbacks send on a buffered channel the
// test drains — that keeps the assertion deterministic under -race without
// sleeping.
type recordedDispatch struct {
	kind    string // "credit" or "fault"
	authID  string
	errKind upstreamErrKind
	status  int
	body    string
}

func recordExecutorDispatch(t *testing.T) <-chan recordedDispatch {
	t.Helper()
	events := make(chan recordedDispatch, 8)
	oldHooks := executorErrorDispatchHooks
	executorErrorDispatchHooks = struct {
		reconcile func(authID string)
		fault     func(authID string, kind upstreamErrKind, status int, body string)
	}{
		reconcile: func(authID string) {
			events <- recordedDispatch{kind: "credit", authID: authID}
		},
		fault: func(authID string, kind upstreamErrKind, status int, body string) {
			events <- recordedDispatch{kind: "fault", authID: authID, errKind: kind, status: status, body: body}
		},
	}
	t.Cleanup(func() { executorErrorDispatchHooks = oldHooks })
	return events
}

// TestApplyExecutorErrorEffectDispatch pins the mapping from classification to
// lifecycle effect. This is the behaviour the plugin actually relies on, and it
// is the reason plain rate limiting must never reach a reconcile.
func TestApplyExecutorErrorEffectDispatch(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantKind   string // "" = no dispatch at all
		wantErr    upstreamErrKind
		wantEffect executorErrorEffect
	}{
		{
			name:       "402 credit reconciles",
			status:     402,
			body:       `{"msg":"payment required"}`,
			wantKind:   "credit",
			wantErr:    upstreamErrHardCredit,
			wantEffect: executorEffectRecheckCredits,
		},
		{
			name:       "429 soft rate is left alone",
			status:     429,
			body:       `{"msg":"too many requests"}`,
			wantErr:    upstreamErrSoftRate,
			wantEffect: executorEffectIgnore,
		},
		{
			name:       "6004 model rate is left alone",
			status:     429,
			body:       `{"code":6004,"msg":"将在 2026-09-15 10:00:00 重置"}`,
			wantKind:   "fault",
			wantErr:    upstreamErrModelRateLimit,
			wantEffect: executorEffectModelRateLimit,
		},
		{
			name:       "11128 content block is left alone",
			status:     400,
			body:       `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`,
			wantErr:    upstreamErrContentBlocked,
			wantEffect: executorEffectIgnore,
		},
		{
			name:       "bad params are left alone",
			status:     400,
			body:       `{"msg":"Unmarshal chat params failed"}`,
			wantErr:    upstreamErrBadParams,
			wantEffect: executorEffectIgnore,
		},
		{
			name:       "session dead disables",
			status:     401,
			body:       `{"msg":"Offline user session not found"}`,
			wantKind:   "fault",
			wantErr:    upstreamErrSessionDead,
			wantEffect: executorEffectDisableUntilRelogin,
		},
		{
			name:       "11140 ban disables",
			status:     403,
			body:       `{"code":11140,"msg":"request illegal"}`,
			wantKind:   "fault",
			wantErr:    upstreamErrAccountBanned,
			wantEffect: executorEffectDisableUntilRelogin,
		},
		{
			name:       "14017 stays enabled",
			status:     429,
			body:       `{"code":14017,"msg":"trial not activated"}`,
			wantKind:   "fault",
			wantErr:    upstreamErrAccountNotActivated,
			wantEffect: executorEffectSoftCooldown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events := recordExecutorDispatch(t)
			applyExecutorErrorEffect("auth-id-under-test", classifyUpstreamError(tc.status, tc.body), tc.status, tc.body)

			if tc.wantKind == "" {
				select {
				case got := <-events:
					t.Fatalf("expected no lifecycle dispatch, got %+v (effect %d)", got, int(tc.wantEffect))
				default:
				}
				return
			}
			select {
			case got := <-events:
				if got.kind != tc.wantKind || got.authID != "auth-id-under-test" {
					t.Fatalf("dispatch = %+v, want kind %q for auth-id-under-test", got, tc.wantKind)
				}
				if got.kind == "fault" && got.errKind != tc.wantErr {
					t.Fatalf("fault kind = %s, want %s", got.errKind, tc.wantErr)
				}
				if got.kind == "fault" && got.status != tc.status {
					t.Fatalf("fault status = %d, want %d", got.status, tc.status)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("expected %s dispatch, got none", tc.wantKind)
			}
		})
	}
}

// TestExecutorErrorDispatchHonoursLifecycleToggle: with lifecycle_auto off no
// entry point may dispatch anything, not even a log-worthy ignore class.
func TestExecutorErrorDispatchHonoursLifecycleToggle(t *testing.T) {
	events := recordExecutorDispatch(t)
	lifecycleAutoMu.Lock()
	oldAuto := lifecycleAuto
	lifecycleAuto = false
	lifecycleAutoMu.Unlock()
	t.Cleanup(func() {
		lifecycleAutoMu.Lock()
		lifecycleAuto = oldAuto
		lifecycleAutoMu.Unlock()
	})

	reconcileAfterExecutorError("auth-id", 402, `{"msg":"payment required"}`)
	reconcileByUID("uid-1", 401, `{"msg":"Offline user session not found"}`)
	reconcileAfterExecutorError("auth-id", 429, `{"code":6004,"msg":"将在 2026-09-15 10:00:00 重置"}`)

	select {
	case got := <-events:
		t.Fatalf("lifecycle_auto=false still dispatched %+v", got)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestReconcileEntryPointsShareClassification: the streaming path (reconcileByUID)
// and the sync path (reconcileAfterExecutorError) must agree on every class —
// they see the same upstream bodies and a divergence would make an account's
// treatment depend on which API surface the client used.
func TestReconcileEntryPointsShareClassification(t *testing.T) {
	bodies := []struct {
		status int
		body   string
	}{
		{429, `{"code":6004,"msg":"将在 2026-09-15 10:00:00 重置"}`},
		{400, `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`},
		{400, `{"msg":"Unmarshal chat params failed"}`},
		{429, `{"msg":"too many requests"}`},
		{403, `{"code":11140,"msg":"request illegal"}`},
		{429, `{"code":14017,"msg":"trial not activated"}`},
		{401, `{"msg":"Offline user session not found"}`},
		{402, `{"msg":"payment required"}`},
	}
	for _, b := range bodies {
		kind := classifyUpstreamError(b.status, b.body)
		events := recordExecutorDispatch(t)
		reconcileAfterExecutorError("shared-auth", b.status, b.body)
		eventsByUID := recordExecutorDispatch(t)
		reconcileByUID("shared-auth", b.status, b.body)

		// The hooks run in goroutines but write to buffered channels
		// immediately; a 250ms window is generous under -race. Waiting longer
		// only slows the suite for silence assertions on ignore-kinds.
		select {
		case a := <-events:
			select {
			case c := <-eventsByUID:
				if a.kind != c.kind || a.errKind != c.errKind {
					t.Fatalf("status=%d body=%q: sync=%+v stream=%+v", b.status, b.body, a, c)
				}
			case <-time.After(250 * time.Millisecond):
				t.Fatalf("status=%d body=%q (%s): stream path dispatched nothing", b.status, b.body, kind)
			}
		case <-time.After(250 * time.Millisecond):
			// Both must be silent for ignore-kinds; verify that too.
			select {
			case c := <-eventsByUID:
				t.Fatalf("status=%d body=%q (%s): sync path silent but stream dispatched %+v", b.status, b.body, kind, c)
			default:
			}
		}
	}
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			t.Fatalf("mustAtoi(%q): not a number", s)
		}
		n = n*10 + int(r-'0')
	}
	return n
}
