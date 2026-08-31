// Package telemetry contains the Launcher's small, dependency-free telemetry
// primitives.  HTTP logging lives here so every network boundary uses the
// same request identity and structured event contract.
package telemetry

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

const (
	serviceName          = "launcher"
	clientRequestEvent   = "http.client.request.completed"
	serverRequestEvent   = "http.server.request.completed"
	requestIDHeader      = "X-Request-Id"
	requestIDRandomBytes = 12
)

type (
	loggerContextKey    struct{}
	requestIDContextKey struct{}
)

// WithLogger associates a logger with a request tree.  Callers that do not
// provide one use slog.Default, which keeps library use and tests useful while
// the desktop application can still send records to launcher.log.
func WithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	if ctx == nil || logger == nil {
		return ctx
	}
	return context.WithValue(ctx, loggerContextKey{}, logger)
}

func loggerFromContext(ctx context.Context) *slog.Logger {
	if logger, ok := loggerValue(ctx); ok {
		return logger
	}
	return slog.Default()
}

func loggerValue(ctx context.Context) (*slog.Logger, bool) {
	if ctx == nil {
		return nil, false
	}
	if logger, ok := ctx.Value(loggerContextKey{}).(*slog.Logger); ok && logger != nil {
		return logger, true
	}
	return nil, false
}

// WithRequestID associates an HTTP request identity with a context so nested
// work and logs can correlate with the X-Request-Id header.
func WithRequestID(ctx context.Context, requestID string) context.Context {
	if ctx == nil {
		return ctx
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return ctx
	}
	return context.WithValue(ctx, requestIDContextKey{}, requestID)
}

// RequestID returns the request identity carried by ctx, if any.
func RequestID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	requestID, _ := ctx.Value(requestIDContextKey{}).(string)
	return strings.TrimSpace(requestID)
}

// EnsureRequestID preserves a caller-supplied request identity and generates
// one when an outbound request starts without it.
func EnsureRequestID(request *http.Request) string {
	if request == nil {
		return newRequestID()
	}
	if request.Header == nil {
		request.Header = make(http.Header)
	}
	requestID := strings.TrimSpace(request.Header.Get(requestIDHeader))
	if requestID == "" {
		requestID = RequestID(request.Context())
		if requestID == "" {
			requestID = newRequestID()
		}
		request.Header.Set(requestIDHeader, requestID)
	}
	// Request.WithContext returns a shallow copy. Assigning that copy back to
	// the caller-owned request preserves the existing API while ensuring the
	// context seen by the transport and downstream handlers carries the same ID.
	*request = *request.WithContext(WithRequestID(request.Context(), requestID))
	return requestID
}

// CountedReadCloser records bytes consumed from a response body while keeping
// the original close behavior.  It lets streamed artifact downloads report
// actual response bytes rather than only an advertised Content-Length.
type CountedReadCloser struct {
	io.ReadCloser
	bytes atomic.Int64
}

func (body *CountedReadCloser) Read(contents []byte) (int, error) {
	count, err := body.ReadCloser.Read(contents)
	body.bytes.Add(int64(count))
	return count, err
}

// Bytes returns the number of response bytes consumed so far.
func (body *CountedReadCloser) Bytes() int64 { return body.bytes.Load() }

// RequestBytes returns the exact request size when net/http knows it.  A
// negative ContentLength means the caller streamed an unknown-sized body.
func RequestBytes(request *http.Request) int64 {
	if request == nil || request.ContentLength < 0 {
		return 0
	}
	return request.ContentLength
}

// LogHTTPClientRequest records one completed outbound HTTP exchange.  The
// error value is deliberately serialized as its unmodified text so operators
// can correlate the raw transport/API failure with the user-facing message.
func LogHTTPClientRequest(
	ctx context.Context,
	request *http.Request,
	response *http.Response,
	started time.Time,
	requestBytes int64,
	responseBytes int64,
	err error,
) {
	requestID := EnsureRequestID(request)
	logContext := ctx
	if request != nil {
		if _, ok := loggerValue(logContext); !ok {
			logContext = request.Context()
		}
	}
	status := 0
	method, route := "", ""
	if request != nil {
		method = request.Method
		if request.URL != nil {
			route = request.URL.Path
		}
	}
	if response != nil {
		status = response.StatusCode
		if responseBytes == 0 && response.ContentLength >= 0 {
			responseBytes = response.ContentLength
		}
	}
	if err == nil && (status < http.StatusOK || status >= http.StatusMultipleChoices) {
		err = fmt.Errorf("HTTP %d", status)
	}
	level := slog.LevelDebug
	if err != nil {
		level = slog.LevelError
	}
	attrs := []slog.Attr{
		slog.String("service", serviceName),
		slog.String("event", clientRequestEvent),
		slog.String("request_id", requestID),
		slog.String("method", method),
		slog.String("route", route),
		slog.String("path", route),
		slog.Int("status", status),
		slog.Int64("duration_ms", time.Since(started).Milliseconds()),
		slog.Int64("request_bytes", requestBytes),
		slog.Int64("response_bytes", responseBytes),
	}
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	loggerFromContext(logContext).LogAttrs(logContext, level, "HTTP client request completed", attrs...)
}

