// Package main implements the Qoder CLIProxyAPI dynamic plugin.
//
// Qoder wraps the Qoder CN (qoder.com.cn) OpenAPI as a cliproxy
// provider: it exchanges a PAT for a jobToken, refreshes it, signs inference
// requests with COSY, and exposes the standard chat-completions interface.
// upstream /v2/chat/completions endpoint.
//
// This file is a clean-room reimplementation reconstructed from the public
// qoder.so binary (symbol table, string constants and RPC shape) published
// by Sliverkiss. Original credit for the Qoder plugin goes to Sliverkiss;
// see https://github.com/Sliverkiss/cpa-plugin. Built with -buildmode=c-shared
// and exports the cliproxy C ABI entry points.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

// Wrappers so Go can invoke the host function-pointer table via cgo. The host
// API captured at init is used to push streaming chunks back asynchronously.
static int wb_call_host(cliproxy_host_api* api, const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	return api->call(api->host_ctx, method, request, request_len, response);
}
static void wb_free_host_buffer(cliproxy_host_api* api, void* ptr, size_t len) {
	api->free_buffer(ptr, len);
}

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	providerName = "qoder"
	// displayName is the human-readable plugin title shown by management clients;
	// providerName stays the stable identifier (config keys, routes, auth files).
	displayName  = "Qoder"
	authFileName = "qoder.json"
	// legacyAuthPrefix is the file prefix this plugin used before it was
	// the plugin's previous name. Such files are still claimed and rewritten to
	// the current prefix on the next save.
	legacyAuthPrefix = "qoderwork-"
	legacyProvider   = "qoderwork"
	pluginLogoURL    = "https://raw.githubusercontent.com/hex-ci/cpa-plugin/main/qoder/assets/qoder.png"

	// defaultCosyVersion is the COSY protocol version the official CLI ships.
	// It equals the CLI version and is validated server-side against the
	// client version gate, so it is a config key: upstream bumps it without
	// changing anything else.
	defaultCosyVersion = "1.1.75"

	// loginTTL bounds one device-authorization flow. Users may need to log in
	// to the site first (Aliyun SSO) before authorizing — give them room.
	loginTTL = 10 * time.Minute
)

// loginCtx holds one in-flight device-authorization login flow. Qoder's
// desktop clients use a PKCE device flow: the plugin generates
// verifier/challenge + nonce/machine_id, the user authorizes in a browser,
// and PollLogin exchanges the grant at /api/v1/deviceToken/poll.
type loginCtx struct {
	verifier  string    // PKCE code_verifier — required by the poll endpoint
	nonce     string    // device-flow nonce — paired with the auth URL
	realm     realmSpec // region this attempt authorizes against
	expires   time.Time
	startedAt int64 // unix nano, set when StartLogin creates the state
}

var (
	hostAPI        *C.cliproxy_host_api // captured at init, used for async host calls
	loginStates    sync.Map             // state(string) -> *loginCtx
	httpClientOnce sync.Once
	sharedClient   *http.Client
)

// loginStatesPruneInterval bounds how often the janitor sweeps abandoned
// login states (user started a login but never finished).
const loginStatesPruneInterval = time.Minute

func init() {
	go func() {
		ticker := time.NewTicker(loginStatesPruneInterval)
		defer ticker.Stop()
		for range ticker.C {
			now := time.Now()
			loginStates.Range(func(key, value any) bool {
				if lc, ok := value.(*loginCtx); ok && now.After(lc.expires) {
					loginStates.Delete(key)
				}
				return true
			})
		}
	}()
}

func main() {}

// -----------------------------------------------------------------------------
// C ABI exports
// -----------------------------------------------------------------------------

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	hostAPI = host
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	// Intentionally a no-op. The host calls this on its own exit path (after
	// the host Go runtime has started tearing down) and dlclose()es this
	// library immediately afterwards. Touching any Go runtime state here —
	// mutexes, channel close, goroutine synchronization — risks a SIGSEGV in
	// cgo (observed on every docker restart: SIGSEGV in
	// _Cfunc_cliproxy_shutdown_plugin, PC near a freed runtime pointer).
	// The scheduler goroutine and janitor ticker hold no resources that
	// outlive the process; the OS reclaims them on exit.
}

