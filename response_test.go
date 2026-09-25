package main

import (
	"encoding/json"
	"strings"
	"testing"
)

type decodedGemini struct {
	Candidates []struct {
		Content struct {
			Role  string `json:"role"`
			Parts []struct {
				Text *string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		TotalTokenCount      int `json:"totalTokenCount"`
		PromptTokensDetails  []struct {
			Modality   string `json:"modality"`
			TokenCount int    `json:"tokenCount"`
		} `json:"promptTokensDetails"`
	} `json:"usageMetadata"`
	ModelVersion string `json:"modelVersion"`
	ResponseID   string `json:"responseId"`
}

func decodeGemini(t *testing.T, raw []byte) decodedGemini {
	t.Helper()
	var out decodedGemini
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("not JSON: %v: %s", err, raw)
	}
	if len(out.Candidates) != 1 || len(out.Candidates[0].Content.Parts) != 1 || out.Candidates[0].Content.Parts[0].Text == nil {
		t.Fatalf("unexpected shape: %s", raw)
	}
	return out
}

func TestMapTranscriptionShapeAndUsage(t *testing.T) {
	cfg := mustConfig(t, "")
	m, _ := lookupModel("groq-whisper-large-v3")
	res, err := mapTranscription([]byte(sampleVerbose), m, cfg, "fallback-id")
	if err != nil {
		t.Fatal(err)
	}
	g := decodeGemini(t, res.Payload)
	c := g.Candidates[0]
	if c.Content.Role != "model" || c.FinishReason != "STOP" {
		t.Fatalf("role/finish %q %q", c.Content.Role, c.FinishReason)
	}
	if *c.Content.Parts[0].Text != "Merhaba dünya. <ok> & bitti." {
		t.Fatalf("text %q", *c.Content.Parts[0].Text)
	}
	if !strings.Contains(string(res.Payload), "<ok> & bitti") {
		t.Fatal("HTML characters must not be escaped")
	}
	u := g.UsageMetadata
	if u.PromptTokenCount != 80 || u.CandidatesTokenCount != 7 || u.TotalTokenCount != 87 {
		t.Fatalf("usage %+v", u)
	}
	if len(u.PromptTokensDetails) != 1 || u.PromptTokensDetails[0].Modality != "AUDIO" {
		t.Fatalf("prompt details %+v", u.PromptTokensDetails)
	}
	if g.ModelVersion != "groq-whisper-large-v3" || g.ResponseID != "req_01TESTID" {
		t.Fatalf("model/response id %q %q", g.ModelVersion, g.ResponseID)
	}
	if res.Segments != 2 || res.DroppedSegments != 0 || res.DurationSeconds != 2.5 {
		t.Fatalf("stats %+v", res)
	}
}

const silenceVerbose = `{
  "duration": 4.0,
  "text": " Real words. Altyazı M.K.",
  "segments": [
    {"text": " Real words.", "tokens": [1, 2, 3], "avg_logprob": -0.25, "no_speech_prob": 0.05},
    {"text": " Altyazı M.K.", "tokens": [4, 5, 6, 7], "avg_logprob": -1.4, "no_speech_prob": 0.93},
    {"text": " Confident.", "tokens": [8], "avg_logprob": -0.1, "no_speech_prob": 0.95}
  ]
}`

func TestMapTranscriptionDropsSilentSegments(t *testing.T) {
	cfg := mustConfig(t, "")
	m, _ := lookupModel("groq-whisper-large-v3-turbo")
	res, err := mapTranscription([]byte(silenceVerbose), m, cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	g := decodeGemini(t, res.Payload)
	// Segment 2 is high no_speech AND low logprob -> dropped. Segment 3 has a
	// high no_speech_prob but a confident decode -> kept (openai-whisper rule).
	if got := *g.Candidates[0].Content.Parts[0].Text; got != "Real words. Confident." {
		t.Fatalf("text %q", got)
	}
	if res.DroppedSegments != 1 || g.UsageMetadata.CandidatesTokenCount != 4 {
		t.Fatalf("dropped=%d tokens=%d", res.DroppedSegments, g.UsageMetadata.CandidatesTokenCount)
	}
	if g.ResponseID != "" {
		t.Fatalf("no id expected, got %q", g.ResponseID)
	}
}

func TestMapTranscriptionThresholdKnobs(t *testing.T) {
	m, _ := lookupModel("groq-whisper-large-v3")

	off := mustConfig(t, "no_speech_threshold: 1\n")
	res, _ := mapTranscription([]byte(silenceVerbose), m, off, "")
	if got := *decodeGemini(t, res.Payload).Candidates[0].Content.Parts[0].Text; got != "Real words. Altyazı M.K." {
		t.Fatalf("filtering must be disabled at threshold 1, got %q", got)
	}

	pure := mustConfig(t, "logprob_threshold: 0\n")
	res, _ = mapTranscription([]byte(silenceVerbose), m, pure, "")
	if got := *decodeGemini(t, res.Payload).Candidates[0].Content.Parts[0].Text; got != "Real words." {
		t.Fatalf("logprob_threshold 0 filters on no_speech_prob alone, got %q", got)
	}
}

func TestMapTranscriptionAllSilence(t *testing.T) {
	cfg := mustConfig(t, "")
	m, _ := lookupModel("groq-whisper-large-v3")
	body := `{"duration": 3, "text": " Altyazı M.K.", "segments": [{"text": " Altyazı M.K.", "tokens": [1], "avg_logprob": -2, "no_speech_prob": 0.99}]}`
	res, err := mapTranscription([]byte(body), m, cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	g := decodeGemini(t, res.Payload)
	if *g.Candidates[0].Content.Parts[0].Text != "" || g.UsageMetadata.CandidatesTokenCount != 0 {
		t.Fatalf("all-silence must yield an empty transcript: %s", res.Payload)
	}
	if g.UsageMetadata.PromptTokenCount != 96 {
		t.Fatalf("audio tokens %d", g.UsageMetadata.PromptTokenCount)
	}
}

func TestMapTranscriptionEstimatesTokensWhenMissing(t *testing.T) {
	cfg := mustConfig(t, "")
	m, _ := lookupModel("groq-whisper-large-v3")
	res, err := mapTranscription([]byte(`{"duration": 1.01, "text": "abcdefgh"}`), m, cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	g := decodeGemini(t, res.Payload)
	if g.UsageMetadata.CandidatesTokenCount != 2 || g.UsageMetadata.PromptTokenCount != 33 {
		t.Fatalf("usage %+v", g.UsageMetadata)
	}
}

func TestMapTranscriptionRejectsGarbage(t *testing.T) {
	cfg := mustConfig(t, "")
	m, _ := lookupModel("groq-whisper-large-v3")
	for _, body := range []string{"", "not json", "[1,2]", `{"text": 5}`} {
		if _, err := mapTranscription([]byte(body), m, cfg, ""); err == nil {
			t.Fatalf("expected error for %q", body)
		}
	}
}

func TestSanitizeID(t *testing.T) {
	if got := sanitizeID("req_01ab-CD.9:x\n<script>"); got != "req_01ab-CD.9:xscript" {
		t.Fatalf("sanitizeID = %q", got)
	}
	if len(sanitizeID(strings.Repeat("a", 500))) != 128 {
		t.Fatal("id must be bounded")
	}
}
