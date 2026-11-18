//go:build !linux

package ingest

import (
	"os"
	"time"
)

// FileTimes reports the OS timestamps of a session's backing file on
// platforms without a portable birth-time API: both dates fall back to
// the modification time, the closest stand-in available.
func FileTimes(path string) (created, updated time.Time) {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}, time.Time{}
	}
	return info.ModTime(), info.ModTime()
}
