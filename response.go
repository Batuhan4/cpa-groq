package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"unicode/utf8"
)

// audioTokensPerSecond mirrors Gemini's documented audio tokenisation
// (32 tokens per second of audio) so usage for Groq and Gemini voice requests
// is comparable in CPA's statistics. seconds = promptTokenCount / 32.
const audioTokensPerSecond = 32

// groqVerbose is Groq's response_format=verbose_json body.
type groqVerbose struct {
	Text     string        `json:"text"`
	Language string        `json:"language"`
	Duration float64       `json:"duration"`
	Segments []groqSegment `json:"segments"`
	XGroq    *struct {
		ID string `json:"id"`
	} `json:"x_groq"`
}

type groqSegment struct {
	Text         string  `json:"text"`
	Tokens       []int   `json:"tokens"`
	AvgLogprob   float64 `json:"avg_logprob"`
	NoSpeechProb float64 `json:"no_speech_prob"`
}

type geminiResponse struct {
	Candidates    []geminiCandidate `json:"candidates"`
	UsageMetadata geminiUsage       `json:"usageMetadata"`
	ModelVersion  string            `json:"modelVersion,omitempty"`
	ResponseID    string            `json:"responseId,omitempty"`
}

type geminiCandidate struct {
	Content      geminiResponseContent `json:"content"`
	FinishReason string                `json:"finishReason"`
	Index        int                   `json:"index"`
}

type geminiResponseContent struct {
	Role  string           `json:"role"`
	Parts []geminiTextPart `json:"parts"`
}

type geminiTextPart struct {
	Text string `json:"text"`
}

type geminiUsage struct {
	PromptTokenCount        int             `json:"promptTokenCount"`
	CandidatesTokenCount    int             `json:"candidatesTokenCount"`
	TotalTokenCount         int             `json:"totalTokenCount"`
	PromptTokensDetails     []modalityCount `json:"promptTokensDetails,omitempty"`
	CandidatesTokensDetails []modalityCount `json:"candidatesTokensDetails,omitempty"`
}

type modalityCount struct {
	Modality   string `json:"modality"`
	TokenCount int    `json:"tokenCount"`
}

// transcriptResult is the mapped outcome plus non-sensitive stats for logs.
type transcriptResult struct {
	Payload         []byte
	Segments        int
	DroppedSegments int
	DurationSeconds float64
	TextBytes       int
}

// mapTranscription converts a Groq verbose_json body into a Gemini
// generateContent response, applying the no_speech segment filter.
func mapTranscription(body []byte, model modelSpec, cfg *Config, requestID string) (*transcriptResult, error) {
	var v groqVerbose
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, errors.New("the Groq response is not valid verbose_json")
	}

	text := strings.TrimSpace(v.Text)
	outputTokens := 0
	dropped := 0
	if len(v.Segments) > 0 {
		var kept strings.Builder
		for _, seg := range v.Segments {
			if dropSegment(seg, cfg) {
				dropped++
				continue
			}
			kept.WriteString(seg.Text)
			outputTokens += len(seg.Tokens)
		}
		if dropped > 0 {
			text = strings.TrimSpace(kept.String())
		}
	}
	if outputTokens == 0 && text != "" {
		// Groq omitted token ids: estimate ~4 characters per token.
		outputTokens = (utf8.RuneCountInString(text) + 3) / 4
	}

	duration := v.Duration
	if math.IsNaN(duration) || math.IsInf(duration, 0) || duration < 0 {
		duration = 0
	}
	audioTokens := int(math.Ceil(duration * audioTokensPerSecond))

	respID := requestID
	if v.XGroq != nil && strings.TrimSpace(v.XGroq.ID) != "" {
		respID = strings.TrimSpace(v.XGroq.ID)
	}

	resp := geminiResponse{
		Candidates: []geminiCandidate{{
			Content: geminiResponseContent{
				Role:  "model",
				Parts: []geminiTextPart{{Text: text}},
			},
			FinishReason: "STOP",
			Index:        0,
		}},
		UsageMetadata: geminiUsage{
			PromptTokenCount:        audioTokens,
			CandidatesTokenCount:    outputTokens,
			TotalTokenCount:         audioTokens + outputTokens,
			PromptTokensDetails:     []modalityCount{{Modality: "AUDIO", TokenCount: audioTokens}},
			CandidatesTokensDetails: []modalityCount{{Modality: "TEXT", TokenCount: outputTokens}},
		},
		// The CPA model id, not the Groq name: CPA warns about "model
		// substitution" when the reported model differs from the requested one.
		ModelVersion: model.ID,
		ResponseID:   sanitizeID(respID),
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(resp); err != nil {
		return nil, err
	}
	return &transcriptResult{
		Payload:         bytes.TrimRight(buf.Bytes(), "\n"),
		Segments:        len(v.Segments),
		DroppedSegments: dropped,
		DurationSeconds: duration,
		TextBytes:       len(text),
	}, nil
}

// dropSegment mirrors openai-whisper's silence rule: a segment is treated as
// non-speech only when no_speech_prob is high AND the decoder was unsure.
func dropSegment(seg groqSegment, cfg *Config) bool {
	if cfg.NoSpeechThreshold >= 1 {
		return false
	}
	return seg.NoSpeechProb > cfg.NoSpeechThreshold && seg.AvgLogprob < cfg.LogprobThreshold
}

// sanitizeID keeps response ids to a safe character set and length.
func sanitizeID(id string) string {
	var b strings.Builder
	for _, r := range id {
		if b.Len() >= 128 {
			break
		}
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' || r == ':' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
