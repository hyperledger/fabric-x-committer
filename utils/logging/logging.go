/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package logging

import (
	"io"
	"os"
	"strings"
	"sync"

	"github.com/hyperledger/fabric-lib-go/common/flogging"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/grpc/grpclog"
)

// Logger wraps [zap.SugaredLogger] to allow updating all the loggers.
type Logger struct {
	*zap.SugaredLogger
	mu sync.Mutex
}

var loggerInstance Logger

// SetupWithConfig updates the logger with the given config.
func SetupWithConfig(config *Config) {
	grpclog.SetLoggerV2(grpclog.NewLoggerV2(io.Discard, io.Discard, os.Stderr))
	logSpec := strings.ToUpper(config.Level)
	if !config.Enabled {
		logSpec = "FATAL"
	}
	flogging.Init(flogging.Config{LogSpec: logSpec})
	loggerInstance.updateConfig(config)
}

// New returns a logger instance.
// TODO: use the name variable.
func New(_ string) *Logger {
	// Due to package initialization order, the Fabric package that initialize the GRPC logger
	// might be initialized after our own package.
	// Since methods calls can happen only after package initialization,
	// this call ensures the GRPC logger will stay quiet.
	grpclog.SetLoggerV2(grpclog.NewLoggerV2(io.Discard, io.Discard, os.Stderr))
	loggerInstance.initWithDefault()
	return &loggerInstance
}

// ErrorStackTrace prints the stack trace present in the error type.
func (l *Logger) ErrorStackTrace(err error) {
	if err == nil {
		return
	}
	l.WithOptions(zap.AddCallerSkip(1)).Errorf("%+v", err)
}

// WarnStackTrace prints the stack trace present in the error type as warning log.
func (l *Logger) WarnStackTrace(err error) {
	if err == nil {
		return
	}
	l.WithOptions(zap.AddCallerSkip(1)).Warnf("%+v", err)
}

func (l *Logger) updateConfig(config *Config) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.SugaredLogger = createLogger(config).Sugar()
}

func (l *Logger) initWithDefault() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.SugaredLogger == nil {
		l.SugaredLogger = createLogger(&DefaultConfig).Sugar()
	}
}

func createLogger(config *Config) *zap.Logger {
	if config == nil || !config.Enabled {
		return zap.NewNop()
	}

	level := zap.NewAtomicLevel()
	switch strings.ToUpper(config.Level) {
	case Debug:
		level.SetLevel(zap.DebugLevel)
	case Info:
		level.SetLevel(zap.InfoLevel)
	case Warning:
		level.SetLevel(zap.WarnLevel)
	case Error:
		level.SetLevel(zap.ErrorLevel)
	}
	outputs := []string{"stderr"}
	if config.Output != "" {
		outputs = append(outputs, config.Output)
	}

	var encCfg zapcore.EncoderConfig
	if config.Development {
		encCfg = zap.NewDevelopmentEncoderConfig()
		encCfg.EncodeLevel = zapcore.CapitalColorLevelEncoder
	} else {
		encCfg = zap.NewProductionEncoderConfig()
	}
	encCfg.EncodeTime = zapcore.ISO8601TimeEncoder
	encCfg.EncodeName = zapcore.FullNameEncoder

	c := zap.Config{
		Level:       level,
		Development: config.Development,
		Sampling: &zap.SamplingConfig{
			Initial:    100,
			Thereafter: 100,
		},
		Encoding:      "console",
		EncoderConfig: encCfg,
		// We don't need the stack trace at the logging point. We log the error point stack trace manually.
		DisableStacktrace: true,
		OutputPaths:       outputs,
		ErrorOutputPaths:  outputs,
	}

	return zap.Must(c.Build(zap.WithCaller(config.Caller))).Named(config.Name)
}
