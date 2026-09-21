// Package logging is the zerolog bootstrap shared by every SYSTICORE Go
// service plus the request-scoped logger plumbing for HTTP (chi) and gRPC.
//
// The rule: the domain layer never imports this package. It calls
// zerolog.Ctx(ctx), which returns the logger the transport middleware put
// there (request id, method/rpc, user id…) or, outside a request, the root
// logger New installed as zerolog.DefaultContextLogger. No package-level
// Info()/Warn() wrappers, no Fatal — main wires the root logger into
// workers explicitly, so tests can hand them zerolog.Nop().
package logging

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog"
	"gopkg.in/natefinch/lumberjack.v2"
)

// Options is what a service's config maps onto — the module must not
// import any service's config package.
type Options struct {
	Service string
	Level   string // debug | info | warn | error; invalid → info + warning
	Pretty  bool   // console writer for local runs
	File    *FileOptions
}

// FileOptions enables the rotating file sink next to stdout.
type FileOptions struct {
	Dir        string
	MaxSizeMB  int
	MaxBackups int
	MaxAgeDays int
	Compress   bool
}

// New builds the root logger. close stops the daily rotation goroutine and
// closes the file; main defers it. The level lives on the root logger, not
// on zerolog's global, so tests and derived loggers stay isolated.
func New(o Options) (root zerolog.Logger, close func() error, err error) {
	var writers []io.Writer
	close = func() error { return nil }

	if o.File != nil {
		if err := os.MkdirAll(o.File.Dir, 0o750); err != nil {
			return zerolog.Nop(), nil, fmt.Errorf("logging: create log directory: %w", err)
		}
		fw := &lumberjack.Logger{
			Filename:   filepath.Join(o.File.Dir, o.Service+"-app.log"),
			MaxSize:    o.File.MaxSizeMB,
			MaxBackups: o.File.MaxBackups,
			MaxAge:     o.File.MaxAgeDays,
			Compress:   o.File.Compress,
		}
		ctx, cancel := context.WithCancel(context.Background())
		go dailyRotate(ctx, fw)
		close = func() error {
			cancel()
			return fw.Close()
		}
		writers = append(writers, fw)
	}
	if o.Pretty {
		writers = append(writers, zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339})
	} else {
		writers = append(writers, os.Stdout)
	}

	level, perr := zerolog.ParseLevel(o.Level)
	if perr != nil || o.Level == "" {
		level = zerolog.InfoLevel
	}
	zerolog.TimeFieldFormat = time.RFC3339

	root = zerolog.New(io.MultiWriter(writers...)).
		Level(level).
		With().Timestamp().Str("service", o.Service).
		Logger()
	if perr != nil && o.Level != "" {
		root.Warn().Str("provided_level", o.Level).Msg("invalid log level, using info")
	}

	// Code that reaches zerolog.Ctx with a bare context.Background()
	// (background jobs, tests) gets the service logger, not a silent one.
	zerolog.DefaultContextLogger = &root
	return root, close, nil
}

// dailyRotate forces a rotation at local midnight so one file never spans
// two days even when it stays under MaxSize.
func dailyRotate(ctx context.Context, l *lumberjack.Logger) {
	for {
		next := time.Now().Truncate(24 * time.Hour).Add(24 * time.Hour)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(next)):
			_ = l.Rotate()
		}
	}
}