// -----------------------------------------------------------------------------
// Host calls (async streaming + auth callbacks)
// -----------------------------------------------------------------------------

// hostCall invokes a host RPC method via the function-pointer table captured
// at init. Used to push stream chunks back asynchronously (host.stream.emit /
// host.stream.close) and to read the host's auth store (host.auth.list/get).
func hostCall(method string, request []byte) ([]byte, error) {
	if hostAPI == nil || hostAPI.call == nil {
		return nil, fmt.Errorf("host API unavailable")
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var cReq unsafe.Pointer
	var reqLen C.size_t
	if len(request) > 0 {
		cReq = C.CBytes(request)
		defer C.free(cReq)
		reqLen = C.size_t(len(request))
	}
	var resp C.cliproxy_buffer
	rc := C.wb_call_host(hostAPI, cMethod, (*C.uint8_t)(cReq), reqLen, &resp)
	var out []byte
	if resp.ptr != nil && resp.len > 0 {
		out = C.GoBytes(resp.ptr, C.int(resp.len))
	}
	if resp.ptr != nil && hostAPI.free_buffer != nil {
		C.wb_free_host_buffer(hostAPI, resp.ptr, resp.len)
	}
	if rc != 0 {
		return out, fmt.Errorf("host call %s returned %d", method, int(rc))
	}
	return out, nil
}

// -----------------------------------------------------------------------------

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if err := configure(request); err != nil {
			return errorEnvelope("config_invalid", err.Error()), nil
		}
		return okEnvelope(wbRegistration())
	case pluginabi.MethodModelStatic:
		return handleModelStatic(request)
	case pluginabi.MethodModelForAuth:
		return handleModelForAuth(request)
	case pluginabi.MethodAuthIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerName})
	case pluginabi.MethodAuthParse:
		return handleParseAuth(request)
	case pluginabi.MethodAuthLoginStart:
		return handleStartLogin(request)
	case pluginabi.MethodAuthLoginPoll:
		return handlePollLogin(request)
	case pluginabi.MethodAuthRefresh:
		return handleRefreshAuth(request)
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerName})
	case pluginabi.MethodExecutorExecute:
		return handleExecExecute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return handleExecStream(request)
	case pluginabi.MethodExecutorCountTokens:
		// Upstream Qoder has no dedicated count_tokens API. Return
		// unhandled-style zero estimate so clients fall back / skip.
		return okEnvelope(pluginapi.ExecutorResponse{Payload: []byte(`{"input_tokens":0}`)})
	case pluginabi.MethodManagementRegister:
		// Cache host-injected BasePath so handleManagement doesn't hardcode
		// /v0/management (v0.6.31: tolerate future host path changes).
		var regReq pluginapi.ManagementRegistrationRequest
		if err := json.Unmarshal(request, &regReq); err == nil {
			if regReq.BasePath != "" {
				setManagementBasePath(regReq.BasePath)
			}
		}
		return okEnvelope(managementRegistration())
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
	case pluginabi.MethodSchedulerPick:
		return handleSchedulerPick(request)
	case pluginabi.MethodUsageHandle:
		return handleUsage(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// -----------------------------------------------------------------------------
// Registration & models
// -----------------------------------------------------------------------------

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type streamResponse struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

type registrationCapability struct {
	ModelProvider         bool                         `json:"model_provider"`
	AuthProvider          bool                         `json:"auth_provider"`
	FrontendAuthProvider  bool                         `json:"frontend_auth_provider"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats,omitempty"`
	Scheduler             bool                         `json:"scheduler"`
	ManagementAPI         bool                         `json:"management_api"`
	UsagePlugin           bool                         `json:"usage_plugin"`
}

// version is injected at build time via -ldflags "-X main.version=...".
var version = "1.0.1"

func wbRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             displayName,
			Version:          version,
			Author:           "Hex",
			GitHubRepository: "https://github.com/hex-ci/cpa-plugin",
			Logo:             pluginLogoURL,
			ConfigFields: []pluginapi.ConfigField{
				{Name: "cosy_version", Type: pluginapi.ConfigFieldTypeString, Description: "COSY protocol version, sent as cosy-version and inside the signature payload. Track the official CLI version (default 1.1.75)."},
				{Name: "realm_default", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{realmCN, realmGlobal}, Description: "Region for new imports: cn (qoder.com.cn) or global (qoder.com). Stored per credential; existing accounts keep their own realm."},
				{Name: "scheduler_mode", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{schedulerModeOff, schedulerModeCredits}, Description: "Multi-account selection: off (defer to built-in, default) or credits (prefer the account with the most remaining credits)."},
				{Name: "checkin_auto", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Daily check-in at 10:00 China time (default true). Skipped where the region has no check-in endpoint."},
				{Name: "token_keepalive", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Refresh access tokens daily at 22:00 local time (default true)."},
				{Name: "lifecycle_auto", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Disable an account when its credits are exhausted and re-enable it after they are restored (default true). Accounts are never deleted."},
				{Name: "queue_wait", Type: pluginapi.ConfigFieldTypeBoolean, Description: "When upstream answers 10605 (model capacity queue), wait for retryAfterSeconds instead of failing the request (default true)."},
				{Name: "queue_max_wait", Type: pluginapi.ConfigFieldTypeInteger, Description: "Total queue waiting budget per request in seconds, 1-3600 (default 600). Exhausted budget surfaces as 429 with Retry-After."},
				{Name: "cosy_machine_os", Type: pluginapi.ConfigFieldTypeString, Description: "Value of cosy-machineos as {arch}_{platform}. Empty follows the host (x86_64_linux, x86_64_win32, aarch64_darwin)."},
				{Name: "model_list_body", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{modelListBodyKWM, modelListBodyEmpty}, Description: "Body the catalog request signs and sends: kwm (encoded empty object, current CLI) or empty (older shells)."},
				{Name: "desensitize", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Insert U+200B into configured blocked terms in system/developer prompt text, marked user text and tool title/description fields (default false)."},
				{Name: "desensitize_terms", Type: pluginapi.ConfigFieldTypeArray, Description: "Editable literal term list for desensitize; missing uses the built-in 85 terms, [] means an empty custom list."},
				{Name: "models", Type: pluginapi.ConfigFieldTypeArray, Description: "Optional model overrides. Each item can have id, name, context, max_tokens, reasoning, levels, enabled."},
				{Name: "usage_report_url", Type: pluginapi.ConfigFieldTypeString, Description: "CPAMP usage import URL; reporting stays off unless this (or env USAGE_REPORT_URL) and a key are both set."},
				{Name: "usage_report_key", Type: pluginapi.ConfigFieldTypeString, Description: "CPAMP admin key override. Prefer env CPAMP_ADMIN_KEY / USAGE_REPORT_KEY or a secret file."},
			},
		},
		Capabilities: registrationCapability{
			ModelProvider:         true,
			AuthProvider:          true,
			FrontendAuthProvider:  false,
			Executor:              true,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeOAuth,
			ExecutorInputFormats:  []string{"chat-completions"},
			ExecutorOutputFormats: []string{"chat-completions"},
			ManagementAPI:         true,
			Scheduler:             true,
			UsagePlugin:           true,
		},
	}
}

// dynamicModelsCacheTTL bounds how long a fetched model list is reused.
// model.static / model.for_auth are re-invoked by CPA on every config reload
// and on each models query; without caching, every reload fans out to one
// upstream call per account.
const dynamicModelsCacheTTL = 5 * time.Minute

var dynamicModelsCache struct {
	sync.RWMutex
	models  []pluginapi.ModelInfo
	fetched time.Time
}

//
// CPA applies oauth-model-alias to the models this plugin registers, so the
// gateway may route a request whose model ID is an alias (e.g.
// "point/deepseek-v4-flash") to this executor. The upstream only knows the
// real model IDs, so the plugin must map the alias back before forwarding.
//
// ExecutorRequest carries no host config, so the alias table is cached from
// the AuthModelRequest.Host summary every time the host asks for models
// (model.static / model.for_auth are re-queried by CPA on config reload,
// keeping this cache in sync with oauth-model-alias changes). Auth-level
// attribute overrides ("model_alias"/"model-alias"/"oauth-model-alias")
// are parsed per request and take precedence over the global table.

var modelAliasCache struct {
	sync.RWMutex
	byAlias map[string]string
}

// ------------------------------------------------------------------------------
// Usage reporting (request monitoring)
// ------------------------------------------------------------------------------
//
// CPA built-in executors publish via host usage.DefaultManager → redisqueue.
// Plugin executors cannot: c-shared .so has its own Go runtime, so
// usage.PublishRecord would hit a separate empty DefaultManager (no sink).
//
// Only effective path: POST NDJSON to CPA-Manager-Plus
// /v0/management/usage/import. Key/URL resolved automatically from
// config → env → docker secret files (see resolveUsageReport).
// usage.Detail is still used as a pure token-counter struct.

func hostAuthListFiles() ([]pluginapi.HostAuthFileEntry, error) {
	raw, err := hostCall(pluginabi.MethodHostAuthList, nil)
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		return nil, fmt.Errorf("host.auth.list failed")
	}
	var resp rpcHostAuthListResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		return nil, err
	}
	return resp.Files, nil
}

