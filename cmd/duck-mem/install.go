package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	serviceName = "duck-mem.service"
	trayName    = "duck-mem-tray.service"
	unitMarker  = "# Managed by duck-mem\n"
	pathBlock   = "\n# duck-mem PATH begin\nexport PATH=\"$HOME/.local/bin:$PATH\"\n# duck-mem PATH end\n"
)

func userHome() string {
	home, _ := os.UserHomeDir()
	return home
}

func binPath() string   { return filepath.Join(userHome(), ".local", "bin", "duck-mem") }
func unitDir() string   { return filepath.Join(userHome(), ".config", "systemd", "user") }
func dataDir() string   { return filepath.Join(userHome(), ".local", "share", "duck-mem") }
func configDir() string { return filepath.Join(userHome(), ".config", "duck-mem") }

func daemonUnit() string {
	return unitMarker + `[Unit]
Description=Duck-mem session memory indexer

[Service]
Type=simple
ExecStart=%h/.local/bin/duck-mem daemon --interval 5m
Restart=on-failure
RestartSec=10s
TimeoutStopSec=30min

[Install]
WantedBy=default.target
`
}

func trayUnit() string {
	return unitMarker + `[Unit]
Description=Duck-mem status tray
After=graphical-session.target
PartOf=graphical-session.target

[Service]
Type=simple
ExecStart=%h/.local/bin/duck-mem tray
Restart=on-failure
RestartSec=10s

[Install]
WantedBy=graphical-session.target
`
}

func systemctl(args ...string) error {
	cmd := exec.Command("systemctl", append([]string{"--user"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl --user %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func writeManagedUnit(path, content string) error {
	if old, err := os.ReadFile(path); err == nil && !strings.HasPrefix(string(old), unitMarker) {
		return fmt.Errorf("refusing to replace unmanaged unit %s", path)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func installUser() error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("user services and SNI tray require Linux")
	}
	if os.Geteuid() == 0 {
		return fmt.Errorf("run --install as the desktop user, not root")
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(binPath()), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(unitDir(), 0o755); err != nil {
		return err
	}
	for _, name := range []string{serviceName, trayName} {
		path := filepath.Join(unitDir(), name)
		if old, err := os.ReadFile(path); err == nil && !strings.HasPrefix(string(old), unitMarker) {
			return fmt.Errorf("refusing to replace unmanaged unit %s", path)
		} else if err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := copyExecutable(self, binPath()); err != nil {
		return err
	}
	if err := ensureUserPath(); err != nil {
		return err
	}
	if err := writeManagedUnit(filepath.Join(unitDir(), serviceName), daemonUnit()); err != nil {
		return err
	}
	if err := writeManagedUnit(filepath.Join(unitDir(), trayName), trayUnit()); err != nil {
		return err
	}
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	if err := systemctl("enable", serviceName, trayName); err != nil {
		return err
	}
	if err := systemctl("restart", serviceName, trayName); err != nil {
		return err
	}
	fmt.Printf("Installed %s; started %s and %s\n", binPath(), serviceName, trayName)
	return nil
}

func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.CreateTemp(filepath.Dir(dst), ".duck-mem-*")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Chmod(0o755); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(out.Name(), dst)
}

func ensureUserPath() error {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.Clean(dir) == filepath.Dir(binPath()) {
			return nil
		}
	}
	profile := filepath.Join(userHome(), ".profile")
	data, err := os.ReadFile(profile)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if strings.Contains(string(data), "# duck-mem PATH begin") {
		return nil
	}
	return os.WriteFile(profile, append(data, []byte(pathBlock)...), 0o644)
}

func uninstallUser(args []string) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("user services require Linux")
	}
	if os.Geteuid() == 0 {
		return fmt.Errorf("run --uninstall as the desktop user, not root")
	}
	var purge bool
	switch {
	case len(args) == 1 && args[0] == "--keep-data":
	case len(args) == 1 && args[0] == "--purge-data":
		purge = true
	case len(args) == 0:
		var err error
		purge, err = askPurge(os.Stdin, os.Stdout)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("usage: duck-mem --uninstall [--keep-data|--purge-data]")
	}
	for _, name := range []string{serviceName, trayName} {
		path := filepath.Join(unitDir(), name)
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !strings.HasPrefix(string(data), unitMarker) {
			return fmt.Errorf("refusing to remove unmanaged unit %s", path)
		}
	}
	for _, name := range []string{trayName, serviceName} {
		if _, err := os.Stat(filepath.Join(unitDir(), name)); err == nil {
			if err := systemctl("disable", "--now", name); err != nil {
				return err
			}
			if err := os.Remove(filepath.Join(unitDir(), name)); err != nil {
				return err
			}
		}
	}
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	if err := os.Remove(binPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := removeUserPath(); err != nil {
		return err
	}
	if purge {
		for _, dir := range []string{dataDir(), configDir(), stateDir()} {
			if err := os.RemoveAll(dir); err != nil {
				return err
			}
		}
		fmt.Println("Uninstalled duck-mem and deleted its database, config, and sync status.")
	} else {
		fmt.Println("Uninstalled duck-mem; kept its database, config, and sync status.")
	}
	return nil
}

func askPurge(in io.Reader, out io.Writer) (bool, error) {
	fmt.Fprintf(out, "Remove duck-mem database (%s), config (%s), and sync status (%s)? [y/N]: ", dataDir(), configDir(), stateDir())
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	switch answer {
	case "", "n", "no":
		return false, nil
	case "y", "yes":
		return true, nil
	default:
		return false, fmt.Errorf("answer yes or no; nothing was uninstalled")
	}
}

func removeUserPath() error {
	profile := filepath.Join(userHome(), ".profile")
	data, err := os.ReadFile(profile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	updated := strings.Replace(string(data), pathBlock, "", 1)
	if updated == string(data) {
		return nil
	}
	return os.WriteFile(profile, []byte(updated), 0o644)
}
