package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestRegisterAndReconfigure(t *testing.T) {
	host := newFakeHost()
	p := newPlugin(host)
	for _, method := range []string{pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure} {
		out, ok := p.dispatch(method, lifecycleJSON(t, testConfigYAML("language: en\n")))
		if !ok {
			t.Fatalf("%s failed: %s", method, out)
		}
		env := decodeEnvelope(t, out)
		var reg struct {
			SchemaVersion uint32             `json:"schema_version"`
			Metadata      pluginapi.Metadata `json:"metadata"`
			Capabilities  map[string]any     `json:"capabilities"`
		}
		if err := json.Unmarshal(env.Result, &reg); err != nil {
			t.Fatal(err)
		}
		if reg.SchemaVersion != 1 || reg.Metadata.Name != "cpa-groq" || reg.Metadata.Version != pluginVersion ||
			reg.Metadata.Author == "" || reg.Metadata.GitHubRepository == "" || len(reg.Metadata.ConfigFields) != len(knownKeys) {
			t.Fatalf("bad registration %+v", reg)
		}
		caps := reg.Capabilities
		if caps["executor"] != true || caps["model_registrar"] != true || caps["executor_model_scope"] != "static" {
			t.Fatalf("bad capabilities %v", caps)
		}
		if fmt.Sprint(caps["executor_input_formats"]) != "[gemini]" || fmt.Sprint(caps["executor_output_formats"]) != "[gemini]" {
			t.Fatalf("bad formats %v", caps)
		}
	}
	if p.cfg.Load().Language != "en" {
		t.Fatal("config not applied")
	}
	// Identical config must not log again; a changed one must.
	before := strings.Count(host.allLogText(), "configuration applied")
	p.dispatch(pluginabi.MethodPluginReconfigure, lifecycleJSON(t, testConfigYAML("language: en\n")))
	p.dispatch(pluginabi.MethodPluginReconfigure, lifecycleJSON(t, testConfigYAML("language: de\n")))
	after := strings.Count(host.allLogText(), "configuration applied")
	if before != 1 || after != 2 {
		t.Fatalf("config change logging: before=%d after=%d", before, after)
	}
}

func TestReconfigureInvalidKeepsPreviousConfig(t *testing.T) {
	host := newFakeHost()
	p := newTestPlugin(t, host, "language: en\n")
	out, ok := p.dispatch(pluginabi.MethodPluginReconfigure, lifecycleJSON(t, []byte("api_key: "+testAPIKey+"\ntimeout_seconds: -1\n")))
	if ok {
		t.Fatal("invalid config must fail")
	}
	env := decodeEnvelope(t, out)
	if env.OK || env.Error.Code != codeInvalidConfig || strings.Contains(string(out), testAPIKey) {
		t.Fatalf("bad error envelope: %s", out)
	}
	if p.cfg.Load().Language != "en" {
		t.Fatal("previous config must stay active")
	}
	if strings.Contains(host.allLogText(), testAPIKey) {
		t.Fatal("api key logged")
	}
}

func TestRegisterMalformed(t *testing.T) {
	p := newPlugin(newFakeHost())
	if _, ok := p.dispatch(pluginabi.MethodPluginRegister, []byte("{broken")); ok {
		t.Fatal("malformed lifecycle request must fail")
	}
	if _, ok := p.dispatch(pluginabi.MethodPluginRegister, nil); ok {
		t.Fatal("register without api_key must fail")
	}
}

