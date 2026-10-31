package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// config holds the settings that survive restarts. The database location is
// the one thing every command and the installed services must agree on
// without a flag.
type config struct {
	Database string `json:"database"`
}

func configDir() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "duck-mem")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "duck-mem")
}

func configPath() string { return filepath.Join(configDir(), "config.json") }

// configuredDB returns the database location recorded by relocate, or ""
// when none was recorded. A corrupt file warns instead of silently falling
// back: reading a different database than the user filled would look like
// lost data.
func configuredDB() string {
	data, err := os.ReadFile(configPath())
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "ignoring unreadable %s: %v\n", configPath(), err)
		}
		return ""
	}
	var c config
	if err := json.Unmarshal(data, &c); err != nil {
		fmt.Fprintf(os.Stderr, "ignoring corrupt %s: %v\n", configPath(), err)
		return ""
	}
	return c.Database
}

// writeConfiguredDB records the database location atomically, so a crash
// mid-write cannot leave a truncated config behind.
func writeConfiguredDB(path string) error {
	if err := os.MkdirAll(configDir(), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(config{Database: path})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(configDir(), ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), configPath())
}
