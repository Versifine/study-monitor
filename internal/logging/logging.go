package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"
)

const serviceName = "exam-monitor"

// Logger emits one JSON object per line with the fields required by M0.
type Logger struct {
	inner *slog.Logger
}

func New(output io.Writer, levelName, buildVersion string) (*Logger, error) {
	level, err := parseLevel(levelName)
	if err != nil {
		return nil, err
	}
	if buildVersion == "" {
		buildVersion = "unknown"
	}
	handler := slog.NewJSONHandler(output, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, attr slog.Attr) slog.Attr {
			switch attr.Key {
			case slog.TimeKey:
				return slog.String(slog.TimeKey, attr.Value.Time().UTC().Format(time.RFC3339Nano))
			case slog.LevelKey:
				return slog.String(slog.LevelKey, strings.ToLower(attr.Value.String()))
			default:
				return attr
			}
		},
	})
	base := slog.New(handler).With(
		slog.String("service", serviceName),
		slog.String("build_version", buildVersion),
	)
	return &Logger{inner: base}, nil
}

func (logger *Logger) Info(component, event, message string, attrs ...slog.Attr) {
	logger.log(slog.LevelInfo, component, event, "", message, attrs...)
}

func (logger *Logger) Warn(component, event, errorCode, message string, attrs ...slog.Attr) {
	logger.log(slog.LevelWarn, component, event, errorCode, message, attrs...)
}

func (logger *Logger) Error(component, event, errorCode, message string, err error, attrs ...slog.Attr) {
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	logger.log(slog.LevelError, component, event, errorCode, message, attrs...)
}

func (logger *Logger) log(level slog.Level, component, event, errorCode, message string, attrs ...slog.Attr) {
	base := []slog.Attr{
		slog.String("component", component),
		slog.String("event", event),
	}
	if errorCode != "" {
		base = append(base, slog.String("error_code", errorCode))
	}
	base = append(base, attrs...)
	logger.inner.LogAttrs(context.Background(), level, message, base...)
}

func parseLevel(name string) (slog.Level, error) {
	switch strings.ToLower(name) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unsupported log level %q", name)
	}
}
