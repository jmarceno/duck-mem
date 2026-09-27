package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

type syncStatus struct {
	CompletedAt time.Time `json:"completed_at"`
	Files       int       `json:"files"`
	Skipped     int       `json:"skipped"`
}

func stateDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "duck-mem")
}

func syncStatusPath() string { return filepath.Join(stateDir(), "last-sync.json") }

func writeSyncStatus(dbPath string, status syncStatus) error {
	if filepath.Clean(dbPath) != filepath.Clean(defaultDB()) {
		return nil // the tray reports only the installed default database
	}
	if err := os.MkdirAll(stateDir(), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(stateDir(), ".sync-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if err := json.NewEncoder(f).Encode(status); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), syncStatusPath())
}

func readSyncStatus() (syncStatus, error) {
	var status syncStatus
	data, err := os.ReadFile(syncStatusPath())
	if err != nil {
		return status, err
	}
	err = json.Unmarshal(data, &status)
	return status, err
}