// hostAuthGetByIndex fetches the raw JSON for one auth index.
func hostAuthGetByIndex(authIndex string) ([]byte, error) {
	body, _ := json.Marshal(map[string]string{"auth_index": authIndex})
	raw, err := hostCall(pluginabi.MethodHostAuthGet, body)
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		return nil, fmt.Errorf("host.auth.get failed")
	}
	var resp rpcHostAuthGetResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		return nil, err
	}
	return resp.JSON, nil
}

// storedAuth is the on-disk shape of a Qoder credential.
type storedAuth struct {
	Auth    storedTokens  `json:"auth"`
	Account storedAccount `json:"account"`
}

// storedTokens holds one credential family at a time in Access/RefreshToken,
// plus the long-lived PAT fallback. Two families coexist in one file:
//
//	PAT family:   accessToken=jt- (24h),  refreshToken=jrt- (48h)
//	OAuth family: accessToken=dt- (~30d), refreshToken=drt- (~1y, rotating)
//
// personalToken (pt-) is family-independent and never overwritten by refreshes
// — it re-exchanges a fresh jobToken pair when both live tokens die.
type storedTokens struct {
	AccessToken   string `json:"accessToken"`   // jt- (24h) or dt- (~30d)
	RefreshToken  string `json:"refreshToken"`  // jrt- (48h) or drt- (~1y)
	PersonalToken string `json:"personalToken"` // pt-..., long-lived fallback
	ExpiresAt     int64  `json:"expiresAt"`     // active-token expiry (unix seconds)
	Domain        string `json:"domain"`        // realm: qoder.com.cn
}

