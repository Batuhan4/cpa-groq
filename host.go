package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

// Host is the host-callback transport (host->call in the C ABI). The cgo
// implementation lives in cabi.go; tests use fakes.
type Host interface {
	// Call invokes a host callback and returns the raw JSON envelope.
	Call(method string, request []byte) ([]byte, error)
}

// hostCallError is a failed host callback, decoded from its error envelope.
type hostCallError struct {
	Code    string
	Message string
	Status  int
}

func (e *hostCallError) Error() string { return e.Message }

// callHost marshals req, invokes method and decodes a successful result into T.
func callHost[T any](h Host, method string, req any) (T, *hostCallError) {
	var zero T
	if h == nil {
		return zero, &hostCallError{Code: "host_unavailable", Message: "host callbacks are unavailable"}
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return zero, &hostCallError{Code: "marshal_failed", Message: "could not encode host request"}
	}
	resp, err := h.Call(method, raw)
	if err != nil {
		return zero, &hostCallError{Code: "host_call_failed", Message: err.Error()}
	}
	var env pluginabi.Envelope
	if err := json.Unmarshal(resp, &env); err != nil {
		return zero, &hostCallError{Code: "host_bad_envelope", Message: "host returned an undecodable envelope"}
	}
	if !env.OK {
		if env.Error == nil {
			return zero, &hostCallError{Code: "host_call_failed", Message: "host call failed"}
		}
		return zero, &hostCallError{Code: env.Error.Code, Message: env.Error.Message, Status: env.Error.HTTPStatus}
	}
	var out T
	if len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, &out); err != nil {
			return zero, &hostCallError{Code: "host_bad_result", Message: fmt.Sprintf("host returned an undecodable %s result", method)}
		}
	}
	return out, nil
}

// Wire types for host callbacks (see internal/pluginhost/host_callbacks.go).

type hostHTTPRequest struct {
	HostCallbackID string              `json:"host_callback_id,omitempty"`
	OperationID    string              `json:"operation_id,omitempty"`
	Method         string              `json:"method"`
	URL            string              `json:"url"`
	Headers        map[string][]string `json:"headers,omitempty"`
	Body           []byte              `json:"body,omitempty"`
}

// hostHTTPResponse is pluginapi.HTTPResponse as serialised by the host (no
// JSON tags there, so the Go field names are the keys).
type hostHTTPResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers"`
	Body       []byte      `json:"Body"`
}

type hostOperationOpenRequest struct {
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type hostOperationOpenResponse struct {
	OperationID string `json:"operation_id"`
}

type hostCancelRequest struct {
	HostCallbackID string `json:"host_callback_id,omitempty"`
	OperationID    string `json:"operation_id"`
}

type hostLogRequest struct {
	HostCallbackID string         `json:"host_callback_id,omitempty"`
	Level          string         `json:"level"`
	Message        string         `json:"message"`
	Fields         map[string]any `json:"fields,omitempty"`
}

type emptyResult struct{}

var errNoHost = errors.New("host callbacks are unavailable")
