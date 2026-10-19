// Package logging configures the process-wide logger for the gpu-fleet
// binaries.
package logging

import (
	"log/slog"
	"os"
)

// Setup routes the default logger to stdout. The standard default writes to
// stderr, which log collectors such as Railway's label as errors.
func Setup() *slog.Logger {
	l := slog.New(slog.NewTextHandler(os.Stdout, nil))
	slog.SetDefault(l)
	return l
}
