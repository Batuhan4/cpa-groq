package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

// testAPIKey is a synthetic, obviously fake key used to prove redaction.
// It deliberately carries Groq's key prefix (assembled at run time) so the
// pattern-based redaction is exercised too.
const testAPIKey = groqKeyPrefix + "TESTONLYnotarealkey0123456789abcdef"

// sineWAV returns a synthetic 16 kHz mono 16-bit PCM WAV tone. Test audio is
// always generated; no recorded speech is committed to this repository.
func sineWAV(seconds float64, freq float64) []byte {
	const rate = 16000
	n := int(seconds * rate)
	pcm := make([]byte, n*2)
	for i := 0; i < n; i++ {
		v := int16(math.Sin(2*math.Pi*freq*float64(i)/rate) * 8000)
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(v)) //nolint:gosec // two's-complement reinterpretation is intended
	}
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+len(pcm))) //nolint:gosec // synthetic clips are far below 4 GiB
	b.WriteString("WAVEfmt ")
	_ = binary.Write(&b, binary.LittleEndian, uint32(16))
	_ = binary.Write(&b, binary.LittleEndian, uint16(1))      // PCM
	_ = binary.Write(&b, binary.LittleEndian, uint16(1))      // mono
	_ = binary.Write(&b, binary.LittleEndian, uint32(rate))   // sample rate
	_ = binary.Write(&b, binary.LittleEndian, uint32(rate*2)) // byte rate
	_ = binary.Write(&b, binary.LittleEndian, uint16(2))      // block align
	_ = binary.Write(&b, binary.LittleEndian, uint16(16))     // bits per sample
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(pcm))) //nolint:gosec // synthetic clips are far below 4 GiB
	b.Write(pcm)
	return b.Bytes()
}

// oggStub is a synthetic buffer carrying the Ogg capture pattern.
func oggStub() []byte {
	return append([]byte("OggS\x00\x02"), bytes.Repeat([]byte{0x11}, 64)...)
}

