package skills

import (
	"fmt"
	"strings"
)

// InstallFromGitHubRepo 从由 "owner/repo" 标识的公共 GitHub 仓库安装技能文件夹。
// 如果 skillName 为空，则仓库本身被视为技能（tar 包根目录提取到
// targetDir/<repo>/）。否则在仓库内查找技能文件夹（任意深度）并将其提取到
// targetDir/<skillName>/。
func InstallFromGitHubRepo(repo, skillName, targetDir string) (*Result, error) {
	repo = normalizeGitHubRepo(repo)
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("repo must be owner/repo, got %q", repo)
	}
	owner, name := parts[0], parts[1]

	client := defaultHTTPClient()
	var lastErr error
	for _, ref := range []string{"main", "master"} {
		tarURL := fmt.Sprintf("https://codeload.github.com/%s/%s/tar.gz/refs/heads/%s", owner, name, ref)

		subpath := ""
		dest := ""
		installedName := skillName

		if skillName == "" {
			// 整个仓库作为技能：将 tar 包顶级目录提取到 targetDir/<name>。
			installedName = name
			dest = fmt.Sprintf("%s/%s", strings.TrimRight(targetDir, "/"), installedName)
		} else {
			found, err := findSkillDirInTarball(client, tarURL, skillName)
			if err != nil {
				lastErr = err
				continue
			}
			if found == "" {
				lastErr = fmt.Errorf("skill %q not found in %s/%s@%s", skillName, owner, name, ref)
				continue
			}
			subpath = found
			dest = fmt.Sprintf("%s/%s", strings.TrimRight(targetDir, "/"), skillName)
		}

		n, err := extractSubpath(client, tarURL, subpath, dest)
		if err != nil {
			lastErr = err
			continue
		}
		if n == 0 {
			lastErr = fmt.Errorf("extracted no files from %s", tarURL)
			continue
		}
		return &Result{
			Source:       "github",
			Name:         installedName,
			Version:      ref,
			InstalledAt:  dest,
			FilesWritten: n,
		}, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no main or master branch on %s", repo)
	}
	return nil, lastErr
}

// normalizeGitHubRepo 去除常见的包装前缀/后缀，以便调用者可以
// 直接传递类似 "https://github.com/owner/repo.git" 的内容。
func normalizeGitHubRepo(repo string) string {
	repo = strings.TrimPrefix(repo, "https://github.com/")
	repo = strings.TrimPrefix(repo, "http://github.com/")
	repo = strings.TrimPrefix(repo, "github.com/")
	repo = strings.TrimSuffix(repo, ".git")
	repo = strings.Trim(repo, "/")
	return repo
}
