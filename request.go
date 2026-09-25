package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// executorRequest is the subset of CPA's rpcExecutorRequest this plugin reads
// (pluginapi.ExecutorRequest has no JSON tags, so fields use Go names).
// OriginalRequest is deliberately not decoded: it duplicates the audio.
type executorRequest struct {
	Model          string `json:"Model"`
	Payload        []byte `json:"Payload"`
	HostCallbackID string `json:"host_callback_id"`
}

// geminiRequest is the subset of a Gemini generateContent body this plugin
// reads. Both the REST camelCase and the snake_case spellings are accepted.
type geminiRequest struct {
	Contents []geminiContent `json:"contents"`
	// CPAGroq is this plugin's opt-in request extension, see README.
	CPAGroq json.RawMessage `json:"cpa_groq"`
}

type geminiContent struct {
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	InlineData      *geminiBlob     `json:"inlineData"`
	InlineDataSnake *geminiBlob     `json:"inline_data"`
	FileData        json.RawMessage `json:"fileData"`
	FileDataSnake   json.RawMessage `json:"file_data"`
}

type geminiBlob struct {
	MimeType      string `json:"mimeType"`
	MimeTypeSnake string `json:"mime_type"`
	Data          string `json:"data"`
}

// requestOptions is the decoded cpa_groq extension object.
type requestOptions struct {
	Language *string `json:"language"`
	Prompt   *string `json:"prompt"`
}

// transcriptionInput is everything the Groq call needs from one request.
type transcriptionInput struct {
	Model    modelSpec
	Audio    []byte
	Kind     audioKind
	Language string // resolved; "" means auto-detect
	Prompt   string // resolved; "" means none
}

// parseExecutorRequest decodes the RPC request and the Gemini payload into a
// transcription input. All failures are client errors (400/413), which CPA
// treats as request faults that never cool down the credential.
func parseExecutorRequest(raw []byte, cfg *Config) (*transcriptionInput, string, *pluginError) {
	var req executorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, "", newError(codeInvalidRequest, http.StatusBadRequest, "malformed executor request")
	}
	model, ok := lookupModel(req.Model)
	if !ok {
		return nil, req.HostCallbackID, newError(codeUnknownModel, http.StatusBadRequest, "unknown model %q; use one of %s", truncateForMessage(req.Model, 80), strings.Join(modelIDs(), ", "))
	}
	in, perr := parseGeminiPayload(req.Payload, cfg)
	if perr != nil {
		return nil, req.HostCallbackID, perr
	}
	in.Model = model
	return in, req.HostCallbackID, nil
}

func parseGeminiPayload(payload []byte, cfg *Config) (*transcriptionInput, *pluginError) {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 {
		return nil, newError(codeInvalidRequest, http.StatusBadRequest, "request body is empty")
	}
	var body geminiRequest
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil, newError(codeInvalidRequest, http.StatusBadRequest, "request body is not a valid Gemini generateContent JSON object")
	}

	blob, fileDataSeen := firstInlineData(body.Contents)
	if blob == nil {
		if fileDataSeen {
			return nil, newError(codeInvalidRequest, http.StatusBadRequest, "file_data parts are not supported; send the audio as inline_data (base64)")
		}
		return nil, newError(codeInvalidRequest, http.StatusBadRequest, "no inline_data audio part found in contents")
	}
	mime := blob.MimeType
	if mime == "" {
		mime = blob.MimeTypeSnake
	}
	audio, err := decodeBase64(blob.Data, cfg.MaxAudioBytes)
	if err != nil {
		if errors.Is(err, errTooLarge) {
			return nil, newError(codeAudioTooLarge, http.StatusRequestEntityTooLarge, "audio is larger than the configured limit of %d bytes", cfg.MaxAudioBytes)
		}
		return nil, newError(codeInvalidRequest, statusForAudioError(err), "%s", err.Error())
	}
	blob.Data = "" // release the base64 copy early; the decoded bytes are all we need
	kind, err := resolveAudioKind(mime, audio)
	if err != nil {
		return nil, newError(codeUnsupportedAudio, http.StatusBadRequest, "%s", err.Error())
	}

	in := &transcriptionInput{
		Audio:    audio,
		Kind:     kind,
		Language: cfg.Language,
		Prompt:   cfg.Prompt,
	}
	if perr := applyRequestOptions(in, body.CPAGroq); perr != nil {
		return nil, perr
	}
	return in, nil
}

// firstInlineData returns the first inline_data part in document order.
func firstInlineData(contents []geminiContent) (*geminiBlob, bool) {
	fileDataSeen := false
	for ci := range contents {
		for pi := range contents[ci].Parts {
			part := &contents[ci].Parts[pi]
			if part.InlineData != nil {
				return part.InlineData, fileDataSeen
			}
			if part.InlineDataSnake != nil {
				return part.InlineDataSnake, fileDataSeen
			}
			if len(part.FileData) > 0 || len(part.FileDataSnake) > 0 {
				fileDataSeen = true
			}
		}
	}
	return nil, fileDataSeen
}

// applyRequestOptions applies the optional cpa_groq object. Unknown keys are
// rejected so a typo ("lang") fails loudly instead of being ignored.
func applyRequestOptions(in *transcriptionInput, raw json.RawMessage) *pluginError {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var opts requestOptions
	if err := dec.Decode(&opts); err != nil {
		return newError(codeInvalidRequest, http.StatusBadRequest, "cpa_groq must be an object with optional string fields \"language\" and \"prompt\"")
	}
	if dec.More() {
		return newError(codeInvalidRequest, http.StatusBadRequest, "cpa_groq must be a single JSON object")
	}
	if opts.Language != nil {
		lang, err := normalizeLanguage(*opts.Language)
		if err != nil {
			return newError(codeInvalidRequest, http.StatusBadRequest, "cpa_groq.language %s", err.Error())
		}
		in.Language = lang
	}
	if opts.Prompt != nil {
		prompt, err := normalizePrompt(*opts.Prompt)
		if err != nil {
			return newError(codeInvalidRequest, http.StatusBadRequest, "cpa_groq.prompt %s", err.Error())
		}
		in.Prompt = prompt
	}
	return nil
}

func truncateForMessage(s string, limit int) string {
	s = sanitizeUpstreamText(s, "")
	if len(s) <= limit {
		return s
	}
	cut := 0
	for i := range s {
		if i > limit {
			break
		}
		cut = i
	}
	return fmt.Sprintf("%s...", s[:cut])
}
