package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestMapUpstreamStatus(t *testing.T) {
	cases := map[int]int{
		400: 400, 401: 401, 402: 402, 403: 403, 404: 404, 409: 409, 413: 413, 415: 415, 422: 422, 424: 424, 429: 429,
		408: 504, 498: 503, 499: 502,
		500: 500, 502: 502, 503: 503, 504: 504,
		501: 502, 520: 502, 599: 502, 302: 502, 0: 502,
	}
	for in, want := range cases {
		if got := mapUpstreamStatus(in); got != want {
			t.Errorf("mapUpstreamStatus(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestUpstreamErrorMessage(t *testing.T) {
	body := `{"error":{"message":"Invalid API Key ` + testAPIKey + ` and ` + groqKeyPrefix + `OTHERkey123","type":"invalid_request_error","code":"invalid_api_key"}}`
	perr := upstreamError(401, nil, []byte(body), testAPIKey)
	if perr.Status != 401 || perr.Code != codeUpstream {
		t.Fatalf("status/code %d %s", perr.Status, perr.Code)
	}
	if strings.Contains(perr.Message, testAPIKey) || strings.Contains(perr.Message, "OTHERkey") {
		t.Fatalf("key leaked: %q", perr.Message)
	}
	if !strings.HasPrefix(perr.Message, "cpa-groq: Groq returned HTTP 401: Invalid API Key") {
		t.Fatalf("message %q", perr.Message)
	}
}

func TestUpstreamErrorRetryAfter(t *testing.T) {
	perr := upstreamError(429, http.Header{"retry-after": {"7"}}, []byte(`{"error":{"message":"Rate limit reached"}}`), testAPIKey)
	if perr.Status != 429 || !strings.Contains(perr.Message, "(retry after 7s)") {
		t.Fatalf("got %d %q", perr.Status, perr.Message)
	}
	date := time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)
	perr = upstreamError(503, http.Header{"Retry-After": {date}}, nil, "")
	if perr.Status != 503 || !strings.Contains(perr.Message, "retry after") {
		t.Fatalf("http-date retry-after not parsed: %q", perr.Message)
	}
	perr = upstreamError(500, http.Header{"Retry-After": {"banana"}}, []byte("<html>oops</html>"), "")
	if strings.Contains(perr.Message, "retry after") || perr.Message != "cpa-groq: Groq returned HTTP 500" {
		t.Fatalf("got %q", perr.Message)
	}
}

func TestSanitizeUpstreamText(t *testing.T) {
	in := "line1\nline2\x00\tend " + strings.Repeat("é", 400)
	out := sanitizeUpstreamText(in, "")
	if strings.ContainsAny(out, "\n\x00\t") {
		t.Fatalf("control characters kept: %q", out)
	}
	if len(out) > maxUpstreamMessageBytes+3 || !strings.HasSuffix(out, "...") {
		t.Fatalf("not truncated: %d bytes", len(out))
	}
}

func TestEnvelopes(t *testing.T) {
	env := decodeEnvelope(t, newError(codeAudioTooLarge, 413, "too big").envelope())
	if env.OK || env.Error == nil || env.Error.HTTPStatus != 413 || env.Error.Code != codeAudioTooLarge || env.Error.Message != "cpa-groq: too big" {
		t.Fatalf("bad envelope %+v", env.Error)
	}
	c := decodeEnvelope(t, errCanceled().envelope())
	if c.Error.HTTPStatus != 0 || c.Error.Message != "context canceled" {
		t.Fatalf("cancel envelope must carry no status and the exact lifecycle message: %+v", c.Error)
	}
}