type storedAccount struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
	// OrganizationName/ID identify the enterprise tenant. The COSY identity
	// body carries them, and an org account without them is treated as a
	// personal account by the inference plane (wrong node, model unusable).
	OrganizationName string `json:"organizationName,omitempty"`
	// UserType is the upstream account class (personal_* / teams / enterprise_*).
	// The chat body carries it, and the gateway routes by it: sending the
	// personal default for a teams account makes the model unresolvable.
	UserType string `json:"userType,omitempty"`
}

// jobTokenResponse is defined in oauth.go; keepalive and handleRefreshAuth
// both use it. The old tokenData struct (camelCase tags) was wrong and has
// been removed — Qoder returns snake_case JSON.

func parseStored(raw []byte) (*storedAuth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	// Accept both shapes seen in the wild:
	//   nested: {"auth":{"accessToken":...},"account":{"uid":...}} (plugin/oauth output)
	//   flat:   {"accessToken":...,"uid":...,"nickname":...} (CPA-Manager-Plus auths/qoder.json)
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	var sa storedAuth
	if _, nested := probe["auth"]; nested {
		if err := json.Unmarshal(raw, &sa); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
	} else {
		var flat struct {
			AccessToken   string `json:"accessToken"`
			RefreshToken  string `json:"refreshToken"`
			PersonalToken string `json:"personalToken"` // PAT fallback — must survive the flat shape too
			ExpiresAt     int64  `json:"expiresAt"`
			Domain        string `json:"domain"`
			UID           string `json:"uid"`
			EnterpriseID  string `json:"enterpriseId"`
			Nickname      string `json:"nickname"`
		}
		if err := json.Unmarshal(raw, &flat); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		sa.Auth = storedTokens{AccessToken: flat.AccessToken, RefreshToken: flat.RefreshToken, PersonalToken: flat.PersonalToken, ExpiresAt: flat.ExpiresAt, Domain: flat.Domain}
		sa.Account = storedAccount{UID: flat.UID, EnterpriseID: flat.EnterpriseID, Nickname: flat.Nickname}
	}
	if sa.Auth.AccessToken == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}
	return &sa, nil
}

