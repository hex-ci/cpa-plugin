// host_bridge.go owns the shared HTTP boundary. Without a plugin proxy it
// preserves the CPA host bridge and legacy direct fallbacks. With proxy-url
// configured it uses the immutable plugin proxy client and never falls back.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// sharedHTTPClient is the inherited direct client used by OAuth and when the
// CPA host bridge is unavailable or intentionally bypassed on Windows. Explicit
// proxy mode uses the client stored in proxyState instead.
func sharedHTTPClient() *http.Client {
	httpClientOnce.Do(func() {
		// No cookie jar here: auth is carried by Bearer headers, and a shared
		// jar would leak upstream set-cookie state across accounts (multi-account
		// deployments could cross-contaminate sessions). Only the short-lived
		// login clients get a jar.
		sharedClient = &http.Client{
			Timeout: 120 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        20,
				IdleConnTimeout:     90 * time.Second,
				MaxIdleConnsPerHost: 5,
			},
		}
	})
	return sharedClient
}

// hostHTTPResponse is the plugin-side view of an HTTP response that came back
// through the host bridge. Body is fully buffered (matches the historical
// io.ReadAll(resp.Body) usage pattern in billing / models / usage callers).
type hostHTTPResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// rpcHostHTTPRequestWire mirrors internal/pluginhost/host_callbacks.go's
// rpcHostHTTPRequest on the wire. The "request" sub-object is the actual HTTP
// call; the flat method/url/headers/body fields are an alternate form we don't
// use (host prefers Request when present).
type rpcHostHTTPRequestWire struct {
	HostCallbackID string            `json:"host_callback_id,omitempty"`
	Request        *rpcHostHTTPInner `json:"request,omitempty"`
}

type rpcHostHTTPBufferedResponseWire struct {
	StatusCodeSnake  int                 `json:"status_code"`
	StatusCodePascal int                 `json:"StatusCode"`
	Headers          map[string][]string `json:"headers,omitempty"`
	Body             []byte              `json:"body,omitempty"`
}

