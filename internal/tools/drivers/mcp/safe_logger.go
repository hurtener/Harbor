package mcp

import (
	"context"
	"log/slog"
)

// safeSDKLogger keeps the SDK's log level and timestamp while suppressing
// messages and attributes it builds from raw HTTP errors. Those may contain
// the complete MCP endpoint URL, including query credentials, before Harbor
// gets a chance to wrap the returned error.
func safeSDKLogger(logger *slog.Logger) *slog.Logger {
	return slog.New(sdkSafeHandler{next: logger.Handler()})
}

type sdkSafeHandler struct{ next slog.Handler }

func (h sdkSafeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h sdkSafeHandler) Handle(ctx context.Context, record slog.Record) error {
	return h.next.Handle(ctx, slog.NewRecord(record.Time, record.Level, "mcp SDK transport event", record.PC))
}

func (h sdkSafeHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h sdkSafeHandler) WithGroup(string) slog.Handler { return h }
