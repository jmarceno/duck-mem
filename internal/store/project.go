package store

import (
	"context"
	"database/sql"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jmarceno/duck-mem/internal/project"
)

// Project reasons reported by ResolveProjectFilter, in resolution order.
const (
	ProjectReasonExact     = "exact"
	ProjectReasonAlias     = "alias"
	ProjectReasonBasename  = "basename"
	ProjectReasonIdentity  = "identity"
	ProjectReasonSubstring = "substring"
	ProjectReasonNone      = "none"
)

// ProjectFilter restricts reads to a resolved set of project values. The
// zero value means no restriction.
type ProjectFilter struct {
	// Values are stored project values, compared case-insensitively.
	Values []string `json:"values,omitempty"`
	// Substring is the legacy filter used when Values is empty. It is the
	// compatibility fallback, and it can over-match by design.
	Substring string `json:"substring,omitempty"`
	// NoMatch matches nothing: the requested project did not resolve, and an
	// unfiltered search would silently escape the requested scope.
	NoMatch bool `json:"no_match,omitempty"`
}

// Empty reports whether the filter places no restriction.
func (f ProjectFilter) Empty() bool {
	return len(f.Values) == 0 && f.Substring == "" && !f.NoMatch
}

// ProjectStat is one stored project value with the summary the advisory
// output and `duck-mem projects` need.
type ProjectStat struct {
	Project   string    `json:"project"`
	Sessions  int       `json:"sessions"`
	Sources   []string  `json:"sources,omitempty"`
	First     time.Time `json:"first,omitempty"`
	Last      time.Time `json:"last,omitempty"`
	Aliases   []string  `json:"aliases,omitempty"`
	RepoRoot  string    `json:"repo_root,omitempty"`
	RemoteURL string    `json:"remote_url,omitempty"`
	CommonDir string    `json:"common_dir,omitempty"`
}

// ProjectCandidate is one did-you-mean lead. Score is the name similarity;
// the wording tells the reader it is a lead, not a verified identity.
type ProjectCandidate struct {
	ProjectStat
	Score float64 `json:"score"`
}

// ProjectResolution is how a --project argument resolved. Filter is what a
// read should use; the rest is advisory output (did-you-mean candidates,
// ambiguity, stale path, identity collapse).
type ProjectResolution struct {
	Request     string             `json:"request"`
	Filter      ProjectFilter      `json:"filter"`
	Reason      string             `json:"reason"`
	Matched     []ProjectStat      `json:"matched,omitempty"`
	Candidates  []ProjectCandidate `json:"candidates,omitempty"`
	Ambiguous   bool               `json:"ambiguous,omitempty"`
	StalePath   bool               `json:"stale_path,omitempty"`
	IdentityKey string             `json:"identity_key,omitempty"`
}

// ProjectIdentity is the git identity captured for one stored project.
type ProjectIdentity struct {
	Project   string
	RepoRoot  string
	RemoteURL string
	CommonDir string
}

