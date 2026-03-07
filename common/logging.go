package common

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

func ConfigureLogger(level, format string, writer io.Writer) error {
	var lvl slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		lvl = slog.LevelDebug
	case "info", "":
		lvl = slog.LevelInfo
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return fmt.Errorf("unknown log level %q", level)
	}

	opts := &slog.HandlerOptions{Level: lvl}
	var handler slog.Handler
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "text":
		handler = slog.NewTextHandler(writer, opts)
	case "json":
		handler = slog.NewJSONHandler(writer, opts)
	default:
		return fmt.Errorf("unknown log format %q", format)
	}

	slog.SetDefault(slog.New(handler))
	return nil
}