type rpcHostHTTPInner struct {
	Method  string              `json:"method,omitempty"`
	URL     string              `json:"url,omitempty"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    []byte              `json:"body,omitempty"`
}

func decodeBufferedHostHTTPResponse(result []byte) (*hostHTTPResponse, error) {
	var wire rpcHostHTTPBufferedResponseWire
	if err := json.Unmarshal(result, &wire); err != nil {
		return nil, fmt.Errorf("decode host.http.do response: %w", err)
	}
	statusCode := wire.StatusCodeSnake
	if statusCode == 0 {
		statusCode = wire.StatusCodePascal
	}
	return &hostHTTPResponse{
		StatusCode: statusCode,
		Headers:    http.Header(wire.Headers),
		Body:       wire.Body,
	}, nil
}

func newRPCHostHTTPRequestWire(req *http.Request, body []byte, callbackID string) rpcHostHTTPRequestWire {
	return rpcHostHTTPRequestWire{
		HostCallbackID: strings.TrimSpace(callbackID),
		Request: &rpcHostHTTPInner{
			Method:  req.Method,
			URL:     req.URL.String(),
			Headers: map[string][]string(req.Header),
			Body:    body,
		},
	}
}

type rpcHostHTTPStreamResponseWire struct {
	StatusCode int                         `json:"status_code"`
	Headers    map[string][]string         `json:"headers,omitempty"`
	StreamID   string                      `json:"stream_id,omitempty"`
	Chunks     []pluginapi.HTTPStreamChunk `json:"chunks,omitempty"`
}

type rpcHostHTTPStreamReadResponseWire struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

// hostBridgeUnwrap unwraps the pluginabi.Envelope returned by host RPC and
// returns the inner Result payload. Returns an error when the envelope itself
// signals failure (ok=false) or is malformed.
func hostBridgeUnwrap(raw []byte, method string) (json.RawMessage, error) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("%s: decode envelope: %w", method, err)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("%s: host error %s: %s", method, env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("%s: host returned not-ok", method)
	}
	return env.Result, nil
}

// hostBridgeAvailable reports whether host.http.* RPC is wired up. False in
// unit tests (no hostAPI) and when the host binary predates the bridge.
func hostBridgeAvailable() bool {
	return hostAPI != nil && hostAPI.call != nil
}

// hostHTTPDo performs a buffered upstream call using the active routing state.
// Request bodies are only buffered when inherited host-bridge routing requires
// serialization; explicit proxy mode sends the original request once.
//
// Fallback: when the host bridge is unavailable (unit tests, host older than
// v7.2.x without the http bridge), we route through sharedHTTPClient directly.
// This keeps the plugin functional in dev/test contexts while preferring the
// compliant path in production. Once a bridge call is attempted, failures are
// returned instead of replaying the request directly; many upstream calls are
// POSTs and may already have executed.
func hostHTTPDo(req *http.Request) (*hostHTTPResponse, error) {
	return hostHTTPDoWithCallback(req, "")
}

func hostHTTPDoWithCallback(req *http.Request, callbackID string) (*hostHTTPResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("nil request")
	}
	return hostHTTPDoWithStateAndCallback(currentProxyState(), req, callbackID)
}

func hostHTTPDoWithState(state *proxyRoutingState, req *http.Request) (*hostHTTPResponse, error) {
	return hostHTTPDoWithStateAndCallback(state, req, "")
}

func hostHTTPDoWithStateAndCallback(state *proxyRoutingState, req *http.Request, callbackID string) (*hostHTTPResponse, error) {
	if state.mode == proxyModeBlocked || state.mode == proxyModeExplicit && state.client == nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, blockedProxyError()
	}
	if state.mode == proxyModeExplicit {
		return hostHTTPDoWithClient(state.client, req)
	}

	var bodyBytes []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, fmt.Errorf("read request body: %w", err)
		}
		_ = req.Body.Close()
		bodyBytes = b
	}
	// Windows stack movement mitigation: nested host calls during synchronous
	// RPCs (model.for_auth, management.handle) cause the host stack to move,
	// rendering the stack response pointer dangling and causing "unexpected
	// end of JSON input". Bypass host bridge on Windows and make direct HTTP
	// calls.
	if !hostBridgeAvailable() || runtime.GOOS == "windows" {
		return hostHTTPDoDirect(req, bodyBytes)
	}
	wire := newRPCHostHTTPRequestWire(req, bodyBytes, callbackID)
	raw, err := hostCall(pluginabi.MethodHostHTTPDo, mustJSON(wire))
	if err != nil {
		return nil, fmt.Errorf("host.http.do: %w", err)
	}
	result, err := hostBridgeUnwrap(raw, pluginabi.MethodHostHTTPDo)
	if err != nil {
		return nil, err
	}
	return decodeBufferedHostHTTPResponse(result)
}

// hostHTTPDoDirect executes the request via the plugin's own http.Client.
// Used as a fallback when the host bridge is unavailable (unit tests).
func hostHTTPDoDirect(req *http.Request, bodyBytes []byte) (*hostHTTPResponse, error) {
	// Rebuild the request since req.Body was already consumed.
	newReq, err := http.NewRequestWithContext(req.Context(), req.Method, req.URL.String(), bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	newReq.Header = req.Header.Clone()
	return hostHTTPDoWithClient(sharedHTTPClient(), newReq)
}

func rejectHTTPRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

func doPluginOwnedHTTP(client *http.Client, req *http.Request) (*http.Response, error) {
	requestClient := *client
	requestClient.CheckRedirect = rejectHTTPRedirect
	return requestClient.Do(req)
}

func hostHTTPDoWithClient(client *http.Client, req *http.Request) (*hostHTTPResponse, error) {
	resp, err := doPluginOwnedHTTP(client, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return &hostHTTPResponse{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header.Clone(),
		Body:       raw,
	}, nil
}

// hostHTTPStream is a handle for an in-flight host-bridged stream. Read returns
// the next chunk; Close aborts the upstream.
//
// Two modes:
//   - Bridged: streamID set, Read/Close forward to host RPC.
//   - Direct: direct is the live upstream response body.
type hostHTTPStream struct {
	streamID  string
	direct    io.ReadCloser
	directErr error
}

// hostHTTPDoStream opens a streaming call via the host bridge. The host owns
// the actual http.Response body; we pull chunks via hostHTTPStreamRead.
//
// Falls back to direct http.Client.Do when the inherited bridge is unavailable.
// Direct and explicit-proxy streams retain the live response body so callers can
// consume flushed SSE data before EOF.
func hostHTTPDoStream(req *http.Request) (*hostHTTPStream, int, http.Header, error) {
	return hostHTTPDoStreamWithCallback(req, "")
}

func hostHTTPDoStreamWithCallback(req *http.Request, callbackID string) (*hostHTTPStream, int, http.Header, error) {
	if req == nil {
		return nil, 0, nil, fmt.Errorf("nil request")
	}
	state := currentProxyState()
	if state.mode == proxyModeBlocked || state.mode == proxyModeExplicit && state.client == nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, 0, nil, blockedProxyError()
	}
	if state.mode == proxyModeExplicit {
		return hostHTTPDoStreamWithClient(state.client, req)
	}

	var bodyBytes []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, 0, nil, fmt.Errorf("read request body: %w", err)
		}
		_ = req.Body.Close()
		bodyBytes = b
	}
	if !hostBridgeAvailable() {
		return hostHTTPDoStreamDirect(req, bodyBytes)
	}
	wire := newRPCHostHTTPRequestWire(req, bodyBytes, callbackID)
	raw, err := hostCall(pluginabi.MethodHostHTTPDoStream, mustJSON(wire))
	if err != nil {
		return nil, 0, nil, fmt.Errorf("host.http.do_stream: %w", err)
	}
	result, err := hostBridgeUnwrap(raw, pluginabi.MethodHostHTTPDoStream)
	if err != nil {
		return nil, 0, nil, err
	}
	var resp rpcHostHTTPStreamResponseWire
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, 0, nil, fmt.Errorf("decode host.http.do_stream response: %w", err)
	}
	if resp.StreamID == "" {
		return nil, resp.StatusCode, http.Header(resp.Headers), fmt.Errorf("host stream bridge unavailable")
	}
	return &hostHTTPStream{streamID: resp.StreamID}, resp.StatusCode, http.Header(resp.Headers), nil
}

// hostHTTPDoStreamDirect is the test-only inherited fallback.
func hostHTTPDoStreamDirect(req *http.Request, bodyBytes []byte) (*hostHTTPStream, int, http.Header, error) {
	newReq, err := http.NewRequestWithContext(req.Context(), req.Method, req.URL.String(), bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, 0, nil, err
	}
	newReq.Header = req.Header.Clone()
	return hostHTTPDoStreamWithClient(sharedHTTPClient(), newReq)
}

func hostHTTPDoStreamWithClient(client *http.Client, req *http.Request) (*hostHTTPStream, int, http.Header, error) {
	resp, err := doPluginOwnedHTTP(client, req)
	if err != nil {
		return nil, 0, nil, err
	}
	return &hostHTTPStream{direct: resp.Body}, resp.StatusCode, resp.Header.Clone(), nil
}

// Read pulls the next chunk. Returns (payload, done, err). done=true means the
// stream ended cleanly; err non-nil means upstream or bridge error.
func (s *hostHTTPStream) Read() ([]byte, bool, error) {
	if s == nil {
		return nil, true, fmt.Errorf("stream closed")
	}
	// Direct mode reads one live upstream chunk at a time.
	if s.direct != nil {
		if s.directErr != nil {
			err := s.directErr
			s.directErr = nil
			return nil, true, err
		}
		buf := make([]byte, 32*1024)
		n, err := s.direct.Read(buf)
		if n > 0 {
			if err == io.EOF {
				return buf[:n], true, nil
			}
			if err != nil {
				s.directErr = err
			}
			return buf[:n], false, nil
		}
		if err == io.EOF {
			return nil, true, nil
		}
		if err != nil {
			return nil, true, err
		}
		return nil, false, nil
	}
	if s.streamID == "" {
		return nil, true, fmt.Errorf("stream closed")
	}
	raw, err := hostCall(pluginabi.MethodHostHTTPStreamRead, mustJSON(map[string]any{"stream_id": s.streamID}))
	if err != nil {
		return nil, true, err
	}
	result, err := hostBridgeUnwrap(raw, pluginabi.MethodHostHTTPStreamRead)
	if err != nil {
		return nil, true, err
	}
	var resp rpcHostHTTPStreamReadResponseWire
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, true, fmt.Errorf("decode host.http.stream_read response: %w", err)
	}
	if resp.Error != "" {
		return nil, true, fmt.Errorf("%s", resp.Error)
	}
	return resp.Payload, resp.Done, nil
}

// Close aborts the upstream stream. Always safe to call (idempotent on host).
func (s *hostHTTPStream) Close() {
	if s == nil {
		return
	}
	if s.direct != nil {
		_ = s.direct.Close()
		s.direct = nil
		s.directErr = nil
		return
	}
	if s.streamID == "" {
		return
	}
	_, _ = hostCall(pluginabi.MethodHostHTTPStreamClose, mustJSON(map[string]any{"stream_id": s.streamID}))
	s.streamID = ""
}

// hostStreamReader adapts a hostHTTPStream to io.Reader so existing
// bufio.Scanner / io.ReadAll call sites work unchanged. The host bridge emits
// arbitrary 32KB chunks (not SSE lines), so line framing must be re-assembled
// by the consumer — Scanner handles that for us.
type hostStreamReader struct {
	s    *hostHTTPStream
	buf  []byte // leftover from previous chunk
	done bool
	err  error
}

func newHostStreamReader(s *hostHTTPStream) *hostStreamReader {
	return &hostStreamReader{s: s}
}

func (r *hostStreamReader) Read(p []byte) (int, error) {
	// Drain buffered bytes first.
	if len(r.buf) > 0 {
		n := copy(p, r.buf)
		r.buf = r.buf[n:]
		return n, nil
	}
	if r.done {
		if r.err != nil {
			return 0, r.err
		}
		return 0, io.EOF
	}
	chunk, done, err := r.s.Read()
	if err != nil {
		r.done = true
		r.err = err
		return 0, err
	}
	if len(chunk) > 0 {
		n := copy(p, chunk)
		if n < len(chunk) {
			r.buf = append(r.buf, chunk[n:]...)
		}
		if done {
			r.done = true
		}
		return n, nil
	}
	if done {
		r.done = true
		return 0, io.EOF
	}
	// Empty chunk, not done — recurse to fetch next.
	return r.Read(p)
}

// mustJSON marshals v and panics on error — the wire structs above are always
// marshalable, so any failure here is a programming bug.
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
