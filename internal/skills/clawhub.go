package skills

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// SkillInfo holds metadata about a skill from the ClawHub registry.
type SkillInfo struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Version     string `json:"version"`
	Downloads   int    `json:"downloads"`
	TarballURL  string `json:"tarballUrl,omitempty"`
}

// ClawHubClient communicates with the ClawHub skill registry.
type ClawHubClient struct {
	Registry   string
	HTTPClient *http.Client
}

// NewClawHubClient creates a client with defaults.
func NewClawHubClient() *ClawHubClient {
	registry := os.Getenv("CLAWHUB_REGISTRY")
	if registry == "" {
		registry = "https://clawhub.com"
	}
	return &ClawHubClient{
		Registry:   strings.TrimRight(registry, "/"),
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// Search queries the ClawHub registry for skills matching the query.
func (c *ClawHubClient) Search(query string) ([]SkillInfo, error) {
	url := fmt.Sprintf("%s/api/skills/search?q=%s", c.Registry, query)
	resp, err := c.HTTPClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("search request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("search returned status %d", resp.StatusCode)
	}

	var results []SkillInfo
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		return nil, fmt.Errorf("decode search results: %w", err)
	}
	return results, nil
}

// Info fetches details about a specific skill.
func (c *ClawHubClient) Info(slug string) (*SkillInfo, error) {
	url := fmt.Sprintf("%s/api/skills/%s", c.Registry, slug)
	resp, err := c.HTTPClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("info request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("skill %q not found", slug)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("info returned status %d", resp.StatusCode)
	}

	var info SkillInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("decode skill info: %w", err)
	}
	return &info, nil
}

// Install downloads and extracts a skill to targetDir/slug/.
// If the API returns a tarball URL, downloads and extracts it.
// Falls back to npx clawhub if available.
func (c *ClawHubClient) Install(slug string, version string, targetDir string) error {
	// Try API-based install first
	info, err := c.fetchVersion(slug, version)
	if err == nil && info.TarballURL != "" {
		return c.downloadAndExtract(info.TarballURL, filepath.Join(targetDir, slug))
	}

	// Fallback: use npx clawhub CLI if available
	if npxPath, lookErr := exec.LookPath("npx"); lookErr == nil {
		args := []string{npxPath, "clawhub@latest", "install", slug, "--dir", targetDir}
		if version != "" {
			args = append(args, "--version", version)
		}
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}

	if err != nil {
		return fmt.Errorf("install %s: %w (npx not available as fallback)", slug, err)
	}
	return fmt.Errorf("install %s: no tarball URL and npx not available", slug)
}

// Update checks for a newer version and installs it.
func (c *ClawHubClient) Update(slug string, targetDir string) error {
	return c.Install(slug, "", targetDir)
}

func (c *ClawHubClient) fetchVersion(slug string, version string) (*SkillInfo, error) {
	url := fmt.Sprintf("%s/api/skills/%s", c.Registry, slug)
	if version != "" {
		url += fmt.Sprintf("?version=%s", version)
	}
	resp, err := c.HTTPClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch version returned status %d", resp.StatusCode)
	}

	var info SkillInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, err
	}
	return &info, nil
}

func (c *ClawHubClient) downloadAndExtract(tarballURL string, destDir string) error {
	resp, err := c.HTTPClient.Get(tarballURL)
	if err != nil {
		return fmt.Errorf("download tarball: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned status %d", resp.StatusCode)
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("create dir: %w", err)
	}

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}

		// Strip the first path component (tarball root)
		name := header.Name
		if idx := strings.IndexByte(name, '/'); idx >= 0 {
			name = name[idx+1:]
		}
		if name == "" {
			continue
		}

		target := filepath.Join(destDir, name)
		// Prevent path traversal
		if !strings.HasPrefix(filepath.Clean(target), filepath.Clean(destDir)) {
			continue
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}

	return nil
}

// ListInstalled scans a directory for installed skills (dirs containing SKILL.md).
func ListInstalled(dir string) ([]InstalledSkill, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var skills []InstalledSkill
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skillFile := filepath.Join(dir, entry.Name(), "SKILL.md")
		if _, statErr := os.Stat(skillFile); statErr != nil {
			continue
		}
		skills = append(skills, InstalledSkill{
			Name: entry.Name(),
			Dir:  filepath.Join(dir, entry.Name()),
		})
	}
	return skills, nil
}

// InstalledSkill represents a locally installed skill.
type InstalledSkill struct {
	Name string
	Dir  string
}