func TestModelRegisterAndIdentifier(t *testing.T) {
	p := newTestPlugin(t, newFakeHost(), "")
	out, ok := p.dispatch(pluginabi.MethodModelRegister, []byte(`{"Plugin":{}}`))
	if !ok {
		t.Fatal(string(out))
	}
	var resp pluginapi.ModelRegistrationResponse
	if err := json.Unmarshal(decodeEnvelope(t, out).Result, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Provider != providerKey || len(resp.Models) != 2 || resp.Models[0].ID != "groq-whisper-large-v3" || resp.Models[1].ID != "groq-whisper-large-v3-turbo" {
		t.Fatalf("models %+v", resp)
	}
	out, _ = p.dispatch(pluginabi.MethodExecutorIdentifier, nil)
	if !strings.Contains(string(out), `"identifier":"cpa-groq"`) {
		t.Fatalf("identifier %s", out)
	}
	for _, m := range []string{pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown} {
		if out, ok := p.dispatch(m, nil); !ok || !decodeEnvelope(t, out).OK {
			t.Fatalf("%s should succeed: %s", m, out)
		}
	}
}

func successDo(t *testing.T, captured *hostHTTPRequest) func(req hostHTTPRequest, canceled <-chan struct{}) ([]byte, error) {
	return func(req hostHTTPRequest, _ <-chan struct{}) ([]byte, error) {
		*captured = req
		return groqReply(200, map[string][]string{"Content-Type": {"application/json"}}, sampleVerbose), nil
	}
}

func TestExecuteSuccess(t *testing.T) {
	host := newFakeHost()
	var got hostHTTPRequest
	host.doFn = successDo(t, &got)
	p := newTestPlugin(t, host, "prompt: default vocabulary\n")
	audio := oggStub()
	body := geminiBody(t, "audio/ogg", audio, map[string]any{"cpa_groq": map[string]any{"prompt": "Ada, Groq"}})

	out, ok := p.dispatch(pluginabi.MethodExecutorExecute, executorRequestJSON(t, "groq-whisper-large-v3-turbo", body))
	if !ok {
		t.Fatalf("execute failed: %s", out)
	}
	var resp struct {
		Payload []byte `json:"Payload"`
	}
	if err := json.Unmarshal(decodeEnvelope(t, out).Result, &resp); err != nil {
		t.Fatal(err)
	}
	g := decodeGemini(t, resp.Payload)
	if *g.Candidates[0].Content.Parts[0].Text != "Merhaba dünya. <ok> & bitti." {
		t.Fatalf("transcript %q", *g.Candidates[0].Content.Parts[0].Text)
	}

	// Outbound request checks.
	if got.Method != "POST" || got.URL != "https://api.groq.com/openai/v1/audio/transcriptions" {
		t.Fatalf("method/url %s %s", got.Method, got.URL)
	}
	if got.HostCallbackID != "cb-1" || got.OperationID == "" {
		t.Fatalf("callback/operation ids %q %q", got.HostCallbackID, got.OperationID)
	}
	if got.Headers["Authorization"][0] != "Bearer "+testAPIKey {
		t.Fatal("authorization header missing")
	}
	fields, file, data := parseForm(t, got.Body, got.Headers["Content-Type"][0])
	if fields["model"] != "whisper-large-v3-turbo" || fields["language"] != "tr" || fields["prompt"] != "Ada, Groq" ||
		fields["temperature"] != "0" || fields["response_format"] != "verbose_json" {
		t.Fatalf("form fields %v", fields)
	}
	if file.FileName() != "audio.ogg" || !bytes.Equal(data, audio) {
		t.Fatal("file part wrong")
	}

	logs := host.allLogText()
	for _, secret := range []string{testAPIKey, "Merhaba", "Ada, Groq", "default vocabulary", base64.StdEncoding.EncodeToString(audio)} {
		if strings.Contains(logs, secret) {
			t.Fatalf("log contains sensitive content %q:\n%s", secret, logs)
		}
	}
	if !strings.Contains(logs, "transcription ok") {
		t.Fatalf("expected a success log line:\n%s", logs)
	}
}

func TestExecuteStreamSuccess(t *testing.T) {
	host := newFakeHost()
	var got hostHTTPRequest
	host.doFn = successDo(t, &got)
	p := newTestPlugin(t, host, "")
	out, ok := p.dispatch(pluginabi.MethodExecutorExecuteStream, executorRequestJSON(t, "groq-whisper-large-v3", geminiBody(t, "audio/ogg", oggStub(), nil)))
	if !ok {
		t.Fatalf("stream failed: %s", out)
	}
	var resp struct {
		Chunks []struct {
			Payload []byte `json:"Payload"`
		} `json:"chunks"`
	}
	if err := json.Unmarshal(decodeEnvelope(t, out).Result, &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Chunks) != 1 {
		t.Fatalf("want exactly one chunk, got %d", len(resp.Chunks))
	}
	g := decodeGemini(t, resp.Chunks[0].Payload)
	if g.Candidates[0].FinishReason != "STOP" || g.UsageMetadata.TotalTokenCount == 0 {
		t.Fatalf("final chunk must carry finishReason and usage: %s", resp.Chunks[0].Payload)
	}
	if strings.Contains(string(resp.Chunks[0].Payload), "\n") {
		t.Fatal("stream chunk must be a single JSON line (CPA adds the SSE framing)")
	}
}

func TestExecuteUpstreamErrors(t *testing.T) {
	cases := []struct {
		status  int
		headers map[string][]string
		body    string
		want    int
		substr  string
	}{
		{400, nil, `{"error":{"message":"could not process file - is it a valid media file?","type":"invalid_request_error"}}`, 400, "valid media file"},
		{401, nil, `{"error":{"message":"Invalid API Key","type":"invalid_request_error","code":"invalid_api_key"}}`, 401, "Invalid API Key"},
		{403, nil, `{"error":{"message":"Forbidden"}}`, 403, "Forbidden"},
		{404, nil, `{"error":{"message":"The model does not exist"}}`, 404, "does not exist"},
		{413, nil, `{"error":{"message":"Request Entity Too Large"}}`, 413, "Too Large"},
		{429, map[string][]string{"Retry-After": {"12"}}, `{"error":{"message":"Rate limit reached"}}`, 429, "retry after 12s"},
		{500, nil, `internal`, 500, "HTTP 500"},
		{503, nil, `{"error":{"message":"over capacity"}}`, 503, "over capacity"},
		{520, nil, ``, 502, "HTTP 520"},
	}
	for _, c := range cases {
		t.Run(fmt.Sprint(c.status), func(t *testing.T) {
			host := newFakeHost()
			host.doFn = func(hostHTTPRequest, <-chan struct{}) ([]byte, error) {
				return groqReply(c.status, c.headers, c.body), nil
			}
			p := newTestPlugin(t, host, "")
			for _, method := range []string{pluginabi.MethodExecutorExecute, pluginabi.MethodExecutorExecuteStream} {
				out, ok := p.dispatch(method, executorRequestJSON(t, "groq-whisper-large-v3", geminiBody(t, "audio/ogg", oggStub(), nil)))
				if ok {
					t.Fatalf("%s: expected failure", method)
				}
				env := decodeEnvelope(t, out)
				if env.Error.HTTPStatus != c.want || !strings.Contains(env.Error.Message, c.substr) {
					t.Fatalf("%s: got %d %q; want %d containing %q", method, env.Error.HTTPStatus, env.Error.Message, c.want, c.substr)
				}
			}
		})
	}
}

func TestExecuteInvalidUpstreamBody(t *testing.T) {
	host := newFakeHost()
	host.doFn = func(hostHTTPRequest, <-chan struct{}) ([]byte, error) {
		return groqReply(200, nil, "<html>captive portal</html>"), nil
	}
	p := newTestPlugin(t, host, "")
	out, _ := p.dispatch(pluginabi.MethodExecutorExecute, executorRequestJSON(t, "groq-whisper-large-v3", geminiBody(t, "audio/ogg", oggStub(), nil)))
	if env := decodeEnvelope(t, out); env.Error == nil || env.Error.HTTPStatus != 502 {
		t.Fatalf("expected 502, got %s", out)
	}
}

func TestExecuteTimeoutCancelsOperation(t *testing.T) {
	host := newFakeHost()
	host.doFn = func(_ hostHTTPRequest, canceled <-chan struct{}) ([]byte, error) {
		select {
		case <-canceled:
			return errResult("host_call_failed", `execute host http request: Post "https://api.groq.com/openai/v1/audio/transcriptions": context canceled`, 499), nil
		case <-time.After(10 * time.Second):
			return nil, errors.New("watchdog never canceled the operation")
		}
	}
	p := newTestPlugin(t, host, "timeout_seconds: 1\n")
	start := time.Now()
	out, ok := p.dispatch(pluginabi.MethodExecutorExecute, executorRequestJSON(t, "groq-whisper-large-v3", geminiBody(t, "audio/ogg", oggStub(), nil)))
	elapsed := time.Since(start)
	if ok {
		t.Fatal("expected timeout failure")
	}
	env := decodeEnvelope(t, out)
	if env.Error.HTTPStatus != http.StatusGatewayTimeout || env.Error.Code != codeUpstreamTimeout {
		t.Fatalf("got %+v", env.Error)
	}
	if elapsed < 900*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("timeout not bounded as configured: %v", elapsed)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.cancels) != 1 || host.cancels[0] != host.requests[0].OperationID {
		t.Fatalf("cancel not sent for the operation: %v", host.cancels)
	}
}

