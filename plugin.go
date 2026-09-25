package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// registrationSchemaVersion is the RPC contract version declared at
// plugin.register. The plugin uses nothing newer than the original contract,
// and hosts accept any version up to their own, so 1 keeps it loadable by
// older and newer CPA releases alike.
const registrationSchemaVersion = 1

// rawRequestOverhead is added to the raw-request size guard (see execute).
const rawRequestOverhead = 16 << 20

// largeRequestBytes: after an execute call whose RPC request was at least
// this big, the plugin's Go heap is returned to the OS immediately instead of
// lingering until the background scavenger runs. Voice notes stay far below.
const largeRequestBytes = 8 << 20

// Plugin holds the plugin's only mutable state: the current config snapshot.
// It is safe for concurrent use; CPA calls the plugin from many goroutines.
type Plugin struct {
	host Host
	cfg  atomic.Pointer[Config]
	now  func() time.Time
}

func newPlugin(host Host) *Plugin {
	return &Plugin{host: host, now: time.Now}
}

// testHookDispatch, when set, runs at the start of every dispatch. Tests use
// it to inject panics; it is never set in production.
var testHookDispatch atomic.Pointer[func(method string)]

// dispatch is the single entry for every host->plugin call. It never panics:
// any panic is converted into an internal_error envelope.
func (p *Plugin) dispatch(method string, request []byte) (out []byte, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			p.logPanic(method, r)
			out, ok = newError(codeInternal, http.StatusInternalServerError, "internal error while handling %s", method).envelope(), false
		}
	}()
	if hook := testHookDispatch.Load(); hook != nil {
		(*hook)(method)
	}
	if len(request) >= largeRequestBytes {
		defer debug.FreeOSMemory()
	}
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return p.register(request)
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		return okEnvelope(emptyResult{})
	case pluginabi.MethodModelRegister:
		return okEnvelope(pluginapi.ModelRegistrationResponse{Provider: providerKey, Models: pluginModelInfos()})
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerKey})
	case pluginabi.MethodExecutorExecute:
		payload, perr := p.execute(request)
		if perr != nil {
			return perr.envelope(), false
		}
		return okEnvelope(executorResponse{Payload: payload})
	case pluginabi.MethodExecutorExecuteStream:
		payload, perr := p.execute(request)
		if perr != nil {
			return perr.envelope(), false
		}
		// Whisper is not incremental: the whole transcript is one final chunk.
		return okEnvelope(executorStreamResponse{Chunks: []executorStreamChunk{{Payload: payload}}})
	case pluginabi.MethodExecutorCountTokens:
		return newError(codeUnsupported, http.StatusBadRequest, "countTokens is not supported for Whisper models").envelope(), false
	case pluginabi.MethodExecutorHTTPRequest:
		return newError(codeUnsupported, http.StatusBadRequest, "raw executor HTTP requests are not supported").envelope(), false
	default:
		return (&pluginError{Code: codeUnknownMethod, Message: "cpa-groq: unknown method " + truncateForMessage(method, 64)}).envelope(), false
	}
}

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

// executorResponse is pluginapi.ExecutorResponse on the wire.
type executorResponse struct {
	Payload []byte `json:"Payload"`
}

// executorStreamResponse is the host's rpcExecutorStreamResponse. Returning
// the chunks inline (instead of host.stream.emit) finishes the stream in the
// same call, so no plugin goroutine outlives the request.
type executorStreamResponse struct {
	Chunks []executorStreamChunk `json:"chunks"`
}

type executorStreamChunk struct {
	Payload []byte `json:"Payload"`
}

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  capabilities       `json:"capabilities"`
}

type capabilities struct {
	ModelRegistrar        bool     `json:"model_registrar"`
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats"`
	ExecutorOutputFormats []string `json:"executor_output_formats"`
}

func okEnvelope(result any) ([]byte, bool) {
	raw, err := json.Marshal(result)
	if err != nil {
		return newError(codeInternal, http.StatusInternalServerError, "could not encode result").envelope(), false
	}
	env, err := json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
	if err != nil {
		return newError(codeInternal, http.StatusInternalServerError, "could not encode envelope").envelope(), false
	}
	return env, true
}

// register handles plugin.register and plugin.reconfigure identically: CPA
// calls reconfigure on every config reload once a plugin has registered, and
// also after a failed first register.
func (p *Plugin) register(request []byte) ([]byte, bool) {
	var req lifecycleRequest
	if len(bytes.TrimSpace(request)) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return newError(codeInvalidConfig, 0, "malformed lifecycle request").envelope(), false
		}
	}
	cfg, err := ParseConfig(req.ConfigYAML)
	if err != nil {
		p.log("", "error", "cpa-groq: rejected configuration", map[string]any{"reason": err.Error()})
		return (&pluginError{Code: codeInvalidConfig, Message: "cpa-groq: " + err.Error()}).envelope(), false
	}
	previous := p.cfg.Swap(cfg)
	p.logConfigChange(previous, cfg)
	return okEnvelope(registration{
		SchemaVersion: registrationSchemaVersion,
		Metadata:      pluginMetadata(),
		Capabilities: capabilities{
			ModelRegistrar:        true,
			Executor:              true,
			ExecutorModelScope:    string(pluginapi.ExecutorModelScopeStatic),
			ExecutorInputFormats:  []string{"gemini"},
			ExecutorOutputFormats: []string{"gemini"},
		},
	})
}