// -------------------------------------------------------------------------------
// HTTP plumbing
// -------------------------------------------------------------------------------

// accountHeaders builds the account-plane header set for one realm: these
// endpoints take a plain Bearer token and are not COSY-signed. The UA must not
// be a Go default — that advertises "this is a script" on every call.
func accountHeaders(r realmSpec) func(*http.Request) {
	return func(req *http.Request) {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("User-Agent", r.accountUserAgent(cosyVersion()))
		req.Header.Set("X-Request-ID", randomHex(16))
		req.Header.Set("Origin", r.Website)
		req.Header.Set("Referer", r.Website+"/")
	}
}

// applyCosyHeaders computes the full COSY-signed header set for one inference
// request and applies it to req. Body must be the QoderEncoding-encoded
// request body (string form), url the gateway endpoint, modelKey the
// upstream model key (goes into x-model-key). sse toggles cache-control.
//
// Returns an error if the cosy session cannot be built (e.g. empty token).
func applyCosyHeaders(req *http.Request, sa *storedAuth, encodedBody, rawURL, modelKey string, sse bool) error {
	sess, err := cosySessionFor(sa)
	if err != nil {
		return err
	}
	hdr, err := sess.headers(sa.Account.UID, encodedBody, rawURL, "text/event-stream", sse)
	if err != nil {
		return err
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if modelKey != "" {
		req.Header.Set("x-model-key", modelKey)
		req.Header.Set("x-model-source", "system")
	}
	return nil
}

// newChatRequest builds one signed chat request. Each attempt needs its own
// request object: the bridge consumes the body.
func newChatRequest(ctx context.Context, sa *storedAuth, encodedBody, chatURL, modelKey string) (*http.Request, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatURL, strings.NewReader(encodedBody))
	if err != nil {
		return nil, err
	}
	if err := applyCosyHeaders(req, sa, encodedBody, chatURL, modelKey, true); err != nil {
		return nil, err
	}
	return req, nil
}

// queuedError renders a capacity-queue timeout as a retryable error carrying
// the upstream's own hint, so the caller can pass it through unchanged.
func queuedError(detail string, retryAfter time.Duration) error {
	secs := int(retryAfter / time.Second)
	if secs <= 0 {
		secs = defaultQueueRetrySeconds
	}
	return fmt.Errorf("upstream_queued: model capacity queue for %ds (last: %s)", secs, detail)
}

// -----------------------------------------------------------------------------
// Auth handlers
// -----------------------------------------------------------------------------

func handleParseAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	// Ownership check (CPA native contract): the host routes by the file's
	// top-level "type" field (synthesizer/file.go). Files without a type fall
	// back to polling every plugin — first Handled=true wins. To prevent
	// claiming foreign providers' legacy files (e.g. WorkBuddy's type-less
	// auths, which parseStored would otherwise accept because the nested
	// {auth,account} shape is identical), only claim files whose declared
	// type matches us — or whose filename carries our prefix.
	var probeType struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(req.RawJSON, &probeType)
	declared := strings.ToLower(strings.TrimSpace(probeType.Type))
	if declared != "" && declared != providerName && declared != legacyProvider {
		// Explicitly another provider's file — never claim it.
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	if declared == "" {
		// No type declared: only claim when the host already routed this to us
		// or the filename carries one of our prefixes.
		routed := strings.EqualFold(strings.TrimSpace(req.Provider), providerName) ||
			strings.EqualFold(strings.TrimSpace(req.Provider), legacyProvider)
		if !routed && !isQoderAuthFileName(req.FileName) {
			return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
		}
	}
	sa, err := parseStored(req.RawJSON)
	if err != nil {
		// Not a Qoder credential; let the host try other providers.
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	// CRITICAL: echo back the host-provided FileName AND leave ID empty.
	//
	// CPA uses ID for auth record identity (upsert key). If we set ID=uid
	// while the host's watcher initially registered with ID=filename,
	// upsertAuthRecord can't find the existing record → creates a NEW one
	// → duplicate auth entries (same file, different IDs).
	//
	// By leaving ID empty, CPA falls back to authIDForPath(path) which
	// derives ID from the file path → always matches the watcher's key.
	// FileName is also echoed back to avoid rename-based duplicates.
	carrier := parseAuthMetadataCarrier(req.RawJSON)
	ad := toAuthDataOpts(sa, nil, false, carrier)
	ad.ID = "" // let host compute from path (prevents ID mismatch dupes)
	if fn := strings.TrimSpace(req.FileName); fn != "" {
		ad.FileName = fn
	}
	return okEnvelope(pluginapi.AuthParseResponse{
		Handled: true,
		Auth:    ad,
	})
}

func toAuthData(sa *storedAuth) pluginapi.AuthData {
	return toAuthDataOpts(sa, nil, false, nil)
}

var authMetadataUserKeys = []string{
	"weight",
	"priority",
	"proxy_url",
	"proxy-url",
	"prefix",
	"headers",
	"request_retry",
	"request-retry",
	"excluded_models",
	"excluded-models",
	"model_aliases",
	"model-aliases",
	"disable_cooling",
	"disable-cooling",
	"websockets",
	"note",
}

func parseAuthMetadataCarrier(raw []byte) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	carrier := make(map[string]any, len(authMetadataUserKeys))
	for _, key := range authMetadataUserKeys {
		if value, ok := doc[key]; ok {
			carrier[key] = value
		}
	}
	if len(carrier) == 0 {
		return nil
	}
	return carrier
}

// toAuthDataOpts builds AuthData with optional credits snapshot and disabled flag.
func toAuthDataOpts(sa *storedAuth, cr *creditsSummary, disabled bool, carrier map[string]any) pluginapi.AuthData {
	storage, _ := json.Marshal(sa)
	id := providerName
	fileName := authFileName
	if sa != nil {
		if uid := sanitizeUIDForFileName(sa.Account.UID); uid != "" {
			id = uid
			fileName = "qoder-" + uid + ".json"
		}
	}
	label := labelForAuth(sa)
	meta := enrichAuthMetadata(sa, cr, disabled)
	note := displayNote(sa, cr, disabled)
	// Parse-time calls carry no credits snapshot. Recomputing the note then
	// would overwrite a real balance with "积分未知" every time the host
	// rescans the auth dir, so keep whatever the file already says.
	if cr == nil {
		if existing, ok := carrier["note"].(string); ok && isPluginNote(existing) {
			note = existing
		}
	}
	meta["note"] = note
	meta["disabled"] = disabled
	for key, value := range carrier {
		if _, pluginOwned := meta[key]; pluginOwned {
			continue
		}
		meta[key] = value
	}
	return pluginapi.AuthData{
		Provider:    providerName,
		ID:          id,
		FileName:    fileName,
		Label:       label,
		Disabled:    disabled,
		StorageJSON: storage,
		// Standardized auth metadata. `type` is required by the host for
		// auth-file classification; `logo`/`note`/`disabled` surface on auth rows.
		Metadata: meta,
	}
}

// -----------------------------------------------------------------------------

