package main

import (
	"flag"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jmarceno/duck-mem/internal/project"
	"github.com/jmarceno/duck-mem/internal/store"
)

// identityRecorder captures the git identity of changed sessions during an
// ingest cycle. Probing is best-effort and cached per directory, so a cycle
// that touches many sessions in one repository runs git once.
type identityRecorder struct {
	db   *store.DB
	seen map[string]bool
}

func newIdentityRecorder(db *store.DB) *identityRecorder {
	return &identityRecorder{db: db, seen: map[string]bool{}}
}

// record probes the directory a changed session was parsed from (before the
// rename alias rewrote its project name) and files the identity under the
// canonical stored project name. A missing directory, a non-repository or a
// failing git call is skipped: ingest must never fail for it.
func (r *identityRecorder) record(rawProject, canonical string) error {
	rawProject = project.Clean(rawProject)
	if rawProject == "" || canonical == "" || r.seen[rawProject] {
		return nil
	}
	r.seen[rawProject] = true
	id, ok := project.ReadGitIdentity(rawProject)
	if !ok {
		return nil
	}
	return r.db.SaveProjectIdentity(canonical, id)
}

func projectsCmd(args []string) {
	fs := flag.NewFlagSet("projects", flag.ExitOnError)
	dbPath := fs.String("db", defaultDB(), "DuckDB file")
	flagArgs, _ := splitArgs(args, map[string]bool{"--db": true})
	_ = fs.Parse(flagArgs)
	mem, err := openMemory(*dbPath, false)
	if err != nil {
		fatal(err)
	}
	defer mem.Close()
	list, err := mem.Projects()
	if err != nil {
		fatal(err)
	}
	groups := identityGroups(list)
	withIdentity := 0
	for _, p := range list {
		if projectIdentityKey(p) != "" {
			withIdentity++
		}
	}
	fmt.Printf("%d stored projects, %d with a captured git identity\n\n", len(list), withIdentity)
	labels := make([]string, len(list))
	width := 0
	for i, p := range list {
		labels[i] = p.Project
		if labels[i] == "" {
			labels[i] = "(no project)"
		}
		if n := utf8.RuneCountInString(labels[i]); n > width {
			width = n
		}
	}
	if width > 56 {
		width = 56
	}
	for i, p := range list {
		fmt.Printf("%-*s  %5d sessions  %-20s %s\n", width, shorten(labels[i], width), p.Sessions,
			strings.Join(p.Sources, ","), dateRange(p.First, p.Last))
		if key := projectIdentityKey(p); key != "" {
			switch {
			case p.RemoteURL != "" && p.RepoRoot != "":
				fmt.Printf("%-*s  git %s · %s\n", width, "", p.RemoteURL, p.RepoRoot)
			case p.RemoteURL != "":
				fmt.Printf("%-*s  git %s\n", width, "", p.RemoteURL)
			case p.RepoRoot != "":
				fmt.Printf("%-*s  git (local) %s\n", width, "", p.RepoRoot)
			default:
				fmt.Printf("%-*s  git (local) %s\n", width, "", p.CommonDir)
			}
			if len(groups[key]) > 1 {
				var others []string
				for _, m := range groups[key] {
					if m.Project != p.Project {
						others = append(others, fmt.Sprintf("%s (%d)", m.Project, m.Sessions))
					}
				}
				fmt.Printf("%-*s  same git identity: %s\n", width, "", strings.Join(others, ", "))
			}
		}
		if len(p.Aliases) > 0 {
			fmt.Printf("%-*s  aliases: %s\n", width, "", strings.Join(p.Aliases, ", "))
		}
	}
}

// identityGroups collects stored projects by shared git identity, so the
// collapse of worktrees and clones is visible.
func identityGroups(list []store.ProjectStat) map[string][]store.ProjectStat {
	groups := map[string][]store.ProjectStat{}
	for _, p := range list {
		if key := projectIdentityKey(p); key != "" {
			groups[key] = append(groups[key], p)
		}
	}
	return groups
}

func projectIdentityKey(p store.ProjectStat) string {
	switch {
	case p.RemoteURL != "":
		return "remote:" + strings.ToLower(p.RemoteURL)
	case p.CommonDir != "":
		return "common:" + strings.ToLower(project.Clean(p.CommonDir))
	case p.RepoRoot != "":
		return "root:" + strings.ToLower(project.Clean(p.RepoRoot))
	}
	return ""
}

// shorten clips a project name to width runes for the listing column.
func shorten(s string, width int) string {
	if utf8.RuneCountInString(s) <= width {
		return s
	}
	return string([]rune(s)[:width-1]) + "…"
}
