package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestParseExecutorRequestSnakeCase(t *testing.T) {
	cfg := mustConfig(t, "")
	audio := sineWAV(0.2, 440)
	raw := executorRequestJSON(t, "groq-whisper-large-v3", geminiBody(t, "audio/wav", audio, nil))
	in, cb, perr := parseExecutorRequest(raw, cfg)
	if perr != nil {
		t.Fatalf("unexpected error: %v", perr)
	}
	if cb != "cb-1" {
		t.Fatalf("callback id %q", cb)
	}
	if in.Model.Upstream != "whisper-large-v3" || in.Kind != kindWAV || !bytes.Equal(in.Audio, audio) {
		t.Fatalf("wrong input: model=%s kind=%v", in.Model.Upstream, in.Kind)
	}
	if in.Language != "tr" {
		t.Fatalf("default language not applied: %q", in.Language)
	}
	// The Gemini-style instruction in the text part must never become the Whisper prompt.
	if in.Prompt != "" {
		t.Fatalf("text part leaked into prompt: %q", in.Prompt)
	}
}

func TestParseExecutorRequestCamelCase(t *testing.T) {
	cfg := mustConfig(t, "prompt: Ada\n")
	audio := oggStub()
	payload, _ := json.Marshal(map[string]any{
		"contents": []any{map[string]any{"parts": []any{
			map[string]any{"inlineData": map[string]any{"mimeType": "audio/ogg", "data": base64.StdEncoding.EncodeToString(audio)}},
		}}},
	})
	in, _, perr := parseExecutorRequest(executorRequestJSON(t, "groq-whisper-large-v3-turbo", payload), cfg)
	if perr != nil {
		t.Fatal(perr)
	}
	if in.Model.Upstream != "whisper-large-v3-turbo" || in.Kind != kindOgg || in.Prompt != "Ada" {
		t.Fatalf("wrong input %+v", in.Model)
	}
}

func TestParseExecutorRequestOverrides(t *testing.T) {
	cfg := mustConfig(t, "prompt: default hint\n")
	body := geminiBody(t, "audio/ogg", oggStub(), map[string]any{
		"cpa_groq": map[string]any{"language": "en-GB", "prompt": "  Kerem, Ada  "},
	})
	in, _, perr := parseExecutorRequest(executorRequestJSON(t, "groq-whisper-large-v3", body), cfg)
	if perr != nil {
		t.Fatal(perr)
	}
	if in.Language != "en" || in.Prompt != "Kerem, Ada" {
		t.Fatalf("overrides not applied: %q %q", in.Language, in.Prompt)
	}

	body = geminiBody(t, "audio/ogg", oggStub(), map[string]any{
		"cpa_groq": map[string]any{"language": "auto", "prompt": ""},
	})
	in, _, perr = parseExecutorRequest(executorRequestJSON(t, "groq-whisper-large-v3", body), cfg)
	if perr != nil {
		t.Fatal(perr)
	}
	if in.Language != "" || in.Prompt != "" {
		t.Fatalf("explicit auto/empty overrides not applied: %q %q", in.Language, in.Prompt)
	}

	body = geminiBody(t, "audio/ogg", oggStub(), map[string]any{"cpa_groq": nil})
	if _, _, perr = parseExecutorRequest(executorRequestJSON(t, "groq-whisper-large-v3", body), cfg); perr != nil {
		t.Fatalf("null extension must be accepted: %v", perr)
	}
}

func TestParseExecutorRequestFirstInlineDataWins(t *testing.T) {
	cfg := mustConfig(t, "")
	first, second := oggStub(), sineWAV(0.1, 200)
	payload, _ := json.Marshal(map[string]any{
		"contents": []any{
			map[string]any{"parts": []any{map[string]any{"text": "hi"}}},
			map[string]any{"parts": []any{
				map[string]any{"inline_data": map[string]any{"mime_type": "audio/ogg", "data": base64.StdEncoding.EncodeToString(first)}},
				map[string]any{"inline_data": map[string]any{"mime_type": "audio/wav", "data": base64.StdEncoding.EncodeToString(second)}},
			}},
		},
	})
	in, _, perr := parseExecutorRequest(executorRequestJSON(t, "groq-whisper-large-v3", payload), cfg)
	if perr != nil {
		t.Fatal(perr)
	}
	if !bytes.Equal(in.Audio, first) {
		t.Fatal("first inline_data part must be used")
	}
}

