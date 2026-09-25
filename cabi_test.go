//go:build cabitest

package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

// These tests run the real exported C entry points (cliproxy_plugin_init,
// cliproxyPluginCall, cliproxyPluginFree, cliproxyPluginShutdown) and the cgo
// host transport against an in-process C-ABI test host.
func TestCABI(t *testing.T) {
	t.Run("init validation", func(t *testing.T) {
		if rc, _, _ := cabiTestInit(1, true, false, false); rc == 0 {
			t.Fatal("nil host must be refused")
		}
		if rc, _, _ := cabiTestInit(1, false, true, false); rc == 0 {
			t.Fatal("nil plugin table must be refused")
		}
		if rc, _, _ := cabiTestInit(2, false, false, false); rc == 0 {
			t.Fatal("foreign ABI version must be refused")
		}
		if rc, _, _ := cabiTestInit(1, false, false, true); rc == 0 {
			t.Fatal("host without call must be refused")
		}
		rc, abi, complete := cabiTestInit(1, false, false, false)
		if rc != 0 || abi != 1 || !complete {
			t.Fatalf("valid init: rc=%d abi=%d complete=%v", rc, abi, complete)
		}
	})

	t.Run("call argument validation", func(t *testing.T) {
		m := pluginabi.MethodExecutorIdentifier
		if rc, _ := cabiTestCall(&m, nil, 0, true); rc == 0 {
			t.Fatal("nil response buffer must fail")
		}
		rc, out := cabiTestCall(nil, nil, 0, false)
		if rc == 0 || !strings.Contains(string(out), "method is required") {
			t.Fatalf("nil method: rc=%d %s", rc, out)
		}
		m = pluginabi.MethodExecutorExecute
		rc, out = cabiTestCall(&m, []byte("x"), maxRequestBytes+1, false)
		if rc == 0 || !strings.Contains(string(out), `"http_status":413`) {
			t.Fatalf("oversized length: rc=%d %s", rc, out)
		}
	})

	t.Run("register through the C ABI logs through the host", func(t *testing.T) {
		setTestHostReply(pluginabi.MethodHostLog, okResult(emptyResult{}))
		m := pluginabi.MethodPluginRegister
		req := lifecycleJSON(t, testConfigYAML("language: de\n"))
		rc, out := cabiTestCall(&m, req, len(req), false)
		if rc != 0 || !decodeEnvelope(t, out).OK {
			t.Fatalf("register: rc=%d %s", rc, out)
		}
		methods, freed := testHostCalls()
		if len(methods) == 0 || methods[len(methods)-1] != pluginabi.MethodHostLog || freed == 0 {
			t.Fatalf("host.log not routed through the host table: %v freed=%d", methods, freed)
		}
	})

	t.Run("execute through the C ABI", func(t *testing.T) {
		setTestHostReply(pluginabi.MethodHostHTTPOperationOpen, okResult(hostOperationOpenResponse{OperationID: "op-c"}))
		setTestHostReply(pluginabi.MethodHostHTTPCancel, okResult(emptyResult{}))
		setTestHostReply(pluginabi.MethodHostHTTPDo, groqReply(200, nil, sampleVerbose))
		m := pluginabi.MethodExecutorExecute
		req := executorRequestJSON(t, "groq-whisper-large-v3", geminiBody(t, "audio/ogg", oggStub(), nil))
		rc, out := cabiTestCall(&m, req, len(req), false)
		if rc != 0 || !strings.Contains(string(out), `"ok":true`) {
			t.Fatalf("execute: rc=%d %s", rc, out)
		}
		setTestHostReply(pluginabi.MethodHostHTTPDo, groqReply(401, nil, `{"error":{"message":"Invalid API Key"}}`))
		rc, out = cabiTestCall(&m, req, len(req), false)
		if rc == 0 || !strings.Contains(string(out), `"http_status":401`) {
			t.Fatalf("401: rc=%d %s", rc, out)
		}
	})

	t.Run("panic never crosses the C boundary", func(t *testing.T) {
		hook := func(string) { panic("injected") }
		testHookDispatch.Store(&hook)
		defer testHookDispatch.Store(nil)
		m := pluginabi.MethodExecutorExecute
		rc, out := cabiTestCall(&m, []byte("{}"), 2, false)
		if rc == 0 || !strings.Contains(string(out), codeInternal) {
			t.Fatalf("rc=%d %s", rc, out)
		}
	})

	t.Run("shutdown detaches the host", func(t *testing.T) {
		cabiTestShutdown()
		if _, err := (cgoHost{}).Call(pluginabi.MethodHostLog, []byte("{}")); !errors.Is(err, errNoHost) {
			t.Fatalf("host must be unavailable after shutdown, got %v", err)
		}
		// Re-init after shutdown works (CPA may reload the same library).
		if rc, _, _ := cabiTestInit(1, false, false, false); rc != 0 {
			t.Fatal("re-init failed")
		}
		cabiTestShutdown()
	})
}
