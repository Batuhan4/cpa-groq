/* Definitions live here, not in a cgo preamble: a Go file with //export may
 * only declare C symbols, and a static in a preamble would be duplicated per
 * translation unit. */
#include <string.h>

#include "cabi.h"
#include "_cgo_export.h"

/* The host API table is owned by CPA and stays valid until CPA has called our
 * shutdown. It is published with release/acquire atomics. */
static const cliproxy_host_api* cpa_groq_host = NULL;

void cpa_groq_store_host(const cliproxy_host_api* host) {
	__atomic_store_n(&cpa_groq_host, host, __ATOMIC_RELEASE);
}

int cpa_groq_have_host(void) {
	const cliproxy_host_api* host = __atomic_load_n(&cpa_groq_host, __ATOMIC_ACQUIRE);
	return host != NULL && host->call != NULL && host->free_buffer != NULL;
}

int cpa_groq_call_host(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	const cliproxy_host_api* host = __atomic_load_n(&cpa_groq_host, __ATOMIC_ACQUIRE);
	if (host == NULL || host->call == NULL) {
		return -1;
	}
	return host->call(host->host_ctx, method, request, request_len, response);
}

void cpa_groq_free_host_buffer(void* ptr, size_t len) {
	const cliproxy_host_api* host = __atomic_load_n(&cpa_groq_host, __ATOMIC_ACQUIRE);
	if (ptr != NULL && host != NULL && host->free_buffer != NULL) {
		host->free_buffer(ptr, len);
	}
}

/* Unlike C.CBytes, returns NULL instead of aborting the process when malloc fails. */
void* cpa_groq_copy_out(const void* src, size_t len) {
	if (src == NULL || len == 0) {
		return NULL;
	}
	void* dst = malloc(len);
	if (dst != NULL) {
		memcpy(dst, src, len);
	}
	return dst;
}

void cpa_groq_set_plugin_api(cliproxy_plugin_api* plugin, uint32_t abi_version) {
	plugin->abi_version = abi_version;
	plugin->call = (cliproxy_plugin_call_fn)cliproxyPluginCall;
	plugin->free_buffer = (cliproxy_plugin_free_fn)cliproxyPluginFree;
	plugin->shutdown = (cliproxy_plugin_shutdown_fn)cliproxyPluginShutdown;
}
