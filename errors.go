package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

// Error codes placed in pluginabi.Error.Code.
const (
	codeInvalidRequest   = "invalid_request"
	codeUnsupportedAudio = "unsupported_audio"
	codeAudioTooLarge    = "audio_too_large"
	codeUnknownModel     = "unknown_model"
	codeUnsupported      = "unsupported_operation"
	codeNotConfigured    = "not_configured"
	codeInvalidConfig    = "invalid_config"
	codeUpstream         = "upstream_error"
	codeUpstreamTimeout  = "upstream_timeout"
	codeUpstreamNetwork  = "upstream_unreachable"
	codeCanceled         = "canceled"
	codeInternal         = "internal_error"
	codeUnknownMethod    = "unknown_method"
)

// maxUpstreamMessageBytes bounds how much of Groq's error text is surfaced.
const maxUpstreamMessageBytes = 300

// pluginError is converted into a pluginabi error envelope. Status is the
// HTTP status CPA surfaces to the client (0 lets CPA pick, see README).
type pluginError struct {
	Code    string
	Message string
	Status  int
}

func (e *pluginError) Error() string { return e.Message }

func newError(code string, status int, format string, args ...any) *pluginError {
	return &pluginError{Code: code, Status: status, Message: "cpa-groq: " + fmt.Sprintf(format, args...)}
}

func (e *pluginError) envelope() []byte {
	raw, err := pluginabi.NewErrorEnvelope(e.Code, e.Message, e.Status)
	if err != nil {
		// json.Marshal of strings and ints cannot fail; keep a static fallback anyway.
		return []byte(`{"ok":false,"error":{"code":"internal_error","message":"cpa-groq: internal error","http_status":500}}`)
	}
	return raw
}

// errCanceled is returned when the client went away. The message is exactly
// "context canceled" and carries no HTTP status on purpose: CPA classifies
// that message as a connection-lifecycle event and does not cool down the
// credential because a caller hung up.
func errCanceled() *pluginError {
	return &pluginError{Code: codeCanceled, Message: "context canceled"}
}

// groqErrorBody is the documented Groq/OpenAI error shape.
type groqErrorBody struct {
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error"`
}

// groqKeyPrefix is Groq's API key prefix, assembled so the literal never
// appears in the source (the repo is scanned for it before every push).
const groqKeyPrefix = "gsk" + "_"

var groqKeyPattern = regexp.MustCompile(regexp.QuoteMeta(groqKeyPrefix) + `[A-Za-z0-9_\-]+`)

// mapUpstreamStatus converts a Groq HTTP status into the status reported to CPA.
//
//   - 4xx pass through unchanged (400/401/403/404/413/422/429 keep their meaning),
//     except 408 -> 504, 498 (Groq flex capacity) -> 503, 499 (Groq-side cancel) -> 502.
//   - 500/502/503/504 pass through; any other 5xx or unexpected status -> 502.
func mapUpstreamStatus(status int) int {
	switch {
	case status == http.StatusRequestTimeout:
		return http.StatusGatewayTimeout
	case status == 498:
		return http.StatusServiceUnavailable
	case status == 499:
		return http.StatusBadGateway
	case status >= 400 && status < 500:
		return status
	case status == http.StatusInternalServerError, status == http.StatusBadGateway,
		status == http.StatusServiceUnavailable, status == http.StatusGatewayTimeout:
		return status
	default:
		return http.StatusBadGateway
	}
}

// upstreamError builds the client-visible error for a non-2xx Groq response.
// The message includes Groq's own error text (sanitized, truncated, with any
// API key redacted) and the retry-after hint when Groq sent one.
func upstreamError(status int, headers http.Header, body []byte, apiKey string) *pluginError {
	mapped := mapUpstreamStatus(status)
	detail := ""
	var parsed groqErrorBody
	if json.Unmarshal(body, &parsed) == nil && parsed.Error != nil {
		detail = parsed.Error.Message
	}
	detail = sanitizeUpstreamText(detail, apiKey)
	msg := fmt.Sprintf("Groq returned HTTP %d", status)
	if detail != "" {
		msg += ": " + detail
	}
	if ra := retryAfter(headers); ra > 0 {
		msg += fmt.Sprintf(" (retry after %ds)", int(math.Ceil(ra.Seconds())))
	}
	return newError(codeUpstream, mapped, "%s", msg)
}

// retryAfter parses Groq's retry-after header (seconds, or an HTTP date).
func retryAfter(headers http.Header) time.Duration {
	if headers == nil {
		return 0
	}
	value := strings.TrimSpace(headerValue(headers, "Retry-After"))
	if value == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(value, 64); err == nil {
		if secs <= 0 || secs > 86400 {
			return 0
		}
		return time.Duration(secs * float64(time.Second))
	}
	if when, err := http.ParseTime(value); err == nil {
		d := time.Until(when)
		if d <= 0 || d > 24*time.Hour {
			return 0
		}
		return d
	}
	return 0
}

// headerValue is a case-insensitive header lookup; host-provided header maps
// are not guaranteed to use canonical keys.
func headerValue(headers http.Header, name string) string {
	if v := headers.Get(name); v != "" {
		return v
	}
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

// sanitizeUpstreamText redacts credentials, strips control characters and
// bounds the length of text that originates from Groq.
func sanitizeUpstreamText(s, apiKey string) string {
	if apiKey != "" {
		s = strings.ReplaceAll(s, apiKey, "[redacted]")
	}
	s = groqKeyPattern.ReplaceAllString(s, "[redacted key]")
	var b strings.Builder
	for _, r := range s {
		if r == utf8.RuneError {
			continue
		}
		if unicode.IsControl(r) {
			r = ' '
		}
		if b.Len()+utf8.RuneLen(r) > maxUpstreamMessageBytes {
			b.WriteString("...")
			break
		}
		b.WriteRune(r)
	}
	return strings.Join(strings.Fields(b.String()), " ")
}
