package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseConfigDefaults(t *testing.T) {
	cfg := mustConfig(t, "")
	if cfg.APIKey != testAPIKey {
		t.Fatalf("api key not applied")
	}
	if cfg.BaseURL != defaultBaseURL || cfg.TranscriptionsURL != "https://api.groq.com/openai/v1/audio/transcriptions" {
		t.Fatalf("unexpected URLs %q %q", cfg.BaseURL, cfg.TranscriptionsURL)
	}
	if cfg.Language != "tr" || cfg.Prompt != "" {
		t.Fatalf("unexpected language/prompt %q %q", cfg.Language, cfg.Prompt)
	}
	if cfg.Timeout != 60*time.Second || cfg.MaxAudioBytes != 25_000_000 {
		t.Fatalf("unexpected timeout/max %v %d", cfg.Timeout, cfg.MaxAudioBytes)
	}
	if cfg.NoSpeechThreshold != 0.6 || cfg.LogprobThreshold != -1.0 {
		t.Fatalf("unexpected thresholds %v %v", cfg.NoSpeechThreshold, cfg.LogprobThreshold)
	}
	if len(cfg.UnknownKeys) != 0 {
		t.Fatalf("host-managed keys must not be reported as unknown: %v", cfg.UnknownKeys)
	}
}

func TestParseConfigOverrides(t *testing.T) {
	cfg := mustConfig(t, `base_url: "https://example.test/openai/v1/"
language: "EN-us"
prompt: "  Ada, CLIProxyAPI, Groq  "
timeout_seconds: 5
max_audio_bytes: 2048
no_speech_threshold: 1
logprob_threshold: 0
store:
  version: "0.1.0"
`)
	if cfg.BaseURL != "https://example.test/openai/v1" {
		t.Fatalf("base url %q", cfg.BaseURL)
	}
	if cfg.Language != "en" || cfg.Prompt != "Ada, CLIProxyAPI, Groq" {
		t.Fatalf("language/prompt %q %q", cfg.Language, cfg.Prompt)
	}
	if cfg.Timeout != 5*time.Second || cfg.MaxAudioBytes != 2048 || cfg.NoSpeechThreshold != 1 || cfg.LogprobThreshold != 0 {
		t.Fatalf("numeric overrides not applied: %+v", cfg)
	}
}

func TestParseConfigAutoLanguage(t *testing.T) {
	for _, v := range []string{`""`, `auto`, `AUTO`} {
		cfg := mustConfig(t, "language: "+v+"\n")
		if cfg.Language != "" {
			t.Fatalf("language %s should mean auto-detect, got %q", v, cfg.Language)
		}
	}
}

func TestParseConfigLoopbackHTTPAllowed(t *testing.T) {
	cfg := mustConfig(t, "base_url: http://127.0.0.1:17701/v1\n")
	if cfg.TranscriptionsURL != "http://127.0.0.1:17701/v1/audio/transcriptions" {
		t.Fatalf("got %q", cfg.TranscriptionsURL)
	}
}

func TestParseConfigRejects(t *testing.T) {
	cases := map[string]string{
		"missing key":         "enabled: true\n",
		"empty key":           "api_key: \"  \"\n",
		"key with space":      "api_key: \"test-abc def123\"\n",
		"short key":           "api_key: abc\n",
		"http base":           "api_key: " + testAPIKey + "\nbase_url: http://api.groq.com/openai/v1\n",
		"base with query":     "api_key: " + testAPIKey + "\nbase_url: https://api.groq.com/openai/v1?x=1\n",
		"base with userinfo":  "api_key: " + testAPIKey + "\nbase_url: https://u:p@api.groq.com/openai/v1\n",
		"relative base":       "api_key: " + testAPIKey + "\nbase_url: /openai/v1\n",
		"bad language":        "api_key: " + testAPIKey + "\nlanguage: turkish\n",
		"digit language":      "api_key: " + testAPIKey + "\nlanguage: t1\n",
		"long prompt":         "api_key: " + testAPIKey + "\nprompt: " + strings.Repeat("a", maxPromptChars+1) + "\n",
		"control prompt":      "api_key: " + testAPIKey + "\nprompt: \"a\\u0007b\"\n",
		"timeout zero":        "api_key: " + testAPIKey + "\ntimeout_seconds: 0\n",
		"timeout huge":        "api_key: " + testAPIKey + "\ntimeout_seconds: 601\n",
		"timeout type":        "api_key: " + testAPIKey + "\ntimeout_seconds: soon\n",
		"max audio tiny":      "api_key: " + testAPIKey + "\nmax_audio_bytes: 10\n",
		"max audio huge":      "api_key: " + testAPIKey + "\nmax_audio_bytes: 100000001\n",
		"no speech negative":  "api_key: " + testAPIKey + "\nno_speech_threshold: -0.1\n",
		"no speech above one": "api_key: " + testAPIKey + "\nno_speech_threshold: 1.5\n",
		"no speech nan":       "api_key: " + testAPIKey + "\nno_speech_threshold: .nan\n",
		"logprob positive":    "api_key: " + testAPIKey + "\nlogprob_threshold: 0.5\n",
		"logprob -inf":        "api_key: " + testAPIKey + "\nlogprob_threshold: -.inf\n",
		"not a mapping":       "- a\n- b\n",
		"broken yaml":         "api_key: [unclosed\n",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			cfg, err := ParseConfig([]byte(raw))
			if err == nil {
				t.Fatalf("expected error, got config %+v", cfg)
			}
			if strings.Contains(err.Error(), testAPIKey) || strings.Contains(err.Error(), "abc def") || strings.Contains(err.Error(), "soon") {
				t.Fatalf("error leaks a config value: %v", err)
			}
		})
	}
}

func TestParseConfigErrorNeverEchoesKey(t *testing.T) {
	// A valid key plus an invalid field: the error names the field only.
	_, err := ParseConfig([]byte("api_key: " + testAPIKey + "\ntimeout_seconds: 0\nbase_url: ftp://x\n"))
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), testAPIKey) {
		t.Fatalf("error leaks api key: %v", err)
	}
	if !strings.Contains(err.Error(), "timeout_seconds") || !strings.Contains(err.Error(), "base_url") {
		t.Fatalf("error should list every problem: %v", err)
	}
}

func TestParseConfigUnknownKeys(t *testing.T) {
	cfg := mustConfig(t, "api-key: value-that-must-not-leak\nlang: en\n")
	got := strings.Join(cfg.UnknownKeys, ",")
	if got != "api-key,lang" {
		t.Fatalf("unknown keys %q", got)
	}
}

func TestNormalizeLanguage(t *testing.T) {
	ok := map[string]string{"tr": "tr", "TR": "tr", "tr-TR": "tr", "pt_BR": "pt", "haw": "haw", " auto ": "", "": ""}
	for in, want := range ok {
		got, err := normalizeLanguage(in)
		if err != nil || got != want {
			t.Fatalf("normalizeLanguage(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"t", "turk", "12", "-tr", "tr!"} {
		if _, err := normalizeLanguage(bad); err == nil {
			t.Fatalf("normalizeLanguage(%q) should fail", bad)
		}
	}
}

func TestParseConfigYAMLErrorsCarryNoValues(t *testing.T) {
	_, err := ParseConfig([]byte("api_key: " + testAPIKey + "\napi_key: " + testAPIKey + "\n"))
	if err == nil || !strings.Contains(err.Error(), "line") || strings.Contains(err.Error(), testAPIKey) {
		t.Fatalf("duplicate key error: %v", err)
	}
}
