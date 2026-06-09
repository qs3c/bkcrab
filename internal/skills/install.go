package skills

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Result describes a successful install so callers can show the user what
// happened and, if they care, where to find the files.
type Result struct {
	Source       string `json:"source"`           // "skills.sh" | "clawhub" | "github"
	Name         string `json:"name"`             // final directory name under targetDir
	Version      string `json:"version,omitempty"`
	InstalledAt  string `json:"installedAt"`      // filesystem path of the new skill dir
	FilesWritten int    `json:"filesWritten"`
}

const clawhubBaseURL = "https://clawhub.ai"

// InstallFromClawHub downloads the latest version of `slug` from clawhub.ai
// and extracts its ZIP into targetDir/<slug>/. Returns a Result on success.
func InstallFromClawHub(slug, targetDir string) (*Result, error) {
	if slug == "" {
		return nil, fmt.Errorf("skill slug required")
	}
	client := defaultHTTPClient()

	// Fetch metadata to discover the latest version.
	metaURL := fmt.Sprintf("%s/api/v1/skills/%s", clawhubBaseURL, slug)
	metaResp, err := client.Get(metaURL)
	if err != nil {
		return nil, fmt.Errorf("clawhub metadata: %w", err)
	}
	defer metaResp.Body.Close()
	if metaResp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("skill %q not found on clawhub", slug)
	}
	if metaResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("clawhub metadata HTTP %d", metaResp.StatusCode)
	}
	var meta struct {
		Name          string `json:"name"`
		LatestVersion struct {
			Version string `json:"version"`
		} `json:"latestVersion"`
	}
	if err := json.NewDecoder(metaResp.Body).Decode(&meta); err != nil {
		return nil, fmt.Errorf("decode clawhub metadata: %w", err)
	}
	version := meta.LatestVersion.Version
	if version == "" {
		return nil, fmt.Errorf("clawhub has no published version for %q", slug)
	}

	dlURL := fmt.Sprintf("%s/api/v1/download?slug=%s&version=%s", clawhubBaseURL, slug, version)
	dlResp, err := client.Get(dlURL)
	if err != nil {
		return nil, fmt.Errorf("clawhub download: %w", err)
	}
	defer dlResp.Body.Close()
	if dlResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(dlResp.Body)
		return nil, fmt.Errorf("clawhub download HTTP %d: %s", dlResp.StatusCode, string(body))
	}
	zipData, err := io.ReadAll(dlResp.Body)
	if err != nil {
		return nil, fmt.Errorf("read zip: %w", err)
	}

	dest := filepath.Join(targetDir, slug)
	n, err := extractZipToDir(zipData, dest)
	if err != nil {
		return nil, err
	}
	return &Result{
		Source:       "clawhub",
		Name:         slug,
		Version:      version,
		InstalledAt:  dest,
		FilesWritten: n,
	}, nil
}

// extractZipToDir unpacks a zip archive into destDir, skipping entries with
// path-traversal components. Returns number of files written.
func extractZipToDir(data []byte, destDir string) (int, error) {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return 0, fmt.Errorf("bad zip: %w", err)
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return 0, err
	}
	absDest, err := filepath.Abs(destDir)
	if err != nil {
		return 0, err
	}
	written := 0
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := filepath.Clean(f.Name)
		if strings.Contains(name, "..") {
			continue
		}
		target := filepath.Join(destDir, name)
		absTarget, err := filepath.Abs(target)
		if err != nil {
			continue
		}
		if absTarget != absDest && !strings.HasPrefix(absTarget, absDest+string(filepath.Separator)) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return written, err
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			rc.Close()
			continue
		}
		if _, err := io.Copy(out, rc); err != nil {
			out.Close()
			rc.Close()
			return written, err
		}
		out.Close()
		rc.Close()
		written++
	}
	return written, nil
}

// InstallAuto tries skills.sh first and falls back to clawhub. Returns the
// Result of the first successful install, or a combined error describing
// both misses. `name` is the skill slug/id the user asked for.
//
// This is the default path for agent-initiated installs: if nothing matches
// after both sources, the caller is expected to offer skill-creator as a
// third option.
func InstallAuto(name, targetDir string) (*Result, error) {
	var firstErr error
	results, err := SearchSkillsSh(name)
	if err != nil {
		firstErr = fmt.Errorf("skills.sh search: %w", err)
	} else if pick := PickSkillsShExact(results, name); pick != nil && pick.SkillID == name {
		r, err := InstallFromSkillsSh(*pick, targetDir)
		if err == nil {
			return r, nil
		}
		firstErr = fmt.Errorf("skills.sh install: %w", err)
	}
	// ClawHub fallback.
	r, err := InstallFromClawHub(name, targetDir)
	if err == nil {
		return r, nil
	}
	if firstErr != nil {
		return nil, fmt.Errorf("not found on skills.sh (%v) or clawhub (%v)", firstErr, err)
	}
	return nil, fmt.Errorf("not found on skills.sh or clawhub (%v)", err)
}