func TestExecuteClientCancel(t *testing.T) {
	host := newFakeHost()
	host.doFn = func(hostHTTPRequest, <-chan struct{}) ([]byte, error) {
		return errResult("host_call_failed", `execute host http request: Post "https://api.groq.com/openai/v1/audio/transcriptions": context canceled`, 499), nil
	}
	p := newTestPlugin(t, host, "")
	out, _ := p.dispatch(pluginabi.MethodExecutorExecute, executorRequestJSON(t, "groq-whisper-large-v3", geminiBody(t, "audio/ogg", oggStub(), nil)))
	env := decodeEnvelope(t, out)
	if env.Error.Code != codeCanceled || env.Error.HTTPStatus != 0 || env.Error.Message != "context canceled" {
		t.Fatalf("got %+v", env.Error)
	}
	if strings.Contains(host.allLogText(), "transcription failed") {
		t.Fatal("client cancellations are not failures worth a warning")
	}
}

func TestExecuteOperationOpenAfterClientLeft(t *testing.T) {
	host := newFakeHost()
	host.openErr = errResult("host_call_failed", "host callback ID is not open", 0)
	p := newTestPlugin(t, host, "")
	out, _ := p.dispatch(pluginabi.MethodExecutorExecute, executorRequestJSON(t, "groq-whisper-large-v3", geminiBody(t, "audio/ogg", oggStub(), nil)))
	if env := decodeEnvelope(t, out); env.Error.Code != codeCanceled {
		t.Fatalf("got %+v", env.Error)
	}
	host.openErr = errResult("host_call_failed", "host http operation bridge is unavailable", 0)
	out, _ = p.dispatch(pluginabi.MethodExecutorExecute, executorRequestJSON(t, "groq-whisper-large-v3", geminiBody(t, "audio/ogg", oggStub(), nil)))
	if env := decodeEnvelope(t, out); env.Error.HTTPStatus != 500 {
		t.Fatalf("got %+v", env.Error)
	}
}

