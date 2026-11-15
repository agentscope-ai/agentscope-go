package logging

import (
	"context"
	"log/slog"

	"github.com/sirupsen/logrus"
)

// LogrusToSlogHook forwards logrus entries to the slog default logger.
type LogrusToSlogHook struct{}

func (h *LogrusToSlogHook) Levels() []logrus.Level {
	return logrus.AllLevels
}

func (h *LogrusToSlogHook) Fire(entry *logrus.Entry) error {
	level := logrusLevelToSlog(entry.Level)

	attrs := make([]slog.Attr, 0, len(entry.Data))
	for k, v := range entry.Data {
		attrs = append(attrs, slog.Any(k, v))
	}

	record := slog.NewRecord(entry.Time, level, entry.Message, 0)
	record.AddAttrs(attrs...)

	return Default().Handler().Handle(context.Background(), record)
}

func logrusLevelToSlog(l logrus.Level) slog.Level {
	switch l {
	case logrus.TraceLevel, logrus.DebugLevel:
		return slog.LevelDebug
	case logrus.InfoLevel:
		return slog.LevelInfo
	case logrus.WarnLevel:
		return slog.LevelWarn
	case logrus.ErrorLevel, logrus.FatalLevel, logrus.PanicLevel:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
