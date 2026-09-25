package main

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	pluginName    = "cpa-groq"
	pluginVersion = "0.1.0"
	pluginAuthor  = "batuhan4"
	pluginRepo    = "https://github.com/Batuhan4/cpa-groq"

	// providerKey is the CPA provider key of this executor. The auth record
	// that CPA needs for routing must use the same key (auth file "type").
	providerKey = "cpa-groq"

	// modelsCreated is a fixed timestamp (2026-09-25T00:00:00Z) so the model
	// list is deterministic across builds.
	modelsCreated = 1790294400
)

type modelSpec struct {
	ID          string // client-facing CPA model id
	Upstream    string // Groq model name
	DisplayName string
	Description string
}

var models = []modelSpec{
	{
		ID:          "groq-whisper-large-v3",
		Upstream:    "whisper-large-v3",
		DisplayName: "Groq Whisper Large v3",
		Description: "Speech-to-text with Groq whisper-large-v3 (cpa-groq). Send audio as inline_data; the transcript is returned as text.",
	},
	{
		ID:          "groq-whisper-large-v3-turbo",
		Upstream:    "whisper-large-v3-turbo",
		DisplayName: "Groq Whisper Large v3 Turbo",
		Description: "Speech-to-text with Groq whisper-large-v3-turbo (cpa-groq). Send audio as inline_data; the transcript is returned as text.",
	},
}

func modelIDs() []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ID)
	}
	return out
}

// lookupModel resolves the model name CPA hands to the executor. It tolerates
// a "models/" prefix, a CPA auth prefix ("x/groq-whisper-large-v3") and a
// CPA thinking suffix ("groq-whisper-large-v3(high)"), none of which change
// the Whisper model that runs.
func lookupModel(name string) (modelSpec, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.TrimPrefix(name, "models/")
	if idx := strings.LastIndexByte(name, '/'); idx >= 0 {
		name = name[idx+1:]
	}
	if strings.HasSuffix(name, ")") {
		if idx := strings.IndexByte(name, '('); idx > 0 {
			name = name[:idx]
		}
	}
	for _, m := range models {
		if m.ID == name {
			return m, true
		}
	}
	return modelSpec{}, false
}

func pluginModelInfos() []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, m := range models {
		out = append(out, pluginapi.ModelInfo{
			ID:          m.ID,
			Object:      "model",
			Created:     modelsCreated,
			OwnedBy:     "groq",
			Type:        providerKey,
			DisplayName: m.DisplayName,
			// Name stays empty: CPA's Gemini model list then uses the ID,
			// which keeps auth-prefixed variants ("x/<id>") distinct.
			Version:                    m.Upstream,
			Description:                m.Description,
			SupportedGenerationMethods: []string{"generateContent", "streamGenerateContent"},
			SupportedInputModalities:   []string{"audio"},
			SupportedOutputModalities:  []string{"text"},
		})
	}
	return out
}

func pluginMetadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:             pluginName,
		Version:          pluginVersion,
		Author:           pluginAuthor,
		GitHubRepository: pluginRepo,
		ConfigFields: []pluginapi.ConfigField{
			{Name: "api_key", Type: pluginapi.ConfigFieldTypeString, Description: "Groq API key. Required. Never logged."},
			{Name: "base_url", Type: pluginapi.ConfigFieldTypeString, Description: "Groq OpenAI-compatible base URL. Default https://api.groq.com/openai/v1."},
			{Name: "language", Type: pluginapi.ConfigFieldTypeString, Description: "Default ISO-639-1 language (default \"tr\"); \"auto\" or empty lets Whisper detect it. Requests can override it with cpa_groq.language."},
			{Name: "prompt", Type: pluginapi.ConfigFieldTypeString, Description: "Default Whisper prompt (spelling/vocabulary hints, at most 896 characters). Requests can override it with cpa_groq.prompt. Text parts of the request are never used as a prompt."},
			{Name: "timeout_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "Upper bound for one Groq call, 1-600 seconds (default 60)."},
			{Name: "max_audio_bytes", Type: pluginapi.ConfigFieldTypeInteger, Description: "Largest decoded audio accepted, checked before upload (default 25000000, Groq free tier)."},
			{Name: "no_speech_threshold", Type: pluginapi.ConfigFieldTypeNumber, Description: "Drop a segment when no_speech_prob is above this AND avg_logprob is below logprob_threshold (default 0.6; 1 disables filtering)."},
			{Name: "logprob_threshold", Type: pluginapi.ConfigFieldTypeNumber, Description: "avg_logprob bound for segment filtering (default -1.0; 0 filters on no_speech_prob alone)."},
		},
	}
}
