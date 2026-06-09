package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/spf13/cobra"
)

// versionCmd prints the current version info.
func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print FastClaw version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("FastClaw %s\n", version)
			fmt.Printf("  commit: %s\n", commit)
			fmt.Printf("  built:  %s\n", date)
			fmt.Printf("  go:     %s\n", runtime.Version())
			fmt.Printf("  os/arch: %s/%s\n", runtime.GOOS, runtime.GOARCH)
		},
	}
}

// upgradeCmd downloads and installs the latest release.
func upgradeCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "upgrade",
		Aliases: []string{"update"},
		Short:   "Upgrade FastClaw to the latest version",
		RunE: func(cmd *cobra.Command, args []string) error {
			return doUpgrade()
		},
	}
}

func doUpgrade() error {
	const repo = "fastclaw-ai/fastclaw"
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)

	fmt.Println("⚡ Checking for updates...")

	// 1. Fetch latest release info
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(apiURL)
	if err != nil {
		return fmt.Errorf("failed to check for updates: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var release struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return fmt.Errorf("failed to parse release info: %w", err)
	}

	// 2. Check if already up to date
	if release.TagName == version {
		fmt.Printf("✅ Already up to date (%s)\n", version)
		return nil
	}
	fmt.Printf("📦 New version available: %s → %s\n", version, release.TagName)

	// 3. Find the right asset for this platform
	goos := runtime.GOOS
	goarch := runtime.GOARCH
	var suffix string
	if goos == "windows" {
		suffix = fmt.Sprintf("fastclaw_%s_%s.zip", goos, goarch)
	} else {
		suffix = fmt.Sprintf("fastclaw_%s_%s.tar.gz", goos, goarch)
	}

	var downloadURL string
	for _, asset := range release.Assets {
		if asset.Name == suffix {
			downloadURL = asset.BrowserDownloadURL
			break
		}
	}
	if downloadURL == "" {
		return fmt.Errorf("no binary found for %s/%s in release %s", goos, goarch, release.TagName)
	}

	fmt.Printf("⬇️  Downloading %s...\n", suffix)

	// 4. Download to temp file
	dlResp, err := client.Get(downloadURL)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer dlResp.Body.Close()

	tmpFile, err := os.CreateTemp("", "fastclaw-update-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := io.Copy(tmpFile, dlResp.Body); err != nil {
		tmpFile.Close()
		return fmt.Errorf("download failed: %w", err)
	}
	tmpFile.Close()

	// 5. Extract binary
	tmpDir, err := os.MkdirTemp("", "fastclaw-extract-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	if goos == "windows" {
		// Unzip
		unzip := exec.Command("powershell", "-Command",
			fmt.Sprintf("Expand-Archive -Path '%s' -DestinationPath '%s' -Force", tmpPath, tmpDir))
		if out, err := unzip.CombinedOutput(); err != nil {
			return fmt.Errorf("unzip failed: %s: %w", string(out), err)
		}
	} else {
		// Untar
		tar := exec.Command("tar", "-xzf", tmpPath, "-C", tmpDir)
		if out, err := tar.CombinedOutput(); err != nil {
			return fmt.Errorf("untar failed: %s: %w", string(out), err)
		}
	}

	// 6. Find current binary path and replace
	currentBin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot find current binary: %w", err)
	}
	currentBin, _ = filepath.EvalSymlinks(currentBin)

	binaryName := "fastclaw"
	if goos == "windows" {
		binaryName = "fastclaw.exe"
	}
	newBin := filepath.Join(tmpDir, binaryName)

	if _, err := os.Stat(newBin); err != nil {
		return fmt.Errorf("extracted binary not found at %s", newBin)
	}

	// 7. Replace current binary
	fmt.Printf("📝 Installing to %s...\n", currentBin)

	// Try direct overwrite first; if permission denied, use sudo on Unix
	if err := replaceBinary(currentBin, newBin); err != nil {
		if goos != "windows" {
			fmt.Println("🔐 Need elevated permissions, trying sudo...")
			sudo := exec.Command("sudo", "cp", newBin, currentBin)
			sudo.Stdin = os.Stdin
			sudo.Stdout = os.Stdout
			sudo.Stderr = os.Stderr
			if err := sudo.Run(); err != nil {
				return fmt.Errorf("install failed (try: sudo cp %s %s): %w", newBin, currentBin, err)
			}
		} else {
			return fmt.Errorf("install failed: %w", err)
		}
	}

	fmt.Printf("✅ Upgraded to %s\n", release.TagName)
	return nil
}

func replaceBinary(dst, src string) error {
	srcData, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, srcData, 0o755)
}
