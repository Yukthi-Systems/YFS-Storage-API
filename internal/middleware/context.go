// Package middleware provides thin net/http middleware: request-ID
// tagging, structured request logging, and panic recovery. Handlers are
// plain http.Handler/http.HandlerFunc — no framework-specific context
// types are introduced.
package middleware

import (
	"context"
	"log/slog"
	"sync"
)

type ctxKey int

const (
	requestIDKey ctxKey = iota
	logFieldsKey
)

// WithRequestID stores id in ctx.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestIDFromContext returns the request ID stored by the logging
// middleware, or "" if none is present.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// logFieldBag accumulates extra structured attributes (upload_id, path,
// file_id, ...) contributed by handlers/services over the lifetime of a
// single request, so the logging middleware can emit them all on one
// final access-log line instead of scattering them across separate calls.
type logFieldBag struct {
	mu    sync.Mutex
	attrs []slog.Attr
}

func withLogFieldBag(ctx context.Context) context.Context {
	return context.WithValue(ctx, logFieldsKey, &logFieldBag{})
}

// AddLogFields attaches extra slog attributes to the current request's
// access-log line. Safe to call from any handler or service that has the
// request context.
func AddLogFields(ctx context.Context, attrs ...slog.Attr) {
	bag, ok := ctx.Value(logFieldsKey).(*logFieldBag)
	if !ok {
		return
	}
	bag.mu.Lock()
	defer bag.mu.Unlock()
	bag.attrs = append(bag.attrs, attrs...)
}

func logFieldsFromContext(ctx context.Context) []slog.Attr {
	bag, ok := ctx.Value(logFieldsKey).(*logFieldBag)
	if !ok {
		return nil
	}
	bag.mu.Lock()
	defer bag.mu.Unlock()
	return append([]slog.Attr(nil), bag.attrs...)
}
