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

// SkillInfo 保存来自 ClawHub 注册表的技能元数据。
type SkillInfo struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Version     string `json:"version"`
	Downloads   int    `json:"downloads"`
	TarballURL  string `json:"tarballUrl,omitempty"`
}

// ClawHubClient 与 ClawHub 技能注册表通信。
type ClawHubClient struct {
	Registry   string
	HTTPClient *http.Client
}

// NewClawHubClient 创建一个使用默认值的客户端。
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

// Search 查询 ClawHub 注册表中与查询匹配的技能。
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

// Info 获取有关特定技能的详细信息。
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

// Install 下载并提取技能到 targetDir/slug/。
// 如果 API 返回 tar 包 URL，则下载并提取它。
// 如果可用，回退到 npx clawhub。
func (c *ClawHubClient) Install(slug string, version string, targetDir string) error {
	// 首先尝试基于 API 的安装
	info, err := c.fetchVersion(slug, version)
	if err == nil && info.TarballURL != "" {
		return c.downloadAndExtract(info.TarballURL, filepath.Join(targetDir, slug))
	}

	// 回退：如果可用则使用 npx clawhub CLI
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

// Update 检查较新版本并安装它。
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

		// 去除第一个路径组件（tar 包根目录）
		name := header.Name
		if idx := strings.IndexByte(name, '/'); idx >= 0 {
			name = name[idx+1:]
		}
		if name == "" {
			continue
		}

		target := filepath.Join(destDir, name)
		// 防止路径遍历
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

// ListInstalled 扫描目录以查找已安装的技能（包含 SKILL.md 的目录）。
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

// InstalledSkill 表示本地安装的技能。
type InstalledSkill struct {
	Name string
	Dir  string
}
