package utils

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/Yukthi-Systems/YFS-Storage-API/internal/config"
	"github.com/natefinch/lumberjack"
)

// NewLogger builds the process-wide structured logger. It emits JSON to
// either stdout or a size/age rotated file on disk, controlled by
// cfg.LogOutput ("stdout" or "file") and, for the file case, cfg.LogDir /
// LogFileName / LogMaxSizeMB / LogMaxBackups / LogMaxAgeDays.
func NewLogger(cfg *config.Config) (*slog.Logger, error) {
	var writer io.Writer

	switch cfg.LogOutput {
	case "stdout":
		writer = os.Stdout
	default:
		if err := os.MkdirAll(cfg.LogDir, 0o755); err != nil {
			return nil, err
		}
		writer = &lumberjack.Logger{
			Filename:   filepath.Join(cfg.LogDir, cfg.LogFileName),
			MaxSize:    cfg.LogMaxSizeMB,
			MaxBackups: cfg.LogMaxBackups,
			MaxAge:     cfg.LogMaxAgeDays,
			Compress:   true,
		}
	}

	handler := slog.NewJSONHandler(writer, &slog.HandlerOptions{
		Level:     parseLevel(cfg.LogLevel),
		AddSource: false,
	})

	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger, nil
}

func parseLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
