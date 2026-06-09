package skills

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// extractSubpath downloads a gzipped tar from url and writes files whose
// in-tarball path matches <topLevel>/<subpath>/<rest> into destDir/<rest>.
// If subpath is empty, everything under the top-level directory is extracted.
// It tolerates tarballs produced by codeload.github.com (each file is prefixed
// with a repo-sha directory) and stops path-traversal via ".." components.
//
// Returns the number of files written.
func extractSubpath(client *http.Client, url, subpath, destDir string) (int, error) {
	resp, err := client.Get(url)
	if err != nil {
		return 0, fmt.Errorf("download tarball: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("tarball HTTP %d: %s", resp.StatusCode, url)
	}

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("gunzip: %w", err)
	}
	defer gz.Close()

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return 0, err
	}
	absDest, err := filepath.Abs(destDir)
	if err != nil {
		return 0, err
	}

	wantPrefix := strings.TrimSuffix(subpath, "/")
	tr := tar.NewReader(gz)
	written := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return written, fmt.Errorf("read tar: %w", err)
		}
		name := hdr.Name
		// Strip the archive's top-level directory (codeload wraps everything
		// in "<repo>-<sha>/"), which we don't care about.
		slash := strings.IndexByte(name, '/')
		if slash < 0 {
			continue
		}
		rel := name[slash+1:]
		if rel == "" {
			continue
		}
		if wantPrefix != "" {
			if rel != wantPrefix && !strings.HasPrefix(rel, wantPrefix+"/") {
				continue
			}
			rel = strings.TrimPrefix(rel, wantPrefix)
			rel = strings.TrimPrefix(rel, "/")
			if rel == "" {
				continue
			}
		}

		target := filepath.Join(destDir, rel)
		absTarget, err := filepath.Abs(target)
		if err != nil {
			continue
		}
		if absTarget != absDest && !strings.HasPrefix(absTarget, absDest+string(filepath.Separator)) {
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return written, err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return written, err
			}
			mode := os.FileMode(hdr.Mode) & 0o777
			if mode == 0 {
				mode = 0o644
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return written, err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return written, err
			}
			f.Close()
			written++
		}
	}
	return written, nil
}

// findSkillDirInTarball fetches the tarball once and returns the in-tarball
// subpath (relative to the top-level dir) whose basename is skillID and which
// contains a SKILL.md, or "" if no such folder exists.
//
// Needed for skills.sh entries where the skill lives at an arbitrary depth in
// the source repo (e.g. pdf-viewer/skills/view-pdf/).
func findSkillDirInTarball(client *http.Client, url, skillID string) (string, error) {
	resp, err := client.Get(url)
	if err != nil {
		return "", fmt.Errorf("probe tarball: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("probe HTTP %d", resp.StatusCode)
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return "", err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	suffix := "/" + skillID + "/SKILL.md"
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		slash := strings.IndexByte(hdr.Name, '/')
		if slash < 0 {
			continue
		}
		rel := hdr.Name[slash+1:]
		if strings.HasSuffix(rel, suffix) || rel == skillID+"/SKILL.md" {
			return strings.TrimSuffix(rel, "/SKILL.md"), nil
		}
	}
	return "", nil
}

// defaultHTTPClient is the shared timeout-bounded client for registry calls.
func defaultHTTPClient() *http.Client {
	return &http.Client{Timeout: 60 * time.Second}
}