// ProjectStats returns every stored project with session counts, sources and
// date range, plus its aliases and captured git identity.
func (db *DB) ProjectStats(ctx context.Context) ([]ProjectStat, error) {
	defer db.setRead(ctx)()
	stats := map[string]*ProjectStat{}
	add := func(name string) *ProjectStat {
		if st, ok := stats[name]; ok {
			return st
		}
		st := &ProjectStat{Project: name}
		stats[name] = st
		return st
	}
	rows, err := db.query(`SELECT project, count(*), min(started_at), max(started_at)
		FROM sessions GROUP BY project`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var name string
		var n int
		var first, last sql.NullTime
		if err := rows.Scan(&name, &n, &first, &last); err != nil {
			rows.Close()
			return nil, err
		}
		st := add(name)
		st.Sessions = n
		if first.Valid {
			st.First = first.Time
		}
		if last.Valid {
			st.Last = last.Time
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = db.query(`SELECT project, source, count(*) FROM sessions GROUP BY project, source ORDER BY project, source`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var name, source string
		var n int
		if err := rows.Scan(&name, &source, &n); err != nil {
			rows.Close()
			return nil, err
		}
		st := add(name)
		st.Sources = append(st.Sources, source)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	aliases, err := db.projectAliases(ctx)
	if err != nil {
		return nil, err
	}
	byFold := map[string]*ProjectStat{}
	for _, st := range stats {
		byFold[project.Fold(project.Clean(st.Project))] = st
	}
	for old, next := range aliases {
		if st := byFold[project.Fold(project.Clean(next))]; st != nil {
			st.Aliases = append(st.Aliases, old)
		}
	}
	identities, err := db.projectIdentities(ctx)
	if err != nil {
		return nil, err
	}
	for _, id := range identities {
		if st := byFold[project.Fold(project.Clean(id.Project))]; st != nil {
			st.RepoRoot, st.RemoteURL, st.CommonDir = id.RepoRoot, id.RemoteURL, id.CommonDir
		}
	}
	list := make([]ProjectStat, 0, len(stats))
	for _, st := range stats {
		sort.Strings(st.Aliases)
		list = append(list, *st)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Sessions != list[j].Sessions {
			return list[i].Sessions > list[j].Sessions
		}
		return list[i].Project < list[j].Project
	})
	return list, nil
}

// SaveProjectIdentity records (or refreshes) the git identity of a project.
// Ingest calls it best-effort for changed sessions only; no bulk backfill.
func (db *DB) SaveProjectIdentity(name string, id project.GitIdentity) error {
	if name == "" || (id.RepoRoot == "" && id.Remote == "" && id.CommonDir == "") {
		return nil
	}
	_, err := db.sql.Exec(`INSERT INTO project_identities(project, repo_root, remote_url, common_dir, updated_at)
		VALUES(?, ?, ?, ?, now())
		ON CONFLICT(project) DO UPDATE SET repo_root=excluded.repo_root,
		remote_url=excluded.remote_url, common_dir=excluded.common_dir, updated_at=now()`,
		name, id.RepoRoot, id.Remote, id.CommonDir)
	return err
}

// ResolveProjectFilter turns a --project argument into a filter plus the
// advisory messages to print. Resolution order: exact value, renamed alias,
// shared git identity, then the legacy substring fallback. The fuzzy tier
// only produces suggestions; it never selects or merges projects.
func (db *DB) ResolveProjectFilter(ctx context.Context, request string) (ProjectResolution, error) {
	res := ProjectResolution{Request: request, Reason: ProjectReasonNone,
		Filter: ProjectFilter{NoMatch: true}}
	if strings.TrimSpace(request) == "" {
		res.Reason = ProjectReasonExact
		res.Filter = ProjectFilter{}
		return res, nil
	}
	stats, err := db.ProjectStats(ctx)
	if err != nil {
		return res, err
	}
	identities, err := db.projectIdentities(ctx)
	if err != nil {
		return res, err
	}
	aliases, err := db.projectAliases(ctx)
	if err != nil {
		return res, err
	}
	cleanRequest := project.Clean(request)
	byFold := map[string][]ProjectStat{}
	for _, st := range stats {
		key := project.Fold(project.Clean(st.Project))
		byFold[key] = append(byFold[key], st)
	}
	exact := func(name string) []ProjectStat {
		return byFold[project.Fold(project.Clean(name))]
	}
	set := func(matched []ProjectStat, reason string) {
		sortStats(matched)
		res.Reason = reason
		res.Matched = matched
		res.Filter = ProjectFilter{Values: projectValues(matched)}
	}

	matched := exact(request)
	reason := ProjectReasonExact
	if len(matched) == 0 {
		if target, ok := resolveAlias(cleanRequest, aliases); ok {
			matched = exact(target)
			reason = ProjectReasonAlias
		}
	}
	if len(matched) == 0 {
		matched = matchBasename(cleanRequest, stats)
		reason = ProjectReasonBasename
	}
	if len(matched) == 1 {
		if group, key := expandIdentity(matched[0], identities, byFold); len(group) > 1 {
			matched, reason = group, ProjectReasonIdentity
			res.IdentityKey = key
		}
		set(matched, reason)
		return res, nil
	}
	if len(matched) > 1 {
		res.Ambiguous = true
		set(matched, reason)
		return res, nil
	}
	if group, key := identityFromRequest(cleanRequest, identities, byFold); len(group) > 0 {
		res.IdentityKey = key
		set(group, ProjectReasonIdentity)
		return res, nil
	}
	needle := strings.ToLower(strings.TrimSpace(request))
	var sub []ProjectStat
	for _, st := range stats {
		if strings.Contains(strings.ToLower(st.Project), needle) {
			sub = append(sub, st)
		}
	}
	if len(sub) > 0 {
		res.Ambiguous = len(sub) > 1
		set(sub, ProjectReasonSubstring)
		return res, nil
	}
	res.Reason = ProjectReasonNone
	res.Candidates = projectCandidates(cleanRequest, stats, 3)
	if looksLikePath(cleanRequest) {
		if _, err := os.Stat(cleanRequest); err != nil {
			res.StalePath = true
		}
	}
	return res, nil
}

// matchBasename matches a bare directory name against the basename of every
// stored project, which is what an agent usually passes as --project. A full
// path matches exactly earlier; this tier keeps a name from over-matching
// unrelated siblings the way the substring fallback would.
func matchBasename(request string, stats []ProjectStat) []ProjectStat {
	base := project.Fold(project.BaseName(request))
	if base == "" {
		return nil
	}
	var out []ProjectStat
	for _, st := range stats {
		if project.Fold(project.BaseName(st.Project)) == base {
			out = append(out, st)
		}
	}
	return out
}

// expandIdentity replaces one project with every stored project sharing the
// same git identity (worktrees, clones, renames of the same remote).
func expandIdentity(st ProjectStat, identities []ProjectIdentity, byFold map[string][]ProjectStat) ([]ProjectStat, string) {
	id, ok := identityFor(st.Project, identities)
	if !ok {
		return nil, ""
	}
	key := identityKeyOf(id)
	var group []ProjectStat
	seen := map[string]bool{}
	for _, other := range identities {
		if identityKeyOf(other) != key {
			continue
		}
		for _, m := range byFold[project.Fold(project.Clean(other.Project))] {
			fold := project.Fold(m.Project)
			if !seen[fold] {
				seen[fold] = true
				group = append(group, m)
			}
		}
	}
	if len(group) < 2 {
		return nil, ""
	}
	return group, key
}

// identityFromRequest resolves a request that names the repository rather
// than a stored project: a path that is a git checkout now, a captured repo
// root or common dir, or the remote's name (the celeste/celestial case).
func identityFromRequest(request string, identities []ProjectIdentity, byFold map[string][]ProjectStat) ([]ProjectStat, string) {
	// Probe only an explicit path; a bare name is matched against captured
	// identities instead of whatever directory the process runs in.
	if looksLikePath(request) {
		if id, ok := project.ReadGitIdentity(request); ok {
			key := id.IdentityKey()
			if group := groupForIdentity(key, identities, byFold); len(group) > 0 {
				return group, key
			}
		}
	}
	requestFold := project.Fold(project.Clean(request))
	requestBase := project.Fold(project.BaseName(request))
	keys := map[string]bool{}
	for _, id := range identities {
		switch {
		case project.Fold(project.Clean(id.RepoRoot)) == requestFold,
			project.Fold(project.Clean(id.CommonDir)) == requestFold,
			project.Fold(id.RemoteURL) == requestFold,
			requestBase != "" && project.Fold(project.BaseName(id.RemoteURL)) == requestBase:
			keys[identityKeyOf(id)] = true
		}
	}
	if len(keys) != 1 {
		// Zero keys: no identity matched. Several keys: the name is shared by
		// different repositories, so guessing one would blend projects.
		return nil, ""
	}
	var key string
	for k := range keys {
		key = k
	}
	return groupForIdentity(key, identities, byFold), key
}

func groupForIdentity(key string, identities []ProjectIdentity, byFold map[string][]ProjectStat) []ProjectStat {
	var group []ProjectStat
	seen := map[string]bool{}
	for _, id := range identities {
		if identityKeyOf(id) != key {
			continue
		}
		for _, st := range byFold[project.Fold(project.Clean(id.Project))] {
			if fold := project.Fold(st.Project); !seen[fold] {
				seen[fold] = true
				group = append(group, st)
			}
		}
	}
	return group
}

func identityFor(name string, identities []ProjectIdentity) (ProjectIdentity, bool) {
	fold := project.Fold(project.Clean(name))
	for _, id := range identities {
		if project.Fold(project.Clean(id.Project)) == fold {
			return id, true
		}
	}
	return ProjectIdentity{}, false
}

func identityKeyOf(id ProjectIdentity) string {
	switch {
	case id.RemoteURL != "":
		return "remote:" + project.Fold(id.RemoteURL)
	case id.CommonDir != "":
		return "common:" + project.Fold(project.Clean(id.CommonDir))
	case id.RepoRoot != "":
		return "root:" + project.Fold(project.Clean(id.RepoRoot))
	}
	return ""
}

// resolveAlias follows the canonical old-name map. A full old path matches
// exactly; a bare name matches when exactly one alias basename has it.
func resolveAlias(name string, aliases map[string]string) (string, bool) {
	if next, ok := aliases[project.Fold(project.Clean(name))]; ok {
		return followAlias(next, aliases), true
	}
	base := project.Fold(project.BaseName(name))
	if base == "" {
		return "", false
	}
	targets := map[string]string{}
	for old, next := range aliases {
		if project.Fold(project.BaseName(old)) != base {
			continue
		}
		final := followAlias(next, aliases)
		targets[project.Fold(final)] = final
	}
	if len(targets) != 1 {
		return "", false
	}
	for _, target := range targets {
		return target, true
	}
	return "", false
}

func followAlias(name string, aliases map[string]string) string {
	seen := map[string]bool{}
	for i := 0; i < 16; i++ {
		fold := project.Fold(project.Clean(name))
		if seen[fold] {
			return name
		}
		seen[fold] = true
		next, ok := aliases[fold]
		if !ok {
			return name
		}
		name = next
	}
	return name
}

func projectValues(stats []ProjectStat) []string {
	out := make([]string, 0, len(stats))
	seen := map[string]bool{}
	for _, st := range stats {
		if !seen[st.Project] {
			seen[st.Project] = true
			out = append(out, st.Project)
		}
	}
	return out
}

func sortStats(stats []ProjectStat) {
	sort.Slice(stats, func(i, j int) bool {
		if stats[i].Sessions != stats[j].Sessions {
			return stats[i].Sessions > stats[j].Sessions
		}
		return stats[i].Project < stats[j].Project
	})
}

func projectCandidates(request string, stats []ProjectStat, limit int) []ProjectCandidate {
	var out []ProjectCandidate
	for _, st := range stats {
		names := append([]string{st.Project}, st.Aliases...)
		if st.RemoteURL != "" {
			names = append(names, st.RemoteURL)
		}
		score := 0.0
		for _, name := range names {
			if s := project.NameScore(request, name); s > score {
				score = s
			}
		}
		if score >= project.MinNameScore {
			out = append(out, ProjectCandidate{ProjectStat: st, Score: score})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].Sessions != out[j].Sessions {
			return out[i].Sessions > out[j].Sessions
		}
		return out[i].Project < out[j].Project
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// projectAliases loads the canonical old->new map keyed by folded old name.
// A database from before project_aliases existed has no map yet.
func (db *DB) projectAliases(ctx context.Context) (map[string]string, error) {
	defer db.setRead(ctx)()
	rows, err := db.query(`SELECT old_name, new_name FROM project_aliases`)
	if err != nil {
		return nil, missingTable(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var old, next string
		if err := rows.Scan(&old, &next); err != nil {
			return nil, err
		}
		out[project.Fold(project.Clean(old))] = next
	}
	return out, rows.Err()
}

// projectIdentities loads every captured git identity.
func (db *DB) projectIdentities(ctx context.Context) ([]ProjectIdentity, error) {
	defer db.setRead(ctx)()
	rows, err := db.query(`SELECT project, repo_root, remote_url, common_dir FROM project_identities`)
	if err != nil {
		return nil, missingTable(err)
	}
	defer rows.Close()
	var out []ProjectIdentity
	for rows.Next() {
		var id ProjectIdentity
		if err := rows.Scan(&id.Project, &id.RepoRoot, &id.RemoteURL, &id.CommonDir); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// missingTable tolerates reading a database created by an older version
// read-only: the daemon adds new tables when it opens the file for writing.
func missingTable(err error) error {
	if err != nil && strings.Contains(err.Error(), "does not exist") {
		return nil
	}
	return err
}

// projectSQL renders a project filter for the graph queries, whose project
// column cannot take a bound placeholder. Values are SQL-quoted.
func projectSQL(alias string, filter ProjectFilter) string {
	if filter.NoMatch {
		return " AND FALSE"
	}
	if len(filter.Values) > 0 {
		parts := make([]string, len(filter.Values))
		for i, v := range filter.Values {
			parts[i] = "lower(" + sqlString(v) + ")"
		}
		return " AND lower(" + alias + ".project) IN (" + strings.Join(parts, ",") + ")"
	}
	if filter.Substring != "" {
		return ` AND contains(lower(` + alias + `.project), lower(` + sqlString(filter.Substring) + `))`
	}
	return ""
}

// looksLikePath reports whether a request reads as a filesystem path rather
// than a bare project name.
func looksLikePath(p string) bool {
	if p == "" {
		return false
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, "/") {
		return true
	}
	return len(p) >= 3 && p[1] == ':' && p[2] == '/'
}