func geminiBody(t *testing.T, mime string, audio []byte, extra map[string]any) []byte {
	t.Helper()
	body := map[string]any{
		"contents": []any{map[string]any{
			"role": "user",
			"parts": []any{
				map[string]any{"text": "Transcribe this audio verbatim in Turkish. Do not translate."},
				map[string]any{"inline_data": map[string]any{"mime_type": mime, "data": base64.StdEncoding.EncodeToString(audio)}},
			},
		}},
	}
	for k, v := range extra {
		body[k] = v
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func executorRequestJSON(t *testing.T, model string, payload []byte) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"Model":            model,
		"Format":           "gemini",
		"Stream":           false,
		"Payload":          payload,
		"OriginalRequest":  payload,
		"SourceFormat":     "gemini",
		"host_callback_id": "cb-1",
		"Headers":          map[string][]string{"X-Goog-Api-Key": {"client-key"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func testConfigYAML(extra string) []byte {
	return []byte("enabled: true\npriority: 0\napi_key: " + testAPIKey + "\n" + extra)
}

func mustConfig(t *testing.T, extra string) *Config {
	t.Helper()
	cfg, err := ParseConfig(testConfigYAML(extra))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	return cfg
}

func lifecycleJSON(t *testing.T, yaml []byte) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"config_yaml": yaml, "schema_version": 6})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func decodeEnvelope(t *testing.T, raw []byte) pluginabi.Envelope {
	t.Helper()
	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope is not JSON: %v: %s", err, raw)
	}
	return env
}

// fakeHost emulates CPA's host callbacks used by the plugin.
type fakeHost struct {
	mu sync.Mutex

	// doFn produces the host.http.do reply. It receives the decoded request
	// and a channel that is closed when the operation is canceled.
	doFn func(req hostHTTPRequest, canceled <-chan struct{}) ([]byte, error)

	openErr   []byte // error envelope for operation_open, if set
	panicOn   string // method name that panics
	nextOp    int
	ops       map[string]chan struct{}
	requests  []hostHTTPRequest
	cancels   []string
	logs      []hostLogRequest
	callCount map[string]int
}

func newFakeHost() *fakeHost {
	return &fakeHost{ops: map[string]chan struct{}{}, callCount: map[string]int{}}
}

func okResult(v any) []byte {
	raw, _ := json.Marshal(v)
	env, _ := json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
	return env
}

func errResult(code, msg string, status int) []byte {
	raw, _ := pluginabi.NewErrorEnvelope(code, msg, status)
	return raw
}

func (f *fakeHost) Call(method string, request []byte) ([]byte, error) {
	f.mu.Lock()
	f.callCount[method]++
	panicOn := f.panicOn
	f.mu.Unlock()
	if panicOn == method {
		panic("fake host panic in " + method)
	}
	switch method {
	case pluginabi.MethodHostHTTPOperationOpen:
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.openErr != nil {
			return f.openErr, nil
		}
		f.nextOp++
		id := fmt.Sprintf("op-%d", f.nextOp)
		f.ops[id] = make(chan struct{})
		return okResult(hostOperationOpenResponse{OperationID: id}), nil
	case pluginabi.MethodHostHTTPCancel:
		var req hostCancelRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, err
		}
		f.mu.Lock()
		f.cancels = append(f.cancels, req.OperationID)
		if ch, ok := f.ops[req.OperationID]; ok {
			close(ch)
			delete(f.ops, req.OperationID)
		}
		f.mu.Unlock()
		return okResult(emptyResult{}), nil
	case pluginabi.MethodHostHTTPDo:
		var req hostHTTPRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, err
		}
		f.mu.Lock()
		f.requests = append(f.requests, req)
		ch := f.ops[req.OperationID]
		fn := f.doFn
		f.mu.Unlock()
		if ch == nil {
			return errResult("host_call_failed", fmt.Sprintf("host http operation %q is not open", req.OperationID), 0), nil
		}
		if fn == nil {
			return nil, errors.New("no doFn configured")
		}
		out, err := fn(req, ch)
		f.mu.Lock()
		delete(f.ops, req.OperationID)
		f.mu.Unlock()
		return out, err
	case pluginabi.MethodHostLog:
		var req hostLogRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, err
		}
		f.mu.Lock()
		f.logs = append(f.logs, req)
		f.mu.Unlock()
		return okResult(emptyResult{}), nil
	default:
		return errResult("unsupported", "unsupported host callback "+method, 0), nil
	}
}

func (f *fakeHost) allLogText() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b strings.Builder
	for _, l := range f.logs {
		raw, _ := json.Marshal(l)
		b.Write(raw)
		b.WriteByte('\n')
	}
	return b.String()
}

// groqReply builds a host.http.do success envelope wrapping a Groq response.
func groqReply(status int, headers map[string][]string, body string) []byte {
	return okResult(hostHTTPResponse{StatusCode: status, Headers: headers, Body: []byte(body)})
}

const sampleVerbose = `{
  "task": "transcribe",
  "language": "Turkish",
  "duration": 2.5,
  "text": " Merhaba dünya. <ok> & bitti.",
  "segments": [
    {"id": 0, "seek": 0, "start": 0, "end": 1.2, "text": " Merhaba dünya.", "tokens": [50364, 1, 2, 3], "temperature": 0, "avg_logprob": -0.2, "compression_ratio": 1.1, "no_speech_prob": 0.01},
    {"id": 1, "seek": 0, "start": 1.2, "end": 2.5, "text": " <ok> & bitti.", "tokens": [4, 5, 6], "temperature": 0, "avg_logprob": -0.3, "compression_ratio": 1.0, "no_speech_prob": 0.02}
  ],
  "x_groq": {"id": "req_01TESTID"}
}`

func newTestPlugin(t *testing.T, host *fakeHost, extraConfig string) *Plugin {
	t.Helper()
	p := newPlugin(host)
	out, ok := p.dispatch(pluginabi.MethodPluginRegister, lifecycleJSON(t, testConfigYAML(extraConfig)))
	if !ok {
		t.Fatalf("register failed: %s", out)
	}
	return p
}
