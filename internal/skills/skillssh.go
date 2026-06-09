package skills

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// skillsShBaseURL is the hostname for https://skills.sh. It returns a public
// JSON search endpoint but does not expose per-skill metadata — tarball
// downloads go directly to codeload.github.com using the source repo listed
// in each search result.
const skillsShBaseURL = "https://skills.sh"

// SkillsShResult is one entry returned by the skills.sh search API.
type SkillsShResult struct {
	ID       string `json:"id"`       // "<owner>/<repo>/<skillId>" (display-only)
	SkillID  string `json:"skillId"`  // folder name of the skill inside the source repo
	Name     string `json:"name"`     // human-readable name
	Source   string `json:"source"`   // "<owner>/<repo>" — the GitHub location
	Installs int    `json:"installs"` // popularity hint for ranking
}

// SearchSkillsSh queries https://skills.sh/api/search?q=... and returns the
// raw results. An empty slice means no matches.
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

// PickSkillsShExact returns the result that best matches name: exact skillId
// match wins; otherwise falls back to the most-installed entry. Returns nil
// when results is empty.
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

// InstallFromSkillsSh installs skills.sh result r into targetDir/<r.SkillID>/.
// It fetches the source repo's tarball (trying main then master), finds the
// in-tarball path of the skill folder (skills may live at arbitrary depth in
// the repo), and extracts that folder.
func InstallFromSkillsSh(r SkillsShResult, targetDir string) (*Result, error) {
	if r.SkillID == "" || r.Source == "" {
		return nil, fmt.Errorf("skills.sh result missing skillId/source")
	}
	parts := strings.SplitN(r.Source, "/", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("skills.sh source %q is not owner/repo", r.Source)
	}
	owner, repo := parts[0], parts[1]
	// The "source" field sometimes contains a repo-internal subpath appended
	// to owner/repo (e.g. "claude-office-skills/skills"). GitHub repos only
	// have two-segment slugs, so split again and treat the rest as a prefix
	// hint we can use to disambiguate tarball probing.
	prefixHint := ""
	if idx := strings.IndexByte(repo, '/'); idx >= 0 {
		prefixHint = repo[idx+1:]
		repo = repo[:idx]
	}

	client := defaultHTTPClient()
	var lastErr error
	// Try the repo's actual default branch first, then the common
	// conventions as fallback. Many skills.sh entries point at repos with
	// non-standard branches (e.g. `trunk`, `develop`, `dev`) — without the
	// API probe we'd 404 even though the repo exists and contains the
	// skill. Dedup so we don't hit the same ref twice when default is
	// already `main`/`master`.
	refs := []string{"main", "master"}
	if def := githubDefaultBranch(client, owner, repo); def != "" {
		if def != "main" && def != "master" {
			refs = append([]string{def}, refs...)
		} else {
			// Move matching ref to front to short-circuit the happy path.
			refs = append([]string{def}, filterOut(refs, def)...)
		}
	}
	for _, ref := range refs {
		tarURL := fmt.Sprintf("https://codeload.github.com/%s/%s/tar.gz/refs/heads/%s", owner, repo, ref)

		// Probe once to discover the real in-tarball subpath of the skill
		// folder. This is cheap for small repos and avoids double-downloads
		// because the streaming tar reader bails out on the first match.
		subpath, err := findSkillDirInTarball(client, tarURL, r.SkillID)
		if err != nil {
			lastErr = err
			continue
		}
		if subpath == "" {
			// Fall back to prefix hint when the probe finds nothing but
			// skills.sh's "source" hinted at a subpath.
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

// githubDefaultBranch asks the GitHub API for the repo's default branch.
// Returns "" on any error (API rate limit, private repo, etc.) — callers
// fall back to the well-known conventions. Best-effort only; we never
// block the install path on this call.
func githubDefaultBranch(client *http.Client, owner, repo string) string {
	u := fmt.Sprintf("https://api.github.com/repos/%s/%s", owner, repo)
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return ""
	}
	// Explicit Accept keeps the v3 JSON format stable. No auth header —
	// unauthenticated requests have a low rate limit (60/h per IP) but
	// that's enough for interactive installs and we don't want to require
	// configuring a token just for default-branch lookup.
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