// execute runs one transcription and returns the Gemini response body.
func (p *Plugin) execute(raw []byte) ([]byte, *pluginError) {
	cfg := p.cfg.Load()
	if cfg == nil {
		return nil, newError(codeNotConfigured, http.StatusInternalServerError, "plugin is not configured")
	}
	// The raw RPC request carries the audio twice (Payload and
	// OriginalRequest), each base64 encoded twice. Reject absurd sizes
	// before decoding anything.
	if int64(len(raw)) > 4*cfg.MaxAudioBytes+rawRequestOverhead {
		return nil, newError(codeAudioTooLarge, http.StatusRequestEntityTooLarge, "request is larger than the configured audio limit of %d bytes allows", cfg.MaxAudioBytes)
	}

	started := p.now()
	in, callbackID, perr := parseExecutorRequest(raw, cfg)
	if perr != nil {
		return nil, perr
	}
	audioBytes := len(in.Audio)
	kind := in.Kind

	body, contentType, err := buildMultipart(groqForm{
		Model:    in.Model.Upstream,
		Language: in.Language,
		Prompt:   in.Prompt,
		Kind:     in.Kind,
		Audio:    in.Audio,
	}, "")
	in.Audio = nil
	if err != nil {
		return nil, newError(codeInternal, http.StatusInternalServerError, "could not build the upload")
	}

	upstreamStart := p.now()
	resp, perr := p.postToGroq(cfg, callbackID, body, contentType)
	upstreamMS := p.now().Sub(upstreamStart).Milliseconds()
	if perr != nil {
		if perr.Code != codeCanceled {
			p.log(callbackID, "warn", "cpa-groq: transcription failed", map[string]any{
				"model": in.Model.ID, "audio_bytes": audioBytes, "audio_kind": kind.String(),
				"status": perr.Status, "code": perr.Code, "upstream_ms": upstreamMS,
			})
		}
		return nil, perr
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		uerr := upstreamError(resp.StatusCode, resp.Headers, resp.Body, cfg.APIKey)
		p.log(callbackID, "warn", "cpa-groq: Groq returned an error", map[string]any{
			"model": in.Model.ID, "audio_bytes": audioBytes, "audio_kind": kind.String(),
			"groq_status": resp.StatusCode, "status": uerr.Status, "upstream_ms": upstreamMS,
		})
		return nil, uerr
	}

	result, err := mapTranscription(resp.Body, in.Model, cfg, headerValue(resp.Headers, "X-Request-Id"))
	if err != nil {
		return nil, newError(codeUpstream, http.StatusBadGateway, "%s", err.Error())
	}
	p.log(callbackID, "debug", "cpa-groq: transcription ok", map[string]any{
		"model": in.Model.ID, "audio_bytes": audioBytes, "audio_kind": kind.String(),
		"language": languageLabel(in.Language), "prompt_set": in.Prompt != "",
		"audio_seconds": fmt.Sprintf("%.2f", result.DurationSeconds), "segments": result.Segments,
		"segments_dropped": result.DroppedSegments, "text_bytes": result.TextBytes,
		"upstream_ms": upstreamMS, "total_ms": p.now().Sub(started).Milliseconds(),
	})
	return result.Payload, nil
}

func languageLabel(lang string) string {
	if lang == "" {
		return "auto"
	}
	return lang
}

// log sends a structured line to CPA's log via host.log. It never includes
// audio, transcripts, prompts or the API key, and never fails the caller.
func (p *Plugin) log(callbackID, level, message string, fields map[string]any) {
	defer func() { _ = recover() }()
	_, _ = callHost[emptyResult](p.host, pluginabi.MethodHostLog, hostLogRequest{
		HostCallbackID: callbackID,
		Level:          level,
		Message:        message,
		Fields:         fields,
	})
}

func (p *Plugin) logPanic(method string, recovered any) {
	stack := string(debug.Stack())
	if len(stack) > 4096 {
		stack = stack[:4096]
	}
	p.log("", "error", "cpa-groq: recovered from panic", map[string]any{
		"method": method,
		"panic":  truncateForMessage(fmt.Sprint(recovered), 200),
		"stack":  stack,
	})
}

func (p *Plugin) logConfigChange(previous, next *Config) {
	if previous != nil && sameSettings(previous, next) {
		return
	}
	fields := map[string]any{
		"base_url":            next.BaseURL,
		"language":            languageLabel(next.Language),
		"prompt_set":          next.Prompt != "",
		"timeout_seconds":     int(next.Timeout / time.Second),
		"max_audio_bytes":     next.MaxAudioBytes,
		"no_speech_threshold": next.NoSpeechThreshold,
		"logprob_threshold":   next.LogprobThreshold,
		"api_key_set":         next.APIKey != "",
		"version":             pluginVersion,
	}
	if previous != nil && previous.APIKey != next.APIKey {
		fields["api_key_changed"] = true
	}
	p.log("", "info", "cpa-groq: configuration applied", fields)
	if len(next.UnknownKeys) > 0 {
		p.log("", "warn", "cpa-groq: ignoring unknown config keys", map[string]any{"keys": strings.Join(next.UnknownKeys, ",")})
	}
}

func sameSettings(a, b *Config) bool {
	return a.APIKey == b.APIKey && a.BaseURL == b.BaseURL && a.Language == b.Language &&
		a.Prompt == b.Prompt && a.Timeout == b.Timeout && a.MaxAudioBytes == b.MaxAudioBytes &&
		a.NoSpeechThreshold == b.NoSpeechThreshold && a.LogprobThreshold == b.LogprobThreshold &&
		strings.Join(a.UnknownKeys, ",") == strings.Join(b.UnknownKeys, ",")
}
