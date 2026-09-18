// Package logger 提供统一的日志封装（基于标准库 slog）。
package logger

import (
	"log/slog"
	"os"
)

// Logger 是 slog.Logger 的别名，方便外部引用。
type Logger = slog.Logger

// New 创建一个 JSON 格式的 slog logger。
func New(level string) *Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	return slog.New(h)
}