func handleExecExecute(raw []byte) ([]byte, error) {
	var req pluginapi.ExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	// Map CPA-facing model name (e.g. "qoder/qmodel_preview" or "qmodel_preview")
	// to the upstream key the gateway recognises.
	upstreamModel := cpaToUpstreamKey(stripProviderPrefix(req.Model))
	started := time.Now()
	authUID := ""
	if sa.Account.UID != "" {
		authUID = sa.Account.UID
	}
	// Build the Qoder agent_chat_generation body from the OpenAI request,
	// then QoderEncoding-encode it. The template embeds a 10657-token system
	// prompt that the server requires for normal behaviour.
	qwReq := &openAIRequest{}
	if err := json.Unmarshal(req.Payload, qwReq); err != nil && len(req.Payload) > 0 {
		publishUsage(req.Model, upstreamModel, authUID, started, usage.Detail{}, true, 0, "payload parse: "+err.Error())
		return nil, fmt.Errorf("payload parse: %w", err)
	}
	body, err := buildQoderBody(qwReq, sa, upstreamModel, accountUserType(sa))
	if err != nil {
		publishUsage(req.Model, upstreamModel, authUID, started, usage.Detail{}, true, 0, "body build: "+err.Error())
		return nil, fmt.Errorf("body build: %w", err)
	}
	encodedBody := qoderEncode(body)
	r := realmOf(sa)
	chatURL := r.chatURL()

	// Compliance: route the POST through host.http.do_stream so request-log
	// captures the outbound call.
	var stream *hostHTTPStream
	var statusCode int
	var streamErr error
	queueCfg := currentConfig()
	for attempt := 0; ; attempt++ {
		httpReq, reqErr := newChatRequest(context.Background(), sa, encodedBody, chatURL, upstreamModel)
		if reqErr != nil {
			publishUsage(req.Model, upstreamModel, authUID, started, usage.Detail{}, true, 0, "cosy: "+reqErr.Error())
			return nil, fmt.Errorf("cosy: %w", reqErr)
		}
		stream, statusCode, _, streamErr = hostHTTPDoStream(httpReq)
		if streamErr != nil {
			publishUsage(req.Model, upstreamModel, authUID, started, usage.Detail{}, true, 0, streamErr.Error())
			return nil, fmt.Errorf("http_error: %w", streamErr)
		}
		if statusCode < 400 {
			break
		}
		payload, _ := io.ReadAll(newHostStreamReader(stream))
		stream.Close()
		signal, isQueue := detectQueueSignal(statusCode, payload)
		if !isQueue || !queueCfg.QueueWait || attempt >= maxQueueRetries {
			publishUsage(req.Model, upstreamModel, authUID, started, usage.Detail{}, true, statusCode, string(payload))
			reconcileAfterExecutorError(req.AuthID, statusCode, string(payload))
			if isQueue {
				return nil, queuedError(signal.Detail, time.Duration(signal.RetryAfter)*time.Second)
			}
			return nil, fmt.Errorf("upstream %d: %s", statusCode, truncateRedacted(string(payload), 200))
		}
		hostLogf("info", fmt.Sprintf("model queue: %s busy, waiting %ds (attempt %d)", upstreamModel, signal.RetryAfter, attempt+1))
		if waitErr := awaitQueueTurn(context.Background(), sa, newRequestSetID(), upstreamModel,
			time.Duration(queueCfg.QueueMaxWait)*time.Second,
			time.Duration(signal.RetryAfter)*time.Second, nil); waitErr != nil {
			publishUsage(req.Model, upstreamModel, authUID, started, usage.Detail{}, true, statusCode, waitErr.Error())
			return nil, queuedError(signal.Detail, time.Duration(queueCfg.QueueMaxWait)*time.Second)
		}
	}
	defer stream.Close()
	reader := rawDumpReader(newHostStreamReader(stream))
	completion, err := aggregateQoderSSE(reader, req.Model)
	if err != nil {
		publishUsage(req.Model, upstreamModel, authUID, started, usage.Detail{}, true, 0, err.Error())
		return nil, err
	}
	publishUsage(req.Model, upstreamModel, authUID, started, usageDetailFromCompletion(completion), false, 0, "")
	invalidateAccountCredits(req.AuthID, authUID)
	return okEnvelope(pluginapi.ExecutorResponse{Payload: completion})
}

