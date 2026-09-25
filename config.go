package main

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Defaults and bounds for plugins.configs.cpa-groq.
const (
	defaultBaseURL           = "https://api.groq.com/openai/v1"
	defaultLanguage          = "tr"
	defaultTimeoutSeconds    = 60
	minTimeoutSeconds        = 1
	maxTimeoutSeconds        = 600
	defaultMaxAudioBytes     = 25_000_000 // Groq free tier: 25 MB per file.
	minMaxAudioBytes         = 1024
	maxMaxAudioBytes         = 100_000_000 // Groq dev tier: 100 MB per file.
	defaultNoSpeechThreshold = 0.6         // openai-whisper transcribe() default.
	defaultLogprobThreshold  = -1.0        // openai-whisper transcribe() default.
	maxPromptChars           = 896         // Groq rejects longer prompts (HTTP 400); Whisper uses at most 224 tokens.
	minAPIKeyLength          = 8
	maxAPIKeyLength          = 512
	transcriptionsPath       = "/audio/transcriptions"
)

// hostManagedKeys are keys CPA itself writes into a plugin's config block.
var hostManagedKeys = map[string]struct{}{
	"enabled":  {},
	"priority": {},
	"store":    {},
}

// knownKeys are the keys this plugin understands.
var knownKeys = map[string]struct{}{
	"api_key":             {},
	"base_url":            {},
	"language":            {},
	"prompt":              {},
	"timeout_seconds":     {},
	"max_audio_bytes":     {},
	"no_speech_threshold": {},
	"logprob_threshold":   {},
}

// Config is an immutable, validated plugin configuration snapshot. A new value
// is built on every plugin.register / plugin.reconfigure and swapped in
// atomically; in-flight requests keep the snapshot they started with.
type Config struct {
	APIKey            string
	BaseURL           string
	TranscriptionsURL string
	Language          string // "" means auto-detect
	Prompt            string // "" means no prompt
	Timeout           time.Duration
	MaxAudioBytes     int64
	NoSpeechThreshold float64
	LogprobThreshold  float64

	// UnknownKeys lists config keys that were ignored (names only, never values).
	UnknownKeys []string
}

type rawConfig struct {
	APIKey            *string  `yaml:"api_key"`
	BaseURL           *string  `yaml:"base_url"`
	Language          *string  `yaml:"language"`
	Prompt            *string  `yaml:"prompt"`
	TimeoutSeconds    *int     `yaml:"timeout_seconds"`
	MaxAudioBytes     *int64   `yaml:"max_audio_bytes"`
	NoSpeechThreshold *float64 `yaml:"no_speech_threshold"`
	LogprobThreshold  *float64 `yaml:"logprob_threshold"`
}

// ParseConfig decodes and validates the YAML block CPA passes at
// plugin.register / plugin.reconfigure. Error messages name fields but never
// echo values, so they are safe for CPA's logs.
func ParseConfig(raw []byte) (*Config, error) {
	raw = bytes.TrimSpace(raw)
	var keys map[string]yaml.Node
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &keys); err != nil {
			return nil, fmt.Errorf("config is not a valid YAML mapping (%s)", yamlErrorLines(err))
		}
	}
	var rc rawConfig
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &rc); err != nil {
			return nil, fmt.Errorf("config has a field of the wrong type: %s", yamlTypeErrorFields(err))
		}
	}

	cfg := &Config{
		BaseURL:           defaultBaseURL,
		Language:          defaultLanguage,
		Timeout:           defaultTimeoutSeconds * time.Second,
		MaxAudioBytes:     defaultMaxAudioBytes,
		NoSpeechThreshold: defaultNoSpeechThreshold,
		LogprobThreshold:  defaultLogprobThreshold,
	}
	for key := range keys {
		if _, ok := knownKeys[key]; ok {
			continue
		}
		if _, ok := hostManagedKeys[key]; ok {
			continue
		}
		cfg.UnknownKeys = append(cfg.UnknownKeys, sanitizeKeyName(key))
	}
	sort.Strings(cfg.UnknownKeys)

	var problems []string

	if rc.APIKey == nil || strings.TrimSpace(*rc.APIKey) == "" {
		problems = append(problems, "api_key is required")
	} else {
		key := strings.TrimSpace(*rc.APIKey)
		switch {
		case len(key) < minAPIKeyLength || len(key) > maxAPIKeyLength:
			problems = append(problems, fmt.Sprintf("api_key must be %d-%d characters", minAPIKeyLength, maxAPIKeyLength))
		case !isPrintableASCIIToken(key):
			problems = append(problems, "api_key must not contain spaces or control characters")
		default:
			cfg.APIKey = key
		}
	}

	if rc.BaseURL != nil && strings.TrimSpace(*rc.BaseURL) != "" {
		base, err := normalizeBaseURL(*rc.BaseURL)
		if err != nil {
			problems = append(problems, "base_url "+err.Error())
		} else {
			cfg.BaseURL = base
		}
	}
	cfg.TranscriptionsURL = cfg.BaseURL + transcriptionsPath

	if rc.Language != nil {
		lang, err := normalizeLanguage(*rc.Language)
		if err != nil {
			problems = append(problems, "language "+err.Error())
		} else {
			cfg.Language = lang
		}
	}

	if rc.Prompt != nil {
		prompt, err := normalizePrompt(*rc.Prompt)
		if err != nil {
			problems = append(problems, "prompt "+err.Error())
		} else {
			cfg.Prompt = prompt
		}
	}

	if rc.TimeoutSeconds != nil {
		if *rc.TimeoutSeconds < minTimeoutSeconds || *rc.TimeoutSeconds > maxTimeoutSeconds {
			problems = append(problems, fmt.Sprintf("timeout_seconds must be between %d and %d", minTimeoutSeconds, maxTimeoutSeconds))
		} else {
			cfg.Timeout = time.Duration(*rc.TimeoutSeconds) * time.Second
		}
	}

	if rc.MaxAudioBytes != nil {
		if *rc.MaxAudioBytes < minMaxAudioBytes || *rc.MaxAudioBytes > maxMaxAudioBytes {
			problems = append(problems, fmt.Sprintf("max_audio_bytes must be between %d and %d", minMaxAudioBytes, maxMaxAudioBytes))
		} else {
			cfg.MaxAudioBytes = *rc.MaxAudioBytes
		}
	}

	if rc.NoSpeechThreshold != nil {
		v := *rc.NoSpeechThreshold
		if math.IsNaN(v) || v < 0 || v > 1 {
			problems = append(problems, "no_speech_threshold must be between 0 and 1 (1 disables segment filtering)")
		} else {
			cfg.NoSpeechThreshold = v
		}
	}

	if rc.LogprobThreshold != nil {
		v := *rc.LogprobThreshold
		if math.IsNaN(v) || math.IsInf(v, 0) || v > 0 || v < -100 {
			problems = append(problems, "logprob_threshold must be between -100 and 0")
		} else {
			cfg.LogprobThreshold = v
		}
	}

	if len(problems) > 0 {
		return nil, errors.New("invalid config: " + strings.Join(problems, "; "))
	}
	return cfg, nil
}