func TestExecuteNetworkError(t *testing.T) {
	host := newFakeHost()
	host.doFn = func(hostHTTPRequest, <-chan struct{}) ([]byte, error) {
		return errResult("host_call_failed", `execute host http request: Post "https://api.groq.com/openai/v1/audio/transcriptions": dial tcp: lookup api.groq.com: no such host`, 0), nil
	}
	p := newTestPlugin(t, host, "")
	out, _ := p.dispatch(pluginabi.MethodExecutorExecute, executorRequestJSON(t, "groq-whisper-large-v3", geminiBody(t, "audio/ogg", oggStub(), nil)))
	env := decodeEnvelope(t, out)
	if env.Error.HTTPStatus != 502 || env.Error.Code != codeUpstreamNetwork || !strings.Contains(env.Error.Message, "no such host") {
		t.Fatalf("got %+v", env.Error)
	}
}

func TestExecuteRequestFaultsNeverReachGroq(t *testing.T) {
	host := newFakeHost()
	host.doFn = func(hostHTTPRequest, <-chan struct{}) ([]byte, error) {
		t.Error("Groq must not be called for invalid requests")
		return nil, errors.New("unexpected")
	}
	p := newTestPlugin(t, host, "max_audio_bytes: 4096\n")
	cases := map[string][]byte{
		"garbage rpc":     []byte("\x00\x01garbage"),
		"garbage payload": executorRequestJSON(t, "groq-whisper-large-v3", []byte("{{{{")),
		"oversized audio": executorRequestJSON(t, "groq-whisper-large-v3", geminiBody(t, "audio/ogg", append(oggStub(), make([]byte, 8192)...), nil)),
		"random bytes":    executorRequestJSON(t, "groq-whisper-large-v3", geminiBody(t, "application/octet-stream", bytes.Repeat([]byte{0x5a}, 512), nil)),
	}
	for name, raw := range cases {
		out, ok := p.dispatch(pluginabi.MethodExecutorExecute, raw)
		env := decodeEnvelope(t, out)
		if ok || env.Error == nil || (env.Error.HTTPStatus != 400 && env.Error.HTTPStatus != 413) {
			t.Fatalf("%s: got %s", name, out)
		}
	}
	// The raw-size guard rejects before decoding: 4*4096 + 16 MiB + 1.
	huge := make([]byte, 4*4096+rawRequestOverhead+1)
	out, _ := p.dispatch(pluginabi.MethodExecutorExecute, huge)
	if env := decodeEnvelope(t, out); env.Error.HTTPStatus != 413 {
		t.Fatalf("raw guard: %s", out)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if host.callCount[pluginabi.MethodHostHTTPOperationOpen] != 0 {
		t.Fatal("no host HTTP operation may be opened for rejected requests")
	}
}

func TestUnsupportedMethods(t *testing.T) {
	p := newTestPlugin(t, newFakeHost(), "")
	for method, want := range map[string]int{
		pluginabi.MethodExecutorCountTokens: 400,
		pluginabi.MethodExecutorHTTPRequest: 400,
		"something.new":                     0,
	} {
		out, ok := p.dispatch(method, []byte("{}"))
		env := decodeEnvelope(t, out)
		if ok || env.OK || env.Error.HTTPStatus != want {
			t.Fatalf("%s: %s", method, out)
		}
	}
}

func TestNotConfigured(t *testing.T) {
	p := newPlugin(newFakeHost())
	out, ok := p.dispatch(pluginabi.MethodExecutorExecute, executorRequestJSON(t, "groq-whisper-large-v3", geminiBody(t, "audio/ogg", oggStub(), nil)))
	if ok || decodeEnvelope(t, out).Error.Code != codeNotConfigured {
		t.Fatalf("got %s", out)
	}
}

func TestDispatchRecoversFromPanics(t *testing.T) {
	host := newFakeHost()
	p := newTestPlugin(t, host, "")
	hook := func(string) { panic("boom") }
	testHookDispatch.Store(&hook)
	defer testHookDispatch.Store(nil)
	out, ok := p.dispatch(pluginabi.MethodExecutorExecute, nil)
	if ok {
		t.Fatal("panic must be reported as failure")
	}
	env := decodeEnvelope(t, out)
	if env.Error.Code != codeInternal || env.Error.HTTPStatus != 500 {
		t.Fatalf("got %+v", env.Error)
	}
	if !strings.Contains(host.allLogText(), "recovered from panic") {
		t.Fatal("panic should be logged")
	}
}

func TestHostPanicsAreContained(t *testing.T) {
	// A panic inside the host transport while on the request goroutine.
	host := newFakeHost()
	var got hostHTTPRequest
	host.doFn = successDo(t, &got)
	p := newTestPlugin(t, host, "")
	host.panicOn = pluginabi.MethodHostHTTPDo
	out, ok := p.dispatch(pluginabi.MethodExecutorExecute, executorRequestJSON(t, "groq-whisper-large-v3", geminiBody(t, "audio/ogg", oggStub(), nil)))
	if ok || decodeEnvelope(t, out).Error.Code != codeInternal {
		t.Fatalf("got %s", out)
	}

	// A panic inside the watchdog goroutine (host.http.cancel) must not crash
	// the process; the request still ends as a timeout.
	host2 := newFakeHost()
	host2.doFn = func(_ hostHTTPRequest, canceled <-chan struct{}) ([]byte, error) {
		select {
		case <-canceled:
		case <-time.After(1500 * time.Millisecond):
		}
		return errResult("host_call_failed", "context canceled", 499), nil
	}
	p2 := newTestPlugin(t, host2, "timeout_seconds: 1\n")
	host2.panicOn = pluginabi.MethodHostHTTPCancel
	out, _ = p2.dispatch(pluginabi.MethodExecutorExecute, executorRequestJSON(t, "groq-whisper-large-v3", geminiBody(t, "audio/ogg", oggStub(), nil)))
	if env := decodeEnvelope(t, out); env.Error.HTTPStatus != http.StatusGatewayTimeout {
		t.Fatalf("got %s", out)
	}

	// A panicking host.log never affects the request.
	host3 := newFakeHost()
	host3.doFn = successDo(t, &got)
	p3 := newTestPlugin(t, host3, "")
	host3.panicOn = pluginabi.MethodHostLog
	if out, ok := p3.dispatch(pluginabi.MethodExecutorExecute, executorRequestJSON(t, "groq-whisper-large-v3", geminiBody(t, "audio/ogg", oggStub(), nil))); !ok {
		t.Fatalf("log panic broke the request: %s", out)
	}
}

func TestConcurrentExecuteAndReconfigure(t *testing.T) {
	host := newFakeHost()
	host.doFn = func(req hostHTTPRequest, _ <-chan struct{}) ([]byte, error) {
		time.Sleep(time.Millisecond)
		return groqReply(200, nil, sampleVerbose), nil
	}
	p := newTestPlugin(t, host, "")
	var wg sync.WaitGroup
	errs := make(chan string, 200)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			method := pluginabi.MethodExecutorExecute
			if i%2 == 0 {
				method = pluginabi.MethodExecutorExecuteStream
			}
			out, ok := p.dispatch(method, executorRequestJSON(t, "groq-whisper-large-v3", geminiBody(t, "audio/wav", sineWAV(0.05, 300), nil)))
			if !ok {
				errs <- string(out)
			}
		}(i)
	}
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p.dispatch(pluginabi.MethodPluginReconfigure, lifecycleJSON(t, testConfigYAML(fmt.Sprintf("timeout_seconds: %d\n", 10+i))))
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("concurrent execute failed: %s", e)
	}
}