func TestParseExecutorRequestErrors(t *testing.T) {
	cfg := mustConfig(t, "max_audio_bytes: 2048\n")
	valid := geminiBody(t, "audio/ogg", oggStub(), nil)
	fileOnly, _ := json.Marshal(map[string]any{"contents": []any{map[string]any{"parts": []any{
		map[string]any{"file_data": map[string]any{"mime_type": "audio/ogg", "file_uri": "gs://x"}},
	}}}})
	textOnly, _ := json.Marshal(map[string]any{"contents": []any{map[string]any{"parts": []any{map[string]any{"text": "hello"}}}}})
	cases := []struct {
		name   string
		raw    []byte
		status int
		substr string
	}{
		{"malformed rpc", []byte("{not json"), http.StatusBadRequest, "malformed executor request"},
		{"unknown model", executorRequestJSON(t, "gemini-3-flash", valid), http.StatusBadRequest, "unknown model"},
		{"empty payload", executorRequestJSON(t, "groq-whisper-large-v3", nil), http.StatusBadRequest, "empty"},
		{"payload not json", executorRequestJSON(t, "groq-whisper-large-v3", []byte("garbage")), http.StatusBadRequest, "not a valid Gemini"},
		{"text only", executorRequestJSON(t, "groq-whisper-large-v3", textOnly), http.StatusBadRequest, "no inline_data"},
		{"file data", executorRequestJSON(t, "groq-whisper-large-v3", fileOnly), http.StatusBadRequest, "file_data parts are not supported"},
		{"bad base64", executorRequestJSON(t, "groq-whisper-large-v3", []byte(`{"contents":[{"parts":[{"inline_data":{"mime_type":"audio/ogg","data":"%%%"}}]}]}`)), http.StatusBadRequest, "not valid base64"},
		{"too large", executorRequestJSON(t, "groq-whisper-large-v3", geminiBody(t, "audio/ogg", append(oggStub(), make([]byte, 4096)...), nil)), http.StatusRequestEntityTooLarge, "larger than the configured limit"},
		{"image", executorRequestJSON(t, "groq-whisper-large-v3", geminiBody(t, "image/png", []byte("\x89PNG...."), nil)), http.StatusBadRequest, "not audio"},
		{"noise", executorRequestJSON(t, "groq-whisper-large-v3", geminiBody(t, "application/octet-stream", bytes.Repeat([]byte{7}, 64), nil)), http.StatusBadRequest, "unsupported audio format"},
		{"unknown extension key", executorRequestJSON(t, "groq-whisper-large-v3", geminiBody(t, "audio/ogg", oggStub(), map[string]any{"cpa_groq": map[string]any{"lang": "en"}})), http.StatusBadRequest, "cpa_groq must be an object"},
		{"extension not object", executorRequestJSON(t, "groq-whisper-large-v3", geminiBody(t, "audio/ogg", oggStub(), map[string]any{"cpa_groq": "en"})), http.StatusBadRequest, "cpa_groq must be an object"},
		{"bad extension language", executorRequestJSON(t, "groq-whisper-large-v3", geminiBody(t, "audio/ogg", oggStub(), map[string]any{"cpa_groq": map[string]any{"language": "klingon"}})), http.StatusBadRequest, "cpa_groq.language"},
		{"long extension prompt", executorRequestJSON(t, "groq-whisper-large-v3", geminiBody(t, "audio/ogg", oggStub(), map[string]any{"cpa_groq": map[string]any{"prompt": strings.Repeat("x", 2000)}})), http.StatusBadRequest, "cpa_groq.prompt"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, perr := parseExecutorRequest(c.raw, cfg)
			if perr == nil {
				t.Fatal("expected error")
			}
			if perr.Status != c.status || !strings.Contains(perr.Message, c.substr) {
				t.Fatalf("got %d %q; want %d containing %q", perr.Status, perr.Message, c.status, c.substr)
			}
			if !strings.HasPrefix(perr.Message, "cpa-groq: ") {
				t.Fatalf("message should be prefixed: %q", perr.Message)
			}
		})
	}
}

func TestLookupModel(t *testing.T) {
	ok := map[string]string{
		"groq-whisper-large-v3":              "whisper-large-v3",
		"GROQ-WHISPER-LARGE-V3":              "whisper-large-v3",
		"models/groq-whisper-large-v3-turbo": "whisper-large-v3-turbo",
		"groq/groq-whisper-large-v3":         "whisper-large-v3",
		"groq-whisper-large-v3(high)":        "whisper-large-v3",
	}
	for in, want := range ok {
		m, found := lookupModel(in)
		if !found || m.Upstream != want {
			t.Errorf("lookupModel(%q) = %v %v", in, m.Upstream, found)
		}
	}
	for _, bad := range []string{"", "whisper-large-v3", "groq-whisper", "gemini-3-flash"} {
		if _, found := lookupModel(bad); found {
			t.Errorf("lookupModel(%q) should fail", bad)
		}
	}
}