var yamlLinePattern = regexp.MustCompile(`line \d+`)

// yamlErrorLines keeps only the line references of a YAML error: some yaml
// messages quote values, and config values may be secrets.
func yamlErrorLines(err error) string {
	lines := yamlLinePattern.FindAllString(err.Error(), 4)
	if len(lines) == 0 {
		return "unknown line"
	}
	return strings.Join(lines, ", ")
}

// yamlTypeErrorFields reduces a yaml.TypeError to a short, value-free message.
func yamlTypeErrorFields(err error) string {
	var typeErr *yaml.TypeError
	if errors.As(err, &typeErr) && len(typeErr.Errors) > 0 {
		out := make([]string, 0, len(typeErr.Errors))
		for _, msg := range typeErr.Errors {
			// yaml messages look like: "line 3: cannot unmarshal !!str `abc` into int".
			// Keep the line number only; the backticked part may contain a value.
			if idx := strings.Index(msg, ":"); idx > 0 {
				out = append(out, msg[:idx])
			} else {
				out = append(out, "unknown line")
			}
		}
		return strings.Join(out, ", ")
	}
	return "unparseable value"
}

func sanitizeKeyName(key string) string {
	var b strings.Builder
	for _, r := range key {
		if b.Len() >= 64 {
			b.WriteString("...")
			break
		}
		if r < 0x20 || r == 0x7f || !utf8.ValidRune(r) {
			b.WriteRune('?')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isPrintableASCIIToken(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= 0x20 || c >= 0x7f {
			return false
		}
	}
	return true
}

func normalizeBaseURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", errors.New("must be an absolute URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("must not contain credentials, a query or a fragment")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopbackHost(u.Hostname()) {
			return "", errors.New("must use https (plain http is only allowed for loopback hosts)")
		}
	default:
		return "", errors.New("must use https")
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host+u.EscapedPath(), "/"), nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// normalizeLanguage accepts ISO-639-1 codes ("tr"), the few three-letter codes
// Whisper knows ("haw", "yue"), BCP-47 tags whose primary subtag is one of
// those ("tr-TR" -> "tr"), and "" / "auto" for auto-detection.
func normalizeLanguage(raw string) (string, error) {
	lang := strings.ToLower(strings.TrimSpace(raw))
	if lang == "" || lang == "auto" {
		return "", nil
	}
	if idx := strings.IndexAny(lang, "-_"); idx > 0 {
		lang = lang[:idx]
	}
	if len(lang) < 2 || len(lang) > 3 {
		return "", errors.New("must be an ISO-639-1 code such as \"tr\", or \"auto\"")
	}
	for _, r := range lang {
		if r < 'a' || r > 'z' {
			return "", errors.New("must be an ISO-639-1 code such as \"tr\", or \"auto\"")
		}
	}
	return lang, nil
}

func normalizePrompt(raw string) (string, error) {
	prompt := strings.TrimSpace(raw)
	if !utf8.ValidString(prompt) {
		return "", errors.New("must be valid UTF-8")
	}
	if utf8.RuneCountInString(prompt) > maxPromptChars {
		return "", fmt.Errorf("must be at most %d characters (Groq's limit; Whisper uses about 224 tokens)", maxPromptChars)
	}
	for _, r := range prompt {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return "", errors.New("must not contain control characters")
		}
	}
	return prompt, nil
}
