package main

/*
#include "cabi.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"net/http"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

// maxRequestBytes is a hard ceiling on one host->plugin request, independent
// of configuration (the configurable guard lives in Plugin.execute).
const maxRequestBytes = 1 << 30

// thePlugin is the process-wide plugin instance. Its only mutable state is an
// atomic config pointer; the host table lives in C behind atomics.
var thePlugin = newPlugin(cgoHost{})

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) (rc C.int) {
	defer func() {
		if recover() != nil {
			rc = 1
		}
	}()
	if host == nil || plugin == nil {
		return 1
	}
	// Refuse to load against a host speaking a different C ABI instead of
	// guessing at struct layouts.
	if uint32(host.abi_version) != pluginabi.ABIVersion || host.call == nil || host.free_buffer == nil {
		return 1
	}
	C.cpa_groq_store_host(host)
	C.cpa_groq_set_plugin_api(plugin, C.uint32_t(pluginabi.ABIVersion))
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) (rc C.int) {
	defer func() {
		if recover() != nil {
			rc = 1
			func() {
				defer func() { _ = recover() }()
				writeResponse(response, newError(codeInternal, http.StatusInternalServerError, "internal error").envelope())
			}()
		}
	}()
	if response == nil {
		return 1
	}
	response.ptr = nil
	response.len = 0
	if method == nil {
		writeResponse(response, (&pluginError{Code: "invalid_method", Message: "cpa-groq: method is required"}).envelope())
		return 1
	}
	name := C.GoString(method)
	if uint64(requestLen) > maxRequestBytes {
		writeResponse(response, newError(codeAudioTooLarge, http.StatusRequestEntityTooLarge, "request is too large").envelope())
		return 1
	}
	var req []byte
	if request != nil && requestLen > 0 {
		// A read-only view of host memory that is valid for this call only.
		// Everything derived from it is copied by encoding/json, and nothing
		// retains it after dispatch returns.
		req = unsafe.Slice((*byte)(unsafe.Pointer(request)), int(requestLen))
	}
	out, ok := thePlugin.dispatch(name, req)
	if !writeResponse(response, out) {
		return 1
	}
	if !ok {
		return 1
	}
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, length C.size_t) {
	defer func() { _ = recover() }()
	if ptr != nil {
		C.free(ptr)
	}
	_ = length
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	defer func() { _ = recover() }()
	// CPA waits for in-flight calls before shutdown, and every goroutine this
	// plugin starts is joined before its call returns, so clearing the table
	// cannot race with a host callback.
	C.cpa_groq_store_host(nil)
}

// writeResponse copies raw into C memory owned by the host until it calls
// cliproxyPluginFree. Returns false when nothing could be written.
func writeResponse(response *C.cliproxy_buffer, raw []byte) bool {
	if response == nil || len(raw) == 0 {
		return false
	}
	ptr := C.cpa_groq_copy_out(unsafe.Pointer(&raw[0]), C.size_t(len(raw)))
	if ptr == nil {
		return false
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
	return true
}

// cgoHost implements Host over the C host table.
type cgoHost struct{}

var errHostCallFailed = errors.New("host callback failed")

func (cgoHost) Call(method string, request []byte) ([]byte, error) {
	if C.cpa_groq_have_host() == 0 {
		return nil, errNoHost
	}
	// Both buffers are Go memory without Go pointers; the host copies them
	// (C.GoString / C.GoBytes) before returning and does not retain them.
	name := make([]byte, len(method)+1)
	copy(name, method)
	var reqPtr *C.uint8_t
	if len(request) > 0 {
		reqPtr = (*C.uint8_t)(unsafe.Pointer(&request[0]))
	}
	var resp C.cliproxy_buffer
	rc := C.cpa_groq_call_host((*C.char)(unsafe.Pointer(&name[0])), reqPtr, C.size_t(len(request)), &resp)
	var out []byte
	if resp.ptr != nil {
		if resp.len > 0 && uint64(resp.len) <= maxRequestBytes {
			out = C.GoBytes(resp.ptr, C.int(resp.len))
		}
		C.cpa_groq_free_host_buffer(resp.ptr, resp.len)
	}
	if rc != 0 && len(out) == 0 {
		return nil, fmt.Errorf("%w: %s returned %d", errHostCallFailed, method, int(rc))
	}
	return out, nil
}
