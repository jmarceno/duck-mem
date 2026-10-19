package project

import (
	"net/url"
	"strings"
)

// CanonicalRemote normalizes a git remote URL so the ssh, scp-like and https
// spellings of the same remote compare equal: host is case-folded, the user
// is dropped, a port is ignored, and the trailing slash and .git suffix are
// removed. A local remote (a path or file:// URL) becomes its cleaned path.
func CanonicalRemote(remote string) string {
	r := strings.TrimSpace(remote)
	if r == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(r), "file://") {
		if u, err := url.Parse(r); err == nil {
			return Clean(trimRepo(u.Path))
		}
	}
	// A Windows drive letter is a local path, not an scp-like host:path.
	if len(r) >= 3 && r[1] == ':' && (r[2] == '/' || r[2] == '\\') {
		return Clean(trimRepo(r))
	}
	if strings.Contains(r, "://") {
		u, err := url.Parse(r)
		if err != nil || u.Host == "" {
			return Clean(trimRepo(r))
		}
		return join(hostOf(u), u.Path)
	}
	// scp-like: [user@]host:path (a colon that comes before any slash).
	if i := strings.IndexByte(r, ':'); i > 0 && !strings.Contains(r[:i], "/") {
		host, p := r[:i], r[i+1:]
		if at := strings.LastIndexByte(host, '@'); at >= 0 {
			host = host[at+1:]
		}
		if host != "" {
			return join(host, p)
		}
	}
	return Clean(trimRepo(r))
}

func hostOf(u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return ""
	}
	if port := u.Port(); port != "" && port != "22" && port != "443" && port != "80" {
		host += ":" + port
	}
	return host
}

// join combines a canonical host and path the same way for every spelling:
// no leading or trailing slash, no .git suffix.
func join(host, p string) string {
	p = trimRepo(p)
	p = strings.Trim(p, "/")
	if host == "" {
		return Clean(p)
	}
	if p == "" {
		return strings.ToLower(host)
	}
	return strings.ToLower(host) + "/" + p
}

func trimRepo(p string) string {
	p = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(p), "/"), ".git")
	if strings.HasSuffix(strings.ToLower(p), ".git") {
		p = p[:len(p)-4]
	}
	return p
}
