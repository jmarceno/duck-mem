package project

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestClean(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{"/home/u/Projects/omen-the-game/", "/home/u/Projects/omen-the-game"},
		{"/home/u//Projects///omen-the-game", "/home/u/Projects/omen-the-game"},
		{"/home/u/Projects/omen-the-game/.", "/home/u/Projects/omen-the-game"},
		{"~/Projects/omen", filepath.Join(home, "Projects/omen")},
		{`C:\Users\u\Projects\omen\`, "C:/Users/u/Projects/omen"},
		{`\\server\share\omen`, "//server/share/omen"},
		{"/", "/"},
		{"cursor:abc-def", "cursor:abc-def"},
		{"organizer", "organizer"},
	}
	for _, c := range cases {
		if got := Clean(c.in); got != c.want {
			t.Errorf("Clean(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCanonicalRemote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"git@github.com:User/Repo.git", "github.com/User/Repo"},
		{"https://github.com/user/repo.git", "github.com/user/repo"},
		{"https://github.com/user/repo/", "github.com/user/repo"},
		{"ssh://git@github.com/user/repo.git", "github.com/user/repo"},
		{"/home/u/repos/foo.git", "/home/u/repos/foo"},
		{"file:///home/u/repos/foo.git", "/home/u/repos/foo"},
	}
	for _, c := range cases {
		if got := CanonicalRemote(c.in); got != c.want {
			t.Errorf("CanonicalRemote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// The same remote has one identity key whichever spelling it arrived in.
	ssh := GitIdentity{Remote: CanonicalRemote("git@github.com:User/Repo.git")}
	https := GitIdentity{Remote: CanonicalRemote("https://github.com/user/repo.git")}
	if ssh.IdentityKey() != https.IdentityKey() {
		t.Fatalf("same remote, different keys: %q vs %q", ssh.IdentityKey(), https.IdentityKey())
	}
}

func TestNameScore(t *testing.T) {
	cases := []struct {
		request, project string
		want             bool
	}{
		{"celestial", "/home/u/Projects/celeste", true},
		{"organizer", "/home/u/Projects/organizer-tiptap", true},
		{"celestial", "/home/u/Projects/omen-the-game", false},
		{"generic", "/home/u/Projects/Generic", true},
	}
	for _, c := range cases {
		if got := NameScore(c.request, c.project) >= MinNameScore; got != c.want {
			t.Errorf("NameScore(%q, %q) = %v, candidate=%v want %v",
				c.request, c.project, NameScore(c.request, c.project), got, c.want)
		}
	}
}

func TestReadGitIdentity(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	git := func(at string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", at, "-c", "user.name=test", "-c", "user.email=test@example.com"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(dir, "init", "-q")
	git(dir, "remote", "add", "origin", "git@github.com:example/Project.git")
	git(dir, "commit", "--allow-empty", "-q", "-m", "initial")

	id, ok := ReadGitIdentity(dir)
	if !ok {
		t.Fatal("no identity for a git repository")
	}
	if id.RepoRoot != Clean(dir) {
		t.Fatalf("repo root = %q, want %q", id.RepoRoot, Clean(dir))
	}
	if id.Remote != "github.com/example/Project" {
		t.Fatalf("remote = %q", id.Remote)
	}

	wt := filepath.Join(t.TempDir(), "wt")
	git(dir, "worktree", "add", "-q", wt)
	wid, ok := ReadGitIdentity(wt)
	if !ok {
		t.Fatal("no identity for a worktree")
	}
	if wid.IdentityKey() != id.IdentityKey() {
		t.Fatalf("worktree and checkout have different identities: %q vs %q", wid.IdentityKey(), id.IdentityKey())
	}
}
