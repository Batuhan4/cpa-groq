package main

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

const userAgent = pluginName + "/" + pluginVersion

// postToGroq sends the multipart body through host.http.do so the call uses
// CPA's HTTP client, proxy settings and request-log capture.
//
// The host offers explicit cancellation but no per-call deadline, so the
// deadline is enforced here: an operation is opened first, and a watchdog
// goroutine calls host.http.cancel when cfg.Timeout elapses. The watchdog is
// always joined before this function returns, so no goroutine touches the
// host after the plugin call that started it has completed.
func (p *Plugin) postToGroq(cfg *Config, callbackID string, body []byte, contentType string) (*hostHTTPResponse, *pluginError) {
	op, herr := callHost[hostOperationOpenResponse](p.host, pluginabi.MethodHostHTTPOperationOpen, hostOperationOpenRequest{HostCallbackID: callbackID})
	if herr != nil || strings.TrimSpace(op.OperationID) == "" {
		if herr != nil && callbackID != "" && callbackScopeClosed(herr.Message) {
			// The request that owns this callback id is already gone.
			return nil, errCanceled()
		}
		return nil, newError(codeInternal, http.StatusInternalServerError, "host HTTP operation could not be opened")
	}
	opID := op.OperationID

	req := hostHTTPRequest{
		HostCallbackID: callbackID,
		OperationID:    opID,
		Method:         http.MethodPost,
		URL:            cfg.TranscriptionsURL,
		Headers: map[string][]string{
			"Authorization": {"Bearer " + cfg.APIKey},
			"Content-Type":  {contentType},
			"Accept":        {"application/json"},
			"User-Agent":    {userAgent},
		},
		Body: body,
	}

	stop := make(chan struct{})
	var timedOut bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() { _ = recover() }() // a panic here must never take CPA down
		timer := time.NewTimer(cfg.Timeout)
		defer timer.Stop()
		select {
		case <-timer.C:
			timedOut = true
			p.cancelOperation(callbackID, opID)
		case <-stop:
		}
	}()

	resp, herr := func() (hostHTTPResponse, *hostCallError) {
		// Deferred so the watchdog is stopped and joined even if the host
		// transport panics; wg.Wait also orders the read of timedOut below.
		defer func() {
			close(stop)
			wg.Wait()
		}()
		return callHost[hostHTTPResponse](p.host, pluginabi.MethodHostHTTPDo, req)
	}()

	if herr != nil {
		switch {
		case timedOut:
			return nil, newError(codeUpstreamTimeout, http.StatusGatewayTimeout, "Groq did not answer within %s", cfg.Timeout)
		case herr.Status == 499 || callbackScopeClosed(herr.Message):
			// Client went away (or the operation was canceled before the
			// host claimed it because the request scope closed).
			return nil, errCanceled()
		case herr.Status == http.StatusGatewayTimeout || strings.Contains(strings.ToLower(herr.Message), "deadline exceeded"):
			return nil, newError(codeUpstreamTimeout, http.StatusGatewayTimeout, "Groq request timed out")
		default:
			return nil, newError(codeUpstreamNetwork, http.StatusBadGateway, "Groq request failed: %s", sanitizeUpstreamText(herr.Message, cfg.APIKey))
		}
	}
	return &resp, nil
}

func (p *Plugin) cancelOperation(callbackID, opID string) {
	_, _ = callHost[emptyResult](p.host, pluginabi.MethodHostHTTPCancel, hostCancelRequest{HostCallbackID: callbackID, OperationID: opID})
}

// callbackScopeClosed recognises the host's messages for a request scope that
// has already ended (see internal/pluginhost/http_operation_bridge.go).
func callbackScopeClosed(message string) bool {
	m := strings.ToLower(message)
	for _, marker := range []string{"context canceled", "is not open", "callback context closed", "instance is closed"} {
		if strings.Contains(m, marker) {
			return true
		}
	}
	return false
}
