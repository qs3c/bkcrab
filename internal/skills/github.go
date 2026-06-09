package skills

import (
	"fmt"
	"strings"
)

// InstallFromGitHubRepo installs a skill folder from a public GitHub repo
// identified by "owner/repo". If skillName is empty, the repo itself is
// assumed to be the skill (tarball root is extracted into
// targetDir/<repo>/). Otherwise it looks up the skill folder (at any depth)
// inside the repo and extracts it to targetDir/<skillName>/.
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
			// Whole-repo skill: extract the tarball top into targetDir/<name>.
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

// normalizeGitHubRepo strips common wrapper prefixes/suffixes so callers can
// pass things like "https://github.com/owner/repo.git" directly.
func normalizeGitHubRepo(repo string) string {
	repo = strings.TrimPrefix(repo, "https://github.com/")
	repo = strings.TrimPrefix(repo, "http://github.com/")
	repo = strings.TrimPrefix(repo, "github.com/")
	repo = strings.TrimSuffix(repo, ".git")
	repo = strings.Trim(repo, "/")
	return repo
}
