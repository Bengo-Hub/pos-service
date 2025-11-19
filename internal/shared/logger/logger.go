package logger

import (
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// New creates a zap Logger tuned for the given environment.
func New(env string) (*zap.Logger, error) {
	cfg := zap.Config{
		Development:      env == "development",
		Level:            zap.NewAtomicLevelAt(zapcore.InfoLevel),
		Encoding:         "json",
		OutputPaths:      []string{"stdout"},
		ErrorOutputPaths: []string{"stderr"},
		EncoderConfig: zapcore.EncoderConfig{
			TimeKey:        "timestamp",
			MessageKey:     "message",
			LevelKey:       "level",
			CallerKey:      "caller",
			StacktraceKey:  "stack",
			EncodeTime:     zapcore.ISO8601TimeEncoder,
			EncodeDuration: zapcore.StringDurationEncoder,
			EncodeCaller:   zapcore.ShortCallerEncoder,
			EncodeLevel: func(l zapcore.Level, enc zapcore.PrimitiveArrayEncoder) {
				enc.AppendString(l.String())
			},
		},
	}

	if env == "development" {
		cfg.Encoding = "console"
		cfg.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	}

	return cfg.Build()
}

