/* C ABI shared with CLIProxyAPI's plugin host (sdk/pluginabi, ABI version 1). */
#ifndef CPA_GROQ_CABI_H
#define CPA_GROQ_CABI_H

#include <stddef.h>
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

/* Internal helpers: hidden so the only symbol CPA can see is cliproxy_plugin_init
 * (plus the Go export wrappers). */
#define CPA_GROQ_INTERNAL __attribute__((visibility("hidden")))

CPA_GROQ_INTERNAL void cpa_groq_store_host(const cliproxy_host_api* host);
CPA_GROQ_INTERNAL int cpa_groq_have_host(void);
CPA_GROQ_INTERNAL int cpa_groq_call_host(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response);
CPA_GROQ_INTERNAL void cpa_groq_free_host_buffer(void* ptr, size_t len);
CPA_GROQ_INTERNAL void* cpa_groq_copy_out(const void* src, size_t len);
CPA_GROQ_INTERNAL void cpa_groq_set_plugin_api(cliproxy_plugin_api* plugin, uint32_t abi_version);

#endif
