package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"
)

// Install installs FastClaw as an OS service.
func Install() error {
	switch runtime.GOOS {
	case "darwin":
		return installLaunchd()
	case "linux":
		return installSystemd()
	default:
		printWindowsInstructions()
		return nil
	}
}

// Uninstall removes the FastClaw OS service.
func Uninstall() error {
	switch runtime.GOOS {
	case "darwin":
		return uninstallLaunchd()
	case "linux":
		return uninstallSystemd()
	default:
		fmt.Println("Manual removal required on this platform.")
		return nil
	}
}

// --- macOS launchd ---

const launchdLabel = "ai.fastclaw.gateway"

var launchdPlistTemplate = template.Must(template.New("plist").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>{{.Label}}</string>
    <key>ProgramArguments</key>
    <array>
        <string>{{.BinaryPath}}</string>
        <string>gateway</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>{{.LogDir}}/gateway.stdout.log</string>
    <key>StandardErrorPath</key>
    <string>{{.LogDir}}/gateway.stderr.log</string>
    <key>WorkingDirectory</key>
    <string>{{.HomeDir}}</string>
</dict>
</plist>
`))

func launchdPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"), nil
}

func installLaunchd() error {
	bin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find executable: %w", err)
	}
	bin, _ = filepath.EvalSymlinks(bin)

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	logDir := filepath.Join(home, ".fastclaw", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}

	plistPath, err := launchdPlistPath()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return err
	}

	data := struct {
		Label      string
		BinaryPath string
		LogDir     string
		HomeDir    string
	}{
		Label:      launchdLabel,
		BinaryPath: bin,
		LogDir:     logDir,
		HomeDir:    home,
	}

	var buf strings.Builder
	if err := launchdPlistTemplate.Execute(&buf, data); err != nil {
		return fmt.Errorf("render plist: %w", err)
	}

	if err := os.WriteFile(plistPath, []byte(buf.String()), 0o644); err != nil {
		return fmt.Errorf("write plist: %w", err)
	}

	cmd := exec.Command("launchctl", "load", plistPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("launchctl load: %w", err)
	}

	fmt.Printf("Service installed: %s\n", plistPath)
	fmt.Println("FastClaw gateway will start automatically on login.")
	return nil
}

func uninstallLaunchd() error {
	plistPath, err := launchdPlistPath()
	if err != nil {
		return err
	}

	if _, err := os.Stat(plistPath); os.IsNotExist(err) {
		return fmt.Errorf("service not installed (no plist at %s)", plistPath)
	}

	cmd := exec.Command("launchctl", "unload", plistPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Printf("Warning: launchctl unload failed: %v\n", err)
	}

	if err := os.Remove(plistPath); err != nil {
		return fmt.Errorf("remove plist: %w", err)
	}

	fmt.Println("Service uninstalled.")
	return nil
}

// --- Linux systemd ---

const systemdUnit = "fastclaw-gateway.service"

var systemdUnitTemplate = template.Must(template.New("unit").Parse(`[Unit]
Description=FastClaw AI Agent Gateway
After=network.target

[Service]
Type=simple
ExecStart={{.BinaryPath}} gateway
Restart=always
RestartSec=5
Environment=HOME={{.HomeDir}}
WorkingDirectory={{.HomeDir}}
StandardOutput=append:{{.LogDir}}/gateway.stdout.log
StandardError=append:{{.LogDir}}/gateway.stderr.log

[Install]
WantedBy=default.target
`))

func systemdUnitPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user", systemdUnit), nil
}

func installSystemd() error {
	bin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find executable: %w", err)
	}
	bin, _ = filepath.EvalSymlinks(bin)

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	logDir := filepath.Join(home, ".fastclaw", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}

	unitPath, err := systemdUnitPath()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		return err
	}

	data := struct {
		BinaryPath string
		HomeDir    string
		LogDir     string
	}{
		BinaryPath: bin,
		HomeDir:    home,
		LogDir:     logDir,
	}

	var buf strings.Builder
	if err := systemdUnitTemplate.Execute(&buf, data); err != nil {
		return fmt.Errorf("render unit file: %w", err)
	}

	if err := os.WriteFile(unitPath, []byte(buf.String()), 0o644); err != nil {
		return fmt.Errorf("write unit file: %w", err)
	}

	// Enable and start
	for _, args := range [][]string{
		{"--user", "daemon-reload"},
		{"--user", "enable", systemdUnit},
		{"--user", "start", systemdUnit},
	} {
		cmd := exec.Command("systemctl", args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
		}
	}

	fmt.Printf("Service installed: %s\n", unitPath)
	fmt.Println("FastClaw gateway is running and will start on boot.")
	return nil
}

func uninstallSystemd() error {
	unitPath, err := systemdUnitPath()
	if err != nil {
		return err
	}

	if _, err := os.Stat(unitPath); os.IsNotExist(err) {
		return fmt.Errorf("service not installed (no unit at %s)", unitPath)
	}

	for _, args := range [][]string{
		{"--user", "stop", systemdUnit},
		{"--user", "disable", systemdUnit},
	} {
		cmd := exec.Command("systemctl", args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Run() // best-effort
	}

	if err := os.Remove(unitPath); err != nil {
		return fmt.Errorf("remove unit file: %w", err)
	}

	// Reload
	exec.Command("systemctl", "--user", "daemon-reload").Run()

	fmt.Println("Service uninstalled.")
	return nil
}

// --- Windows ---

func printWindowsInstructions() {
	fmt.Println("Automatic service installation is not supported on Windows.")
	fmt.Println()
	fmt.Println("Options:")
	fmt.Println("  1. Use NSSM (Non-Sucking Service Manager):")
	fmt.Println("     nssm install FastClaw <path-to-fastclaw.exe> gateway")
	fmt.Println()
	fmt.Println("  2. Use Task Scheduler:")
	fmt.Println("     - Open Task Scheduler")
	fmt.Println("     - Create a new task that runs 'fastclaw.exe gateway'")
	fmt.Println("     - Set it to run at startup")
}