// HTTPServerMiddleware instruments Wails' AssetServer middleware boundary.
// Wails invokes this middleware for static asset/fallback requests; the
// framework-owned runtime.js/ipc/runtime branches are handled by the outer
// AssetServer and cannot be wrapped through the public Middleware option.
func HTTPServerMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if logger != nil {
				*request = *request.WithContext(WithLogger(request.Context(), logger))
			}
			requestID := EnsureRequestID(request)
			writer.Header().Set(requestIDHeader, requestID)
			started := time.Now()
			recorder := &serverResponseRecorder{ResponseWriter: writer}
			defer func() {
				if recovered := recover(); recovered != nil {
					logHTTPServerRequest(
						request.Context(), request, recorder, started, RequestBytes(request), recorder.bytes,
						fmt.Errorf("panic: %v", recovered),
					)
					panic(recovered)
				}
				logHTTPServerRequest(request.Context(), request, recorder, started, RequestBytes(request), recorder.bytes, nil)
			}()
			next.ServeHTTP(recorder, request)
		})
	}
}

type serverResponseRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (recorder *serverResponseRecorder) WriteHeader(status int) {
	if recorder.wroteHeader {
		return
	}
	recorder.status = status
	recorder.wroteHeader = true
	recorder.ResponseWriter.WriteHeader(status)
}

func (recorder *serverResponseRecorder) Write(contents []byte) (int, error) {
	if !recorder.wroteHeader {
		recorder.WriteHeader(http.StatusOK)
	}
	count, err := recorder.ResponseWriter.Write(contents)
	recorder.bytes += int64(count)
	return count, err
}

func (recorder *serverResponseRecorder) ReadFrom(source io.Reader) (int64, error) {
	if !recorder.wroteHeader {
		recorder.WriteHeader(http.StatusOK)
	}
	if reader, ok := recorder.ResponseWriter.(io.ReaderFrom); ok {
		count, err := reader.ReadFrom(source)
		recorder.bytes += count
		return count, err
	}
	return io.Copy(struct {
		io.Writer
	}{recorder}, source)
}

func (recorder *serverResponseRecorder) Flush() {
	if !recorder.wroteHeader {
		recorder.WriteHeader(http.StatusOK)
	}
	if flusher, ok := recorder.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (recorder *serverResponseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := recorder.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response writer does not support hijacking")
	}
	return hijacker.Hijack()
}

func (recorder *serverResponseRecorder) Push(target string, options *http.PushOptions) error {
	pusher, ok := recorder.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return pusher.Push(target, options)
}

func (recorder *serverResponseRecorder) Unwrap() http.ResponseWriter { return recorder.ResponseWriter }

// logHTTPServerRequest records a completed inbound HTTP request using the same
// field contract as outbound requests, with the server event name.
func logHTTPServerRequest(
	ctx context.Context,
	request *http.Request,
	response *serverResponseRecorder,
	started time.Time,
	requestBytes int64,
	responseBytes int64,
	err error,
) {
	requestID := EnsureRequestID(request)
	status := http.StatusOK
	method, route := "", ""
	if request != nil {
		method = request.Method
		if request.URL != nil {
			route = request.URL.Path
		}
	}
	if response != nil && response.status != 0 {
		status = response.status
	}
	if err == nil && (status < http.StatusOK || status >= http.StatusMultipleChoices) {
		err = fmt.Errorf("HTTP %d", status)
	}
	level := slog.LevelDebug
	if err != nil {
		level = slog.LevelError
	}
	attrs := []slog.Attr{
		slog.String("service", serviceName),
		slog.String("event", serverRequestEvent),
		slog.String("request_id", requestID),
		slog.String("method", method),
		slog.String("route", route),
		slog.String("path", route),
		slog.Int("status", status),
		slog.Int64("duration_ms", time.Since(started).Milliseconds()),
		slog.Int64("request_bytes", requestBytes),
		slog.Int64("response_bytes", responseBytes),
	}
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	loggerFromContext(ctx).LogAttrs(ctx, level, "HTTP server request completed", attrs...)
}

func newRequestID() string {
	buffer := make([]byte, requestIDRandomBytes)
	if _, err := rand.Read(buffer); err != nil {
		return "req_unavailable_" + fmt.Sprint(time.Now().UnixNano())
	}
	return "req_" + base64.RawURLEncoding.EncodeToString(buffer)
}
