package main

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// errTrashCrossDevice means the file and the trash live on different
// filesystems, so renaming it there is impossible.
var errTrashCrossDevice = errors.New("the trash is on another filesystem")

// trashPath moves a file into the trash so nothing duck-mem does destroys
// user data outright. The freedesktop move keeps the operation deterministic;
// gio covers what the spec leaves to the desktop, a trash on another mount.
func trashPath(path string) error {
	err := trashHome(path)
	if !errors.Is(err, errTrashCrossDevice) {
		return err
	}
	gio, err := exec.LookPath("gio")
	if err != nil {
		return fmt.Errorf("cannot trash %s: %w", path, errTrashCrossDevice)
	}
	out, err := exec.Command(gio, "trash", "--", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("gio trash %s: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// trashHome moves path into the home trash per the freedesktop trash spec:
// the file is renamed into Trash/files and only then described by a
// .trashinfo entry, and if describing fails the file goes back.
func trashHome(path string) (err error) {
	src, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(src); err != nil {
		return err
	}
	filesDir, infoDir := trashDirs()
	if err := os.MkdirAll(filesDir, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(infoDir, 0o700); err != nil {
		return err
	}
	name := filepath.Base(src)
	dst := filepath.Join(filesDir, name)
	for i := 2; ; i++ {
		if _, err := os.Lstat(dst); errors.Is(err, fs.ErrNotExist) {
			break
		} else if err != nil {
			return err
		}
		dst = filepath.Join(filesDir, fmt.Sprintf("%s.%d", name, i))
	}
	if err := os.Rename(src, dst); err != nil {
		if errors.Is(err, syscall.EXDEV) {
			return errTrashCrossDevice
		}
		return err
	}
	defer func() {
		if err != nil {
			if back := os.Rename(dst, src); back != nil {
				err = fmt.Errorf("%w; restoring the file to %s failed: %v", err, src, back)
			}
		}
	}()
	info := fmt.Sprintf("[Trash Info]\nPath=%s\nDeletionDate=%s\n",
		trashInfoPath(src), time.Now().Format("2006-01-02T15:04:05"))
	if err := os.WriteFile(filepath.Join(infoDir, filepath.Base(dst)+".trashinfo"), []byte(info), 0o600); err != nil {
		return err
	}
	return nil
}

func trashDirs() (files, info string) {
	root := os.Getenv("XDG_DATA_HOME")
	if root == "" {
		home, _ := os.UserHomeDir()
		root = filepath.Join(home, ".local", "share")
	}
	root = filepath.Join(root, "Trash")
	return filepath.Join(root, "files"), filepath.Join(root, "info")
}

// trashInfoPath escapes an absolute path the way the spec's file URLs do:
// separators stay literal, everything the format cannot carry is percent-encoded.
func trashInfoPath(p string) string {
	u := url.URL{Scheme: "file", Path: p}
	return strings.TrimPrefix(u.String(), "file://")
}
