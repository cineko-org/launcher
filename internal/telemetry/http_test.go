package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestEnsureRequestIDGeneratesAndPropagates(t *testing.T) {
	request := &http.Request{Header: make(http.Header), URL: &url.URL{Path: "/health"}, Method: http.MethodGet}
	generated := EnsureRequestID(request)
	if generated == "" || request.Header.Get(requestIDHeader) != generated {
		t.Fatalf("generated request ID = %q, header = %q", generated, request.Header.Get(requestIDHeader))
	}
	request.Header.Set(requestIDHeader, "upstream-request")
	if got := EnsureRequestID(request); got != "upstream-request" {
		t.Fatalf("propagated request ID = %q", got)
	}
	request = request.WithContext(WithRequestID(context.Background(), "context-request"))
	if got := EnsureRequestID(request); got != "upstream-request" || RequestID(request.Context()) != "upstream-request" {
		t.Fatalf("header/context request ID = %q/%q", got, RequestID(request.Context()))
	}
	request = request.WithContext(WithRequestID(context.Background(), "context-request"))
	request.Header.Del(requestIDHeader)
	if got := EnsureRequestID(request); got != "context-request" || RequestID(request.Context()) != "context-request" || request.Header.Get(requestIDHeader) != "context-request" {
		t.Fatalf("context request ID was not propagated: got=%q context=%q header=%q", got, RequestID(request.Context()), request.Header.Get(requestIDHeader))
	}
}

func TestLogHTTPClientRequestEmitsCanonicalSuccessAndFailure(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	ctx := WithLogger(context.Background(), logger)
	request := &http.Request{
		Method:        http.MethodPost,
		Header:        http.Header{requestIDHeader: []string{"request-1"}},
		URL:           &url.URL{Path: "/v1/auth/pin"},
		ContentLength: 12,
	}
	response := &http.Response{StatusCode: http.StatusCreated}
	LogHTTPClientRequest(ctx, request, response, time.Now().Add(-time.Millisecond), 12, 42, nil)
	LogHTTPClientRequest(ctx, request, &http.Response{StatusCode: http.StatusBadGateway}, time.Now(), 12, 0, errors.New("upstream unavailable"))

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("log records = %d, output=%q", len(lines), output.String())
	}
	var success, failure map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &success); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &failure); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"service": "launcher", "event": clientRequestEvent, "request_id": "request-1",
		"method": "POST", "route": "/v1/auth/pin", "path": "/v1/auth/pin",
		"status": float64(http.StatusCreated), "request_bytes": float64(12), "response_bytes": float64(42),
	} {
		if success[key] != want {
			t.Fatalf("success[%q] = %#v, want %#v", key, success[key], want)
		}
	}
	if success["level"] != "INFO" {
		t.Fatalf("success level = %#v", success["level"])
	}
	if failure["level"] != "ERROR" || failure["error"] != "upstream unavailable" {
		t.Fatalf("failure record = %#v", failure)
	}
}

func TestLogHTTPClientRequestMarksHTTPFailureWithoutError(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	request := &http.Request{Method: http.MethodGet, Header: make(http.Header), URL: &url.URL{Path: "/health"}}
	LogHTTPClientRequest(WithLogger(context.Background(), logger), request, &http.Response{StatusCode: http.StatusServiceUnavailable}, time.Now(), 0, 0, nil)
	if !strings.Contains(output.String(), `"level":"ERROR"`) || !strings.Contains(output.String(), `"error":"HTTP 503"`) {
		t.Fatalf("HTTP failure log = %s", output.String())
	}
}

func TestHTTPServerMiddlewarePropagatesRequestIDAndLogsCompletion(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	handler := HTTPServerMiddleware(logger)(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get(requestIDHeader); got != "incoming-request" || RequestID(request.Context()) != got {
			t.Errorf("handler request identity = header=%q context=%q", got, RequestID(request.Context()))
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://launcher.local/app.js", nil)
	request.Header.Set(requestIDHeader, "incoming-request")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Header().Get(requestIDHeader) != "incoming-request" {
		t.Fatalf("response request ID = %q", response.Header().Get(requestIDHeader))
	}
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"service": "launcher", "event": serverRequestEvent, "request_id": "incoming-request",
		"method": "GET", "route": "/app.js", "path": "/app.js", "status": float64(http.StatusNoContent),
		"request_bytes": float64(0), "response_bytes": float64(0),
	} {
		if record[key] != want {
			t.Fatalf("server record[%q] = %#v, want %#v", key, record[key], want)
		}
	}
	if record["level"] != "INFO" {
		t.Fatalf("server level = %#v", record["level"])
	}
}
