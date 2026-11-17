package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct { void* ptr; size_t len; } cliproxy_buffer;
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
static const cliproxy_host_api* stored_host;
static void store_host(const cliproxy_host_api* host) { stored_host = host; }
static int call_host(const char* method, const uint8_t* body, size_t len, cliproxy_buffer* out) {
    if (stored_host == NULL || stored_host->call == NULL) return 1;
    return stored_host->call(stored_host->host_ctx, method, body, len, out);
}
static void free_host(void* ptr, size_t len) {
    if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL)
        stored_host->free_buffer(ptr, len);
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var controller = newController()

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if host == nil || plugin == nil {
		return 1
	}
	C.store_host(host)
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response == nil {
		return 1
	}
	response.ptr, response.len = nil, 0
	var raw []byte
	if request != nil && requestLen > 0 && requestLen <= 1<<30 {
		raw = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	var result any
	var errHandle error
	if method == nil {
		errHandle = fmt.Errorf("method is required")
	} else {
		result, errHandle = controller.handle(C.GoString(method), raw)
	}
	envelope := pluginabi.Envelope{OK: errHandle == nil}
	if errHandle != nil {
		envelope.Error = pluginabi.NewError("session_quota_control", errHandle.Error())
	} else {
		envelope.Result, errHandle = json.Marshal(result)
		if errHandle != nil {
			envelope.OK = false
			envelope.Error = pluginabi.NewError("encode_response", errHandle.Error())
		}
	}
	encoded, errMarshal := json.Marshal(envelope)
	if errMarshal != nil {
		return 1
	}
	response.ptr = C.CBytes(encoded)
	response.len = C.size_t(len(encoded))
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() { controller.stop() }

func callHost(method string, request, result any) error {
	raw, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		return errMarshal
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	body := C.CBytes(raw)
	defer C.free(body)
	var out C.cliproxy_buffer
	code := C.call_host(cMethod, (*C.uint8_t)(body), C.size_t(len(raw)), &out)
	if out.ptr != nil {
		defer C.free_host(out.ptr, out.len)
	}
	if out.ptr == nil || out.len == 0 || out.len > 1<<30 {
		return fmt.Errorf("host callback %s failed (%d)", method, int(code))
	}
	var envelope pluginabi.Envelope
	if errDecode := json.Unmarshal(C.GoBytes(out.ptr, C.int(out.len)), &envelope); errDecode != nil {
		return fmt.Errorf("decode %s: %w", method, errDecode)
	}
	if !envelope.OK || code != 0 {
		if envelope.Error != nil {
			return fmt.Errorf("%s: %w", method, envelope.Error)
		}
		return fmt.Errorf("host callback %s failed", method)
	}
	return json.Unmarshal(envelope.Result, result)
}

type affinityCall struct {
	pluginapi.SessionAffinityRequest
	CallbackID string `json:"host_callback_id"`
}

type interceptCall struct {
	pluginapi.RequestInterceptRequest
	CallbackID string `json:"host_callback_id"`
}
