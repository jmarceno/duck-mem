//go:build linux

package ingest

import (
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// FileTimes reports the OS timestamps of a session's backing file:
// birth time for created and modification time for last update. Not
// every filesystem tracks birth time, and a copied file's birth time is
// the copy's, so these are approximations — the inode change time and
// the modification time stand in when birth time is unavailable.
func FileTimes(path string) (created, updated time.Time) {
	var st unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, path, 0, unix.STATX_BTIME, &st); err == nil &&
		st.Mask&unix.STATX_BTIME != 0 && st.Btime.Sec > 0 {
		created = time.Unix(st.Btime.Sec, int64(st.Btime.Nsec))
	} else if info, err := os.Stat(path); err == nil {
		if s, ok := info.Sys().(*syscall.Stat_t); ok && s.Ctim.Sec > 0 {
			created = time.Unix(s.Ctim.Sec, int64(s.Ctim.Nsec))
		}
	}
	if info, err := os.Stat(path); err == nil {
		updated = info.ModTime()
	}
	return created, updated
}
