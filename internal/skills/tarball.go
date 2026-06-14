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

// extractSubpath 从 url 下载 gzip 压缩的 tar 包，并将 tar 包内路径匹配
// <topLevel>/<subpath>/<rest> 的文件写入 destDir/<rest>。
// 如果 subpath 为空，则提取顶级目录下的所有内容。
// 兼容 codeload.github.com 生成的 tar 包（每个文件以 repo-sha 目录为前缀），
// 并通过 ".." 组件阻止路径遍历。
//
// 返回写入的文件数量。
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
		// 去除归档文件的顶级目录（codeload 将所有文件包裹在
		// "<repo>-<sha>/" 中），我们对此不关心。
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

// findSkillDirInTarball 获取一次 tar 包，并返回 tar 包内 basename 为 skillID
// 且包含 SKILL.md 的子路径（相对于顶级目录），如果不存在则返回 ""。
//
// 用于 skills.sh 条目中技能位于源仓库任意深度的情况（例如 pdf-viewer/skills/view-pdf/）。
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

// defaultHTTPClient 是用于注册表调用的共享超时限制客户端。
func defaultHTTPClient() *http.Client {
	return &http.Client{Timeout: 60 * time.Second}
}
