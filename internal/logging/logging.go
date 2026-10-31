// Package logging builds the controller's zap-backed logr logger.
package logging

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	"go.uber.org/zap/zapcore"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// New keeps zap development mode off: it swaps in a console encoder, attaches stack traces and turns DPanic into a panic, so debug comes from the level instead.
func New(level zapcore.Level) logr.Logger {
	return zap.New(zap.UseFlagOptions(&zap.Options{Level: level}))
}

// ParseLevel maps --zap-log-level to a zap level; logr verbosity is the negative of it, so V(1) needs zapcore.DebugLevel (-1).
func ParseLevel(s string) (zapcore.Level, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return zapcore.InfoLevel, nil
	}
	// zapcore.ParseLevel only accepts non-negative numbers, so the numeric form is handled here to keep logr verbosity above V(0) reachable
	if n, err := strconv.Atoi(s); err == nil {
		if n < math.MinInt8 || n > math.MaxInt8 {
			return zapcore.InfoLevel, fmt.Errorf("invalid --zap-log-level %q: out of range", s)
		}
		return zapcore.Level(n), nil
	}
	level, err := zapcore.ParseLevel(s)
	if err != nil {
		return zapcore.InfoLevel, fmt.Errorf("invalid --zap-log-level %q: %w", s, err)
	}
	return level, nil
}
