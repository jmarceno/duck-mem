package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jmarceno/duck-mem/internal/project"
)

func addProject(t *testing.T, db *DB, name string, sessions int) {
	t.Helper()
	for i := 0; i < sessions; i++ {
		sid := fmt.Sprintf("%s-%d", name, i)
		if err := db.UpsertSession(Session{ID: sid, Source: "codex", Project: name, Path: sid}); err != nil {
			t.Fatal(err)
		}
	}
}

func resolveProject(t *testing.T, db *DB, request string) ProjectResolution {
	t.Helper()
	res, err := db.ResolveProjectFilter(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestProjectResolution(t *testing.T) {
	db := openTemp(t)
	addProject(t, db, "/home/u/Projects/celeste", 2)
	addProject(t, db, "/home/u/work/celeste-clone", 1)
	addProject(t, db, "/home/u/Projects/generic", 3)
	addProject(t, db, "/home/u/Projects/Generic", 1)
	addProject(t, db, "/home/u/Projects/omen-the-game", 4)
	addProject(t, db, "/home/u/Projects/organizer", 5)
	addProject(t, db, "/home/u/Projects/organizer-tiptap", 2)

	// Two checkouts of the same remote: one identity for both.
	remote := "github.com/u/celestial"
	for _, dir := range []string{"/home/u/Projects/celeste", "/home/u/work/celeste-clone"} {
		if err := db.SaveProjectIdentity(dir, project.GitIdentity{
			RepoRoot: dir, CommonDir: dir + "/.git", Remote: remote,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.RenameProject("/home/u/Projects/omen-the-game", "omen"); err != nil {
		t.Fatal(err)
	}
	// One shared message in each checkout of the remote: the resolved filter
	// must read both, and a failed resolution must read neither.
	for _, dir := range []string{"/home/u/Projects/celeste", "/home/u/work/celeste-clone"} {
		if err := db.InsertMessages([]Message{{
			SessionID: dir + "-0", Seq: 0, Source: "codex", Project: dir, Role: "user", Text: "shared needle",
		}}); err != nil {
			t.Fatal(err)
		}
	}

	// A name that only matched as a substring of a sibling must not blend
	// them: basename matching picks the exact directory.
	res := resolveProject(t, db, "organizer")
	if !slices.Equal(res.Filter.Values, []string{"/home/u/Projects/organizer"}) {
		t.Fatalf("basename match pulled in siblings: %+v", res)
	}

	// The same remote resolves a directory named celeste when asked by the
	// remote's name, which is the celeste/celestial case.
	res = resolveProject(t, db, "celestial")
	if res.Reason != ProjectReasonIdentity || !strings.Contains(res.IdentityKey, remote) {
		t.Fatalf("remote name did not resolve through identity: %+v", res)
	}
	if len(res.Filter.Values) != 2 || !slices.Contains(res.Filter.Values, "/home/u/Projects/celeste") ||
		!slices.Contains(res.Filter.Values, "/home/u/work/celeste-clone") {
		t.Fatalf("identity group incomplete: %+v", res.Filter.Values)
	}
	for _, st := range res.Matched {
		if st.Project == "/home/u/Projects/celeste" && st.Sessions != 2 {
			t.Fatalf("session count missing from advisory data: %+v", st)
		}
	}
	hits, err := db.SearchFiltered(context.Background(), "needle", res.Filter, "", 10)
	if err != nil || len(hits) != 2 {
		t.Fatalf("identity group filter read %d hits, err=%v", len(hits), err)
	}
	if hits[0].Project == hits[1].Project {
		t.Fatalf("both hits came from one checkout: %+v", hits)
	}
	if none, err := db.SearchFiltered(context.Background(), "needle", ProjectFilter{NoMatch: true}, "", 10); err != nil || len(none) != 0 {
		t.Fatalf("NoMatch filter returned %d hits, err=%v", len(none), err)
	}

	// Case-folded basenames can name two genuinely different projects; that
	// is reported, never silently blended.
	res = resolveProject(t, db, "generic")
	if !res.Ambiguous || res.Reason != ProjectReasonBasename || len(res.Matched) != 2 {
		t.Fatalf("case-only ambiguity not reported: %+v", res)
	}

	// A rename is queryable under its old name.
	res = resolveProject(t, db, "omen-the-game")
	if res.Reason != ProjectReasonAlias || !slices.Equal(res.Filter.Values, []string{"omen"}) {
		t.Fatalf("alias did not resolve: %+v", res)
	}

	// Nothing matches: suggest a similar name but filter nothing.
	res = resolveProject(t, db, "celesti")
	if res.Reason != ProjectReasonNone || !res.Filter.NoMatch || len(res.Candidates) == 0 {
		t.Fatalf("did-you-mean missing: %+v", res)
	}
	if res.Candidates[0].Project != "/home/u/Projects/celeste" {
		t.Fatalf("best candidate = %q", res.Candidates[0].Project)
	}

	// A requested path that is gone is called out.
	res = resolveProject(t, db, "/home/u/Projects/deleted")
	if res.Reason != ProjectReasonNone || !res.StalePath {
		t.Fatalf("stale path not reported: %+v", res)
	}
}

// A rename takes the name an agent or the tray user types, which is usually
// the directory's basename, and must not reach a sibling checkout of the
// same remote.
func TestRenameResolvesBasename(t *testing.T) {
	db := openTemp(t)
	addProject(t, db, "/home/u/Projects/gravaai", 3)
	addProject(t, db, "/home/u/Projects/Recordly", 2)
	addProject(t, db, "/home/u/Projects/dup", 1)
	addProject(t, db, "/home/u/work/dup", 1)
	for _, dir := range []string{"/home/u/Projects/gravaai", "/home/u/Projects/Recordly"} {
		if err := db.SaveProjectIdentity(dir, project.GitIdentity{
			RepoRoot: dir, CommonDir: dir + "/.git", Remote: "github.com/u/gravaai",
		}); err != nil {
			t.Fatal(err)
		}
	}

	res, err := db.RenameProject("gravaai", "gravaai-archive")
	if err != nil {
		t.Fatal(err)
	}
	if res.Old != "/home/u/Projects/gravaai" || res.Sessions != 3 {
		t.Fatalf("resolved %+v", res)
	}
	if res, err = db.RenameProject("recordly", "gravaai"); err != nil || res.Sessions != 2 {
		t.Fatalf("sibling checkout was not left intact: %+v err=%v", res, err)
	}
	if _, err := db.RenameProject("dup", "x"); err == nil || !strings.Contains(err.Error(), "several") {
		t.Fatalf("ambiguous basename was not refused: %v", err)
	}
}
