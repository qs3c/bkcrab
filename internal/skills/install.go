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

// Result 描述一次成功的安装，以便调用者可以向用户显示发生了什么，
// 以及如果用户关心的话，在哪里可以找到文件。
type Result struct {
	Source       string `json:"source"`           // "skills.sh" | "clawhub" | "github"
	Name         string `json:"name"`             // final directory name under targetDir
	Version      string `json:"version,omitempty"`
	InstalledAt  string `json:"installedAt"`      // filesystem path of the new skill dir
	FilesWritten int    `json:"filesWritten"`
}

const clawhubBaseURL = "https://clawhub.ai"

// InstallFromClawHub 从 clawhub.ai 下载 `slug` 的最新版本，
// 并将其 ZIP 提取到 targetDir/<slug>/。成功时返回 Result。
func InstallFromClawHub(slug, targetDir string) (*Result, error) {
	if slug == "" {
		return nil, fmt.Errorf("skill slug required")
	}
	client := defaultHTTPClient()

	// 获取元数据以发现最新版本。
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

// extractZipToDir 将 zip 归档解压到 destDir，跳过包含路径遍历组件的条目。
// 返回写入的文件数量。
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

// InstallAuto 首先尝试 skills.sh，然后回退到 clawhub。返回第一个成功安装的
// Result，或描述两次未命中的组合错误。`name` 是用户要求的技能 slug/id。
//
// 这是 Agent 发起安装的默认路径：如果两个来源后都没有匹配项，
// 调用者应该提供 skill-creator 作为第三个选项。
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
	// ClawHub 回退。
	r, err := InstallFromClawHub(name, targetDir)
	if err == nil {
		return r, nil
	}
	if firstErr != nil {
		return nil, fmt.Errorf("not found on skills.sh (%v) or clawhub (%v)", firstErr, err)
	}
	return nil, fmt.Errorf("not found on skills.sh or clawhub (%v)", err)
}
