package session

import (
	"strings"

	"github.com/rs/zerolog"

	"github.com/gammons/slk/internal/debuglog"
)

// DebugLogger is the logger slk gives New (and anything else in the Matrix
// backend that logs through zerolog). When slk's debug log is on
// (SLK_DEBUG, internal/debuglog), it writes each entry there as one
// "[general] matrix: …" line at debug level and above; otherwise it
// discards everything. It never writes to the terminal, which the TUI
// owns. Call it after debuglog.Init.
func DebugLogger() zerolog.Logger {
	if !debuglog.Enabled() {
		return zerolog.Nop()
	}
	w := zerolog.ConsoleWriter{
		Out:     debugWriter{},
		NoColor: true,
		// debuglog stamps each line itself.
		PartsExclude: []string{zerolog.TimestampFieldName},
	}
	return zerolog.New(w).Level(zerolog.DebugLevel)
}

// debugWriter writes zerolog's console lines into the debug log.
type debugWriter struct{}

func (debugWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		debuglog.General("matrix: %s", line)
	}
	return len(p), nil
}
