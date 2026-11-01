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
*/
import "C"

import (
	"unsafe"

	"github.com/local/cli-proxy-usage-meter/internal/plugin"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

var runtime = plugin.New()

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, out *C.cliproxy_plugin_api) C.int {
	if host == nil || out == nil || host.abi_version != C.uint32_t(pluginabi.ABIVersion) {
		return 1
	}
	out.abi_version = C.uint32_t(pluginabi.ABIVersion)
	out.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	out.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	out.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, size C.size_t, response *C.cliproxy_buffer) (status C.int) {
	if response == nil {
		return 1
	}
	response.ptr = nil
	response.len = 0
	name := ""
	defer func() {
		if recover() != nil {
			if name == pluginabi.MethodUsageHandle {
				runtime.Dropped()
			}
			status = 1
		}
	}()
	if method == nil {
		return 1
	}
	name = C.GoString(method)
	if size > C.size_t(plugin.MaxBody) {
		if name == pluginabi.MethodUsageHandle {
			runtime.Dropped()
			write(response, []byte(`{"ok":true,"result":{}}`))
			return 0
		}
		return 1
	}
	if size > 0 && request == nil {
		return 1
	}
	var raw []byte
	if size > 0 {
		raw = C.GoBytes(unsafe.Pointer(request), C.int(size))
	}
	write(response, runtime.Call(name, raw))
	return 0
}
func write(out *C.cliproxy_buffer, raw []byte) {
	if len(raw) > 0 {
		out.ptr = C.CBytes(raw)
		out.len = C.size_t(len(raw))
	}
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, size C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() { runtime.Close() }
