package logger

import (
	"log/slog"
	"os"
)

var defaultLogger *slog.Logger

func init() {
	opts := &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}
	defaultLogger = slog.New(slog.NewTextHandler(os.Stdout, opts))
}

func SetLevel(level slog.Level) {
	opts := &slog.HandlerOptions{Level: level}
	defaultLogger = slog.New(slog.NewTextHandler(os.Stdout, opts))
}

func Debug(msg string, args ...any) { defaultLogger.Debug(msg, args...) }
func Info(msg string, args ...any)  { defaultLogger.Info(msg, args...) }
func Warn(msg string, args ...any)  { defaultLogger.Warn(msg, args...) }
func Error(msg string, args ...any) { defaultLogger.Error(msg, args...) }

func With(args ...any) *slog.Logger {
	return defaultLogger.With(args...)
}
