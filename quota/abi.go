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

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* entire_stored_host;

static void entire_store_host_api(const cliproxy_host_api* host) {
	entire_stored_host = host;
}

static int entire_host_api_available(void) {
	return (entire_stored_host != NULL && entire_stored_host->call != NULL) ? 1 : 0;
}

static int entire_call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (entire_stored_host == NULL || entire_stored_host->call == NULL) {
		return 1;
	}
	return entire_stored_host->call(entire_stored_host->host_ctx, method, request, request_len, response);
}

static void entire_free_host_buffer(void* ptr, size_t len) {
	if (entire_stored_host != NULL && entire_stored_host->free_buffer != NULL && ptr != NULL) {
		entire_stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"context"
	"encoding/json"
	"unsafe"
)

// abiVersion is the native C ABI shape implemented by this plugin.
const abiVersion uint32 = 1

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.entire_store_host_api(host)
	plugin.abi_version = C.uint32_t(abiVersion)
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
		writeResponse(response, encodeErrorEnvelope(newPluginError(codeInvalidMethod, "method is required", 0)))
		return 1
	}
	rawRequest := goBytes(request, requestLen)
	raw, errHandle := handleMethod(C.GoString(method), rawRequest)
	if errHandle != nil {
		writeResponse(response, encodeErrorEnvelope(errHandle))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, length C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = length
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	shutdownRuntime()
}

func goBytes(ptr *C.uint8_t, length C.size_t) []byte {
	if ptr == nil || length == 0 {
		return nil
	}
	return append([]byte(nil), unsafe.Slice((*byte)(unsafe.Pointer(ptr)), int(length))...)
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

// callHost invokes one host RPC and returns the plugin-facing error, if any.
// The host envelope is unwrapped so callers see the plain result payload.
func callHost(method string, payload []byte) (json.RawMessage, error) {
	return callHostCtx(context.Background(), method, payload)
}

// callHostCtx is callHost with an explicit caller context. The C ABI itself is
// context-free; the context only lets a caller stop before crossing into the
// host once it has already been cancelled (renewal under a lock, for example).
func callHostCtx(ctx context.Context, method string, payload []byte) (json.RawMessage, error) {
	if ctx != nil {
		if errCtx := ctx.Err(); errCtx != nil {
			return nil, newPluginError(codeHostCallFailed, "host callback "+method+" cancelled", 0)
		}
	}
	if C.entire_host_api_available() == 0 {
		return nil, newPluginError(codeHostUnavailable, "host callback API is not available", 0)
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var rawRequest *C.uint8_t
	if len(payload) > 0 {
		rawRequest = (*C.uint8_t)(C.CBytes(payload))
		defer C.free(unsafe.Pointer(rawRequest))
	}

	var response C.cliproxy_buffer
	rc := C.entire_call_host_api(cMethod, rawRequest, C.size_t(len(payload)), &response)
	// Copy the payload out before releasing it: the host owns the buffer and may
	// reuse the memory as soon as it is returned.
	var raw []byte
	if response.ptr != nil && response.len > 0 {
		raw = C.GoBytes(response.ptr, C.int(response.len))
		C.entire_free_host_buffer(response.ptr, response.len)
	}
	if rc != 0 || len(raw) == 0 {
		return nil, newPluginError(codeHostCallFailed, "host callback "+method+" failed", 0)
	}
	return decodeHostEnvelope(method, raw)
}

// decodeHostEnvelope validates and unwraps a host JSON envelope.
func decodeHostEnvelope(method string, raw []byte) (json.RawMessage, error) {
	var envelope hostEnvelope
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		return nil, newPluginError(codeHostCallFailed, "host callback "+method+" returned malformed JSON", 0)
	}
	if !envelope.OK {
		message := "host callback " + method + " failed"
		code := codeHostCallFailed
		if envelope.Error != nil {
			if trimmed := trimForMessage(envelope.Error.Message); trimmed != "" {
				message = trimmed
			}
			if trimmed := trimForMessage(envelope.Error.Code); trimmed != "" {
				code = trimmed
			}
		}
		return nil, newPluginError(code, redactSecrets(message), 0)
	}
	return envelope.Result, nil
}