// stripProviderPrefix removes the leading "qoder/" (or any "<provider>/")
// segment from a CPA-facing model name, leaving the bare alias/key.
func stripProviderPrefix(model string) string {
	if i := strings.Index(model, "/"); i > 0 {
		return model[i+1:]
	}
	return model
}

// executorStreamRequest wraps the host's executor.execute_stream RPC: the
// ExecutorRequest plus the async stream id the host uses to receive chunks.
type executorStreamRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func handleExecStream(raw []byte) ([]byte, error) {
	var req executorStreamRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	upstreamModel := cpaToUpstreamKey(stripProviderPrefix(req.Model))
	started := time.Now()
	authUID := ""
	if sa.Account.UID != "" {
		authUID = sa.Account.UID
	}

	// Build the Qoder body (template-based) and QoderEncoding-encode.
	bodyRaw := req.Payload
	if len(bodyRaw) == 0 {
		bodyRaw = req.OriginalRequest
	}
	qwReq := &openAIRequest{}
	if err := json.Unmarshal(bodyRaw, qwReq); err != nil && len(bodyRaw) > 0 {
		publishUsage(req.Model, upstreamModel, authUID, started, usage.Detail{}, true, 0, "payload parse: "+err.Error())
		return nil, fmt.Errorf("payload parse: %w", err)
	}
	body, err := buildQoderBody(qwReq, sa, upstreamModel, accountUserType(sa))
	if err != nil {
		publishUsage(req.Model, upstreamModel, authUID, started, usage.Detail{}, true, 0, "body build: "+err.Error())
		return nil, fmt.Errorf("body build: %w", err)
	}
	encodedBody := qoderEncode(body)

	headers := streamHeaders()
	sseFramed := clientNeedsSSEFrame(req.Metadata)

	// No async stream id → fall back to synchronous chunk collection.
	if req.StreamID == "" {
		collector := &sseUsageCollector{}
		chunks, statusCode, errCollect := collectUpstreamStreamQoder(encodedBody, sa, upstreamModel, sseFramed, collector)
		if errCollect != nil {
			publishUsage(req.Model, upstreamModel, authUID, started, usage.Detail{}, true, statusCode, errCollect.Error())
			return nil, errCollect
		}
		publishUsage(req.Model, upstreamModel, authUID, started, collector.detail(), false, 0, "")
		invalidateAccountCredits(req.AuthID, authUID)
		return okEnvelope(streamResponse{Headers: headers, Chunks: chunks})
	}

	// Async: return immediately with empty chunks. A goroutine pumps the upstream
	// and emits each chunk via host.stream.emit so the client sees true streaming.
	// Use context.Background() (not nil) so the request can be cancelled when the
	// client disconnects — otherwise the pump keeps reading a dead upstream until
	// sharedHTTPClient's 120s timeout, holding a pool slot the whole time.
	ctx, cancel := context.WithCancel(context.Background())
	chatURL := realmOf(sa).chatURL()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, chatURL, strings.NewReader(encodedBody))
	if err != nil {
		cancel()
		streamEmitError(req.StreamID, err.Error())
		streamClose(req.StreamID)
		return okEnvelope(streamResponse{Headers: headers})
	}
	if err := applyCosyHeaders(httpReq, sa, encodedBody, chatURL, upstreamModel, true); err != nil {
		cancel()
		streamEmitError(req.StreamID, "cosy: "+err.Error())
		streamClose(req.StreamID)
		return okEnvelope(streamResponse{Headers: headers})
	}
	go pumpUpstreamStream(func() (*http.Request, error) {
		return newChatRequest(ctx, sa, encodedBody, chatURL, upstreamModel)
	}, cancel, req.StreamID, sseFramed, req.Model, upstreamModel, authUID, started, req.AuthID, sa)
	return okEnvelope(streamResponse{Headers: headers})
}

// -----------------------------------------------------------------------------

func okEnvelope(v any) ([]byte, error) {
	result, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: result})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
