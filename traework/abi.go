// abi.go is the cgo boundary: the host/plugin API structs, the entry points the
// host resolves with dlsym(), the host-call helper and the RPC envelope.
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

// Wrappers so Go can use the host function-pointer table through cgo.
static int tw_call_host(cliproxy_host_api* api, const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	return api->call(api->host_ctx, method, request, request_len, response);
}
static void tw_free_host_buffer(cliproxy_host_api* api, void* ptr, size_t len) {
	api->free_buffer(ptr, len);
}

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"encoding/json"
	"errors"
	"fmt"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

// main is required by the c-shared build mode; the host never calls it.
func main() {}

// hostAPI is captured at init; every outbound host RPC goes through it.
var hostAPI *C.cliproxy_host_api

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
		writeResponse(response, errorEnvelope("handler_error", errHandle.Error()))
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
	// Intentional no-op: the host dlclose()s this library immediately after
	// this call, so touching Go runtime state here (mutexes, channels,
	// goroutine synchronization) can fault inside cgo. The login listeners and
	// janitor goroutines hold nothing that outlives the process.
}

// hostCall invokes a host RPC method through the function-pointer table.
func hostCall(method string, request []byte) ([]byte, error) {
	if hostAPI == nil || hostAPI.call == nil {
		return nil, errors.New("host API unavailable")
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
	rc := C.tw_call_host(hostAPI, cMethod, (*C.uint8_t)(cReq), reqLen, &resp)
	if resp.ptr == nil || resp.len == 0 {
		if rc != 0 {
			return nil, fmt.Errorf("host call %s returned %d", method, int(rc))
		}
		return nil, nil
	}
	out := C.GoBytes(resp.ptr, C.int(resp.len))
	C.tw_free_host_buffer(hostAPI, resp.ptr, resp.len)
	if rc != 0 {
		return out, fmt.Errorf("host call %s returned %d", method, int(rc))
	}
	return out, nil
}

// hostLogInfo writes one line into the host log. Level info on purpose: the
// production host runs with debug disabled, so a debug-level line would be
// invisible exactly where the login path needs to be diagnosable. Never pass
// codes or tokens as fields — the message and field values are user-visible.
func hostLogInfo(message string, fields map[string]any) {
	payload := map[string]any{"level": "info", "message": message}
	if len(fields) > 0 {
		payload["fields"] = fields
	}
	_, _ = hostCall(pluginabi.MethodHostLog, mustJSON(payload))
}

// writeResponse copies an RPC result into the host-provided buffer; the host
// releases it with cliproxyPluginFree.
func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	response.ptr = C.CBytes(raw)
	response.len = C.size_t(len(raw))
}

// envelope is the result frame the host expects from every RPC method.
type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

func okEnvelope(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		return errorEnvelope("encode_error", err.Error())
	}
	out, err := json.Marshal(envelope{OK: true, Result: raw})
	if err != nil {
		return nil
	}
	return out
}

func errorEnvelope(code, message string) []byte {
	return errorEnvelopeWithStatus(code, message, 0)
}

func errorEnvelopeWithStatus(code, message string, status int) []byte {
	out, err := json.Marshal(envelope{Error: &envelopeError{Code: code, Message: message, HTTPStatus: status}})
	if err != nil {
		return nil
	}
	return out
}

type identifierResponse struct {
	Identifier string `json:"identifier"`
}
