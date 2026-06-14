package skills

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// skillsShBaseURL 是 https://skills.sh 的主机名。它返回一个公共的
// JSON 搜索端点，但不暴露每个技能的元数据 — tar 包
// 下载直接使用每个搜索结果中列出的源仓库到 codeload.github.com。
const skillsShBaseURL = "https://skills.sh"

// SkillsShResult 是 skills.sh 搜索 API 返回的一个条目。
type SkillsShResult struct {
	ID       string `json:"id"`       // "<owner>/<repo>/<skillId>"（仅显示）
	SkillID  string `json:"skillId"`  // 源仓库中的技能文件夹名称
	Name     string `json:"name"`     // 人类可读的名称
	Source   string `json:"source"`   // "<owner>/<repo>" — GitHub 位置
	Installs int    `json:"installs"` // 用于排名的流行度提示
}

// SearchSkillsSh 查询 https://skills.sh/api/search?q=... 并返回
// 原始结果。空切片表示没有匹配项。
func SearchSkillsSh(query string) ([]SkillsShResult, error) {
	u := fmt.Sprintf("%s/api/search?q=%s", skillsShBaseURL, url.QueryEscape(query))
	resp, err := defaultHTTPClient().Get(u)
	if err != nil {
		return nil, fmt.Errorf("skills.sh search: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("skills.sh search HTTP %d", resp.StatusCode)
	}
	var body struct {
		Skills []SkillsShResult `json:"skills"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode skills.sh: %w", err)
	}
	return body.Skills, nil
}

// PickSkillsShExact 返回与 name 最匹配的结果：精确匹配 skillId
// 优先；否则回退到安装次数最多的条目。results 为空时返回 nil。
func PickSkillsShExact(results []SkillsShResult, name string) *SkillsShResult {
	if len(results) == 0 {
		return nil
	}
	var best *SkillsShResult
	for i := range results {
		r := &results[i]
		if r.SkillID == name {
			return r
		}
		if best == nil || r.Installs > best.Installs {
			best = r
		}
	}
	return best
}

// InstallFromSkillsSh 将 skills.sh 结果 r 安装到 targetDir/<r.SkillID>/。
// 它获取源仓库的 tar 包（依次尝试 main 和 master 分支），找到
// tar 包内技能文件夹的路径（技能可能位于仓库的任意深度），并提取该文件夹。
func InstallFromSkillsSh(r SkillsShResult, targetDir string) (*Result, error) {
	if r.SkillID == "" || r.Source == "" {
		return nil, fmt.Errorf("skills.sh result missing skillId/source")
	}
	parts := strings.SplitN(r.Source, "/", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("skills.sh source %q is not owner/repo", r.Source)
	}
	owner, repo := parts[0], parts[1]
	// "source" 字段有时包含附加在 owner/repo 后的仓库内部子路径
	//（例如 "claude-office-skills/skills"）。GitHub 仓库只有
	// 两段式 slug，因此再次分割并将剩余部分作为前缀提示，
	// 用于消除 tar 包探测的歧义。
	prefixHint := ""
	if idx := strings.IndexByte(repo, '/'); idx >= 0 {
		prefixHint = repo[idx+1:]
		repo = repo[:idx]
	}

	client := defaultHTTPClient()
	var lastErr error
	// 首先尝试仓库的实际默认分支，然后回退到常见约定。
	// 许多 skills.sh 条目指向具有非标准分支（例如 `trunk`、`develop`、`dev`）的仓库 —
	// 没有 API 探测，即使仓库存在并包含技能，我们也会返回 404。
	// 去重以避免当默认分支已经是 `main`/`master` 时重复访问同一引用。
	refs := []string{"main", "master"}
	if def := githubDefaultBranch(client, owner, repo); def != "" {
		if def != "main" && def != "master" {
			refs = append([]string{def}, refs...)
		} else {
			// 将匹配的引用移到前面以短路快乐路径。
			refs = append([]string{def}, filterOut(refs, def)...)
		}
	}
	for _, ref := range refs {
		tarURL := fmt.Sprintf("https://codeload.github.com/%s/%s/tar.gz/refs/heads/%s", owner, repo, ref)

		// 探测一次以发现技能文件夹在 tar 包中的真实子路径。
		// 对于小型仓库来说这很廉价，并且避免了重复下载，
		// 因为流式 tar 读取器在第一次匹配时就会退出。
		subpath, err := findSkillDirInTarball(client, tarURL, r.SkillID)
		if err != nil {
			lastErr = err
			continue
		}
		if subpath == "" {
			// 当探测未找到任何内容但 skills.sh 的 "source" 提示了子路径时，
			// 回退到前缀提示。
			if prefixHint != "" {
				subpath = prefixHint + "/" + r.SkillID
			} else {
				lastErr = fmt.Errorf("skill %q not found in %s/%s@%s", r.SkillID, owner, repo, ref)
				continue
			}
		}

		dest := fmt.Sprintf("%s/%s", strings.TrimRight(targetDir, "/"), r.SkillID)
		n, err := extractSubpath(client, tarURL, subpath, dest)
		if err != nil {
			lastErr = err
			continue
		}
		if n == 0 {
			lastErr = fmt.Errorf("extracted no files from %s (subpath %q)", tarURL, subpath)
			continue
		}
		return &Result{
			Source:     "skills.sh",
			Name:       r.SkillID,
			Version:    ref,
			InstalledAt: dest,
			FilesWritten: n,
		}, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no main or master branch on %s/%s", owner, repo)
	}
	return nil, lastErr
}

// githubDefaultBranch 向 GitHub API 询问仓库的默认分支。
// 任何错误时返回 ""（API 速率限制、私有仓库等）— 调用者
// 回退到已知约定。仅尽力而为；我们从不因此调用阻塞安装路径。
func githubDefaultBranch(client *http.Client, owner, repo string) string {
	u := fmt.Sprintf("https://api.github.com/repos/%s/%s", owner, repo)
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return ""
	}
	// 显式 Accept 头使 v3 JSON 格式保持稳定。无认证头 —
	// 未经认证的请求有较低的速率限制（每个 IP 60/小时），但
	// 这对交互式安装来说已经足够，我们不想要求
	// 仅为了默认分支查询而配置令牌。
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var body struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return ""
	}
	return body.DefaultBranch
}

func filterOut(items []string, drop string) []string {
	out := make([]string, 0, len(items))
	for _, s := range items {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}
