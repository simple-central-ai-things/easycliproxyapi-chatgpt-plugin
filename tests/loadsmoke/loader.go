//go:build linux && cgo

package loadsmoke

// C ABI loader for the built EasyCLI/CLIProxyAPI v8 plugin artifact. It is
// package code (not test code) because cgo cannot be used from _test.go files;
// the offline dry-run that exercises it lives in loadsmoke_test.go.

/*
#cgo LDFLAGS: -ldl
#include <dlfcn.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, char*, uint8_t*, size_t, cliproxy_buffer*);
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

extern int loadsmokeHostCall(void*, char*, uint8_t*, size_t, cliproxy_buffer*);

static void loadsmokeFreeBuffer(void* ptr, size_t len) {
	(void)len;
	free(ptr);
}

static void* loadsmokeOpen(const char* path) {
	return dlopen(path, RTLD_NOW | RTLD_LOCAL);
}

static const char* loadsmokeLastError(void) {
	const char* err = dlerror();
	return err == NULL ? "" : err;
}

// g_host must outlive init: the plugin stores the pointer for the whole
// session (the real host owns its host_api for the process lifetime).
static cliproxy_host_api g_host;

static int loadsmokeInit(void* handle, uint32_t abi, cliproxy_plugin_api* plug) {
	typedef int (*init_fn)(const cliproxy_host_api*, cliproxy_plugin_api*);
	init_fn fn = (init_fn)dlsym(handle, "cliproxy_plugin_init");
	if (fn == NULL) {
		return -1;
	}
	memset(&g_host, 0, sizeof(g_host));
	g_host.abi_version = abi;
	g_host.host_ctx = NULL;
	g_host.call = loadsmokeHostCall;
	g_host.free_buffer = loadsmokeFreeBuffer;
	return fn(&g_host, plug);
}

static int loadsmokeRPC(void* handle, char* method, uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	typedef int (*call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
	call_fn fn = (call_fn)dlsym(handle, "cliproxyPluginCall");
	if (fn == NULL) {
		return -1;
	}
	return fn(method, request, request_len, response);
}

static void loadsmokeRelease(void* handle, void* ptr, size_t len) {
	typedef void (*free_fn)(void*, size_t);
	free_fn fn = (free_fn)dlsym(handle, "cliproxyPluginFree");
	if (fn != NULL) {
		fn(ptr, len);
	}
}

static void loadsmokeShutdownPlugin(void* handle) {
	typedef void (*shutdown_fn)(void);
	shutdown_fn fn = (shutdown_fn)dlsym(handle, "cliproxyPluginShutdown");
	if (fn != NULL) {
		fn();
	}
}

static int loadsmokeReady(cliproxy_plugin_api* plug) {
	return plug->call != NULL && plug->free_buffer != NULL && plug->shutdown != NULL;
}

static int loadsmokeABI(cliproxy_plugin_api* plug) {
	return (int)plug->abi_version;
}

static void loadsmokeClose(void* handle) {
	dlclose(handle);
}
*/
import "C"

import (
	"fmt"
	"sync"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// HostFunc answers one host.* callback: method plus JSON request in, JSON
// envelope out. A non-nil error fails the callback (the plugin sees rc != 0).
type HostFunc func(method string, payload []byte) ([]byte, error)

var (
	hostMu   sync.Mutex
	hostFunc HostFunc
)

// SetHost installs the fake host callbacks for a dry-run. Pass nil to detach.
func SetHost(fn HostFunc) {
	hostMu.Lock()
	hostFunc = fn
	hostMu.Unlock()
}

func currentHostFunc() HostFunc {
	hostMu.Lock()
	defer hostMu.Unlock()
	return hostFunc
}

// loadsmokeHostCall is the host side of the plugin ABI. The plugin frees the
// returned buffer through host.free_buffer.
//
//export loadsmokeHostCall
func loadsmokeHostCall(_ unsafe.Pointer, method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil || response == nil {
		return 1
	}
	var payload []byte
	if request != nil && requestLen > 0 {
		payload = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	fn := currentHostFunc()
	if fn == nil {
		return 1
	}
	raw, err := fn(C.GoString(method), payload)
	if err != nil || len(raw) == 0 {
		return 1
	}
	response.ptr = C.CBytes(raw)
	response.len = C.size_t(len(raw))
	return 0
}

// Plugin is one loaded artifact.
type Plugin struct {
	handle unsafe.Pointer
	api    C.cliproxy_plugin_api
}

// Open dlopen()s the shared object.
func Open(path string) (*Plugin, error) {
	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))
	handle := C.loadsmokeOpen(cPath)
	if handle == nil {
		return nil, fmt.Errorf("dlopen %s: %s", path, C.GoString(C.loadsmokeLastError()))
	}
	return &Plugin{handle: handle}, nil
}

// Init calls cliproxy_plugin_init with the SDK ABI version and verifies the
// callbacks the host loader requires.
func (p *Plugin) Init() error {
	rc := C.loadsmokeInit(p.handle, C.uint32_t(pluginabi.ABIVersion), &p.api)
	if rc != 0 {
		return fmt.Errorf("cliproxy_plugin_init rc=%d (ABI mismatch or refused host)", rc)
	}
	if C.loadsmokeReady(&p.api) == 0 {
		return fmt.Errorf("plugin registered incomplete callbacks")
	}
	return nil
}

// ABI reports the ABI version the plugin wrote into its plugin_api.
func (p *Plugin) ABI() int { return int(C.loadsmokeABI(&p.api)) }

// RPC performs one cliproxyPluginCall round-trip. The returned bytes are
// copied out of the plugin-owned buffer, which is released through the
// plugin's own free export.
func (p *Plugin) RPC(method string, request []byte) ([]byte, error) {
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var reqPtr *C.uint8_t
	if len(request) > 0 {
		reqPtr = (*C.uint8_t)(C.CBytes(request))
		defer C.free(unsafe.Pointer(reqPtr))
	}
	var out C.cliproxy_buffer
	if rc := C.loadsmokeRPC(p.handle, cMethod, reqPtr, C.size_t(len(request)), &out); rc != 0 {
		return nil, fmt.Errorf("%s: cliproxyPluginCall rc=%d", method, rc)
	}
	if out.ptr == nil || out.len == 0 {
		return nil, nil
	}
	raw := C.GoBytes(out.ptr, C.int(out.len))
	C.loadsmokeRelease(p.handle, out.ptr, out.len)
	return raw, nil
}

// Shutdown calls the plugin's shutdown export (the loader calls it before
// dlclose); it is safe to call after plugin.shutdown has already run.
func (p *Plugin) Shutdown() { C.loadsmokeShutdownPlugin(p.handle) }

// Close dlclose()s the artifact. The host must have shut the plugin down and
// drained its callbacks first.
func (p *Plugin) Close() { C.loadsmokeClose(p.handle) }
