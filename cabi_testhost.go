//go:build cabitest

// This file is compiled only with -tags cabitest. It provides a C-ABI test
// host so the exported entry points and the cgo host transport can be
// exercised in-process. It is never part of a release build.

package main

/*
#include "cabi.h"
extern int cpaGroqTestHostCall(void*, char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cpaGroqTestHostFree(void*, size_t);
*/
import "C"

import (
	"sync"
	"unsafe"
)

var (
	testHostMu      sync.Mutex
	testHostReplies = map[string][]byte{}
	testHostMethods []string
	testHostFreed   int
	testHostAPI     *C.cliproxy_host_api
)

//export cpaGroqTestHostCall
func cpaGroqTestHostCall(hostCtx unsafe.Pointer, method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	name := C.GoString(method)
	if request != nil && requestLen > 0 {
		_ = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	testHostMu.Lock()
	testHostMethods = append(testHostMethods, name)
	reply := testHostReplies[name]
	testHostMu.Unlock()
	if len(reply) > 0 && response != nil {
		response.ptr = C.cpa_groq_copy_out(unsafe.Pointer(&reply[0]), C.size_t(len(reply)))
		response.len = C.size_t(len(reply))
	}
	return 0
}

//export cpaGroqTestHostFree
func cpaGroqTestHostFree(ptr unsafe.Pointer, length C.size_t) {
	testHostMu.Lock()
	testHostFreed++
	testHostMu.Unlock()
	C.free(ptr)
}

func setTestHostReply(method string, reply []byte) {
	testHostMu.Lock()
	defer testHostMu.Unlock()
	testHostReplies[method] = reply
}

func testHostCalls() ([]string, int) {
	testHostMu.Lock()
	defer testHostMu.Unlock()
	return append([]string(nil), testHostMethods...), testHostFreed
}

// cabiTestInit calls cliproxy_plugin_init with a host table built to spec.
func cabiTestInit(abi uint32, nilHost, nilPlugin, nilCall bool) (rc int, pluginABI uint32, complete bool) {
	if testHostAPI == nil {
		testHostAPI = (*C.cliproxy_host_api)(C.malloc(C.size_t(unsafe.Sizeof(C.cliproxy_host_api{}))))
	}
	testHostAPI.abi_version = C.uint32_t(abi)
	testHostAPI.host_ctx = nil
	testHostAPI.call = C.cliproxy_host_call_fn(C.cpaGroqTestHostCall)
	testHostAPI.free_buffer = C.cliproxy_host_free_fn(C.cpaGroqTestHostFree)
	if nilCall {
		testHostAPI.call = nil
	}
	plugin := (*C.cliproxy_plugin_api)(C.calloc(1, C.size_t(unsafe.Sizeof(C.cliproxy_plugin_api{}))))
	defer C.free(unsafe.Pointer(plugin))
	host := testHostAPI
	if nilHost {
		host = nil
	}
	target := plugin
	if nilPlugin {
		target = nil
	}
	rc = int(cliproxy_plugin_init(host, target))
	complete = plugin.call != nil && plugin.free_buffer != nil && plugin.shutdown != nil
	return rc, uint32(plugin.abi_version), complete
}

// cabiTestCall drives cliproxyPluginCall exactly as the host does: C strings,
// C buffers, then plugin free_buffer on the response.
func cabiTestCall(method *string, request []byte, requestLen int, nilResponse bool) (int, []byte) {
	var cMethod *C.char
	if method != nil {
		cMethod = C.CString(*method)
		defer C.free(unsafe.Pointer(cMethod))
	}
	var cReq *C.uint8_t
	if len(request) > 0 {
		cReq = (*C.uint8_t)(C.CBytes(request))
		defer C.free(unsafe.Pointer(cReq))
	}
	var resp *C.cliproxy_buffer
	if !nilResponse {
		resp = (*C.cliproxy_buffer)(C.calloc(1, C.size_t(unsafe.Sizeof(C.cliproxy_buffer{}))))
		defer C.free(unsafe.Pointer(resp))
	}
	rc := int(cliproxyPluginCall(cMethod, cReq, C.size_t(requestLen), resp))
	var out []byte
	if resp != nil && resp.ptr != nil {
		out = C.GoBytes(resp.ptr, C.int(resp.len))
		cliproxyPluginFree(resp.ptr, resp.len)
	}
	return rc, out
}

func cabiTestShutdown() {
	cliproxyPluginShutdown()
}
