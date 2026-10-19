package project

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// GitIdentity is the repository a directory belongs to. CommonDir is shared
// by every worktree of one repository; Remote (canonical, see
// CanonicalRemote) is shared by worktrees and clones of the same upstream.
type GitIdentity struct {
	RepoRoot  string
	CommonDir string
	Remote    string
}

// gitProbeTimeout bounds each git call so a hung repository cannot stall an
// ingest cycle or a query.
const gitProbeTimeout = 2 * time.Second

// ReadGitIdentity probes the git identity of dir. It is best-effort by
// design: a missing directory, a non-repository, or a git binary that is
// absent or fails all report ok=false, and callers continue without it.
func ReadGitIdentity(dir string) (GitIdentity, bool) {
	dir = Clean(dir)
	// A relative value is a project name, not a checkout: probing it would
	// resolve against whatever the process working directory happens to be.
	if dir == "" || !filepath.IsAbs(dir) {
		return GitIdentity{}, false
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return GitIdentity{}, false
	}
	root, common, ok := gitRevParse(dir)
	if !ok || root == "" {
		return GitIdentity{}, false
	}
	id := GitIdentity{RepoRoot: Clean(root), CommonDir: Clean(common)}
	if remote := gitRemote(dir); remote != "" {
		id.Remote = CanonicalRemote(remote)
	}
	return id, true
}

// gitRevParse reads the top level of the working tree and the common git
// directory (shared by worktrees) in one call. The common directory is
// relative to the top level for a normal repository.
func gitRevParse(dir string) (root, common string, ok bool) {
	out, err := gitOutput(dir, "rev-parse", "--show-toplevel", "--git-common-dir")
	if err != nil {
		return "", "", false
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 0 {
		return "", "", false
	}
	root = strings.TrimSpace(lines[0])
	common = root
	if len(lines) > 1 {
		if c := strings.TrimSpace(lines[1]); c != "" {
			common = c
			if !filepath.IsAbs(common) {
				common = filepath.Join(root, common)
			}
		}
	}
	return root, common, true
}

func gitRemote(dir string) string {
	out, err := gitOutput(dir, "config", "--get", "remote.origin.url")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func gitOutput(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Output()
	return string(out), err
}

// IdentityKey is the grouping key of a repository: the canonical remote when
// there is one, else the common git directory, else the top level. Two
// projects with the same key are the same repository.
func (id GitIdentity) IdentityKey() string {
	switch {
	case id.Remote != "":
		return "remote:" + strings.ToLower(id.Remote)
	case id.CommonDir != "":
		return "common:" + strings.ToLower(id.CommonDir)
	case id.RepoRoot != "":
		return "root:" + strings.ToLower(id.RepoRoot)
	}
	return ""
}
