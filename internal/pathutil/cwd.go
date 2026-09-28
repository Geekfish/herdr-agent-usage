/**
 * Cwd comparison helpers for session resolution.
 *
 * Agents and Herdr often disagree on the exact string for the same directory
 * (trailing slash, Absolute vs relative, symlink, macOS /private prefix).
 * Exact string equality then fails even though the user never changed projects.
 */
package pathutil

import (
	"path/filepath"
	"strings"
)

// Normalize cleans and, when possible, absolutizes and resolves symlinks.
// If the path does not exist, EvalSymlinks fails and Clean/Abs results are kept.
func Normalize(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = filepath.Clean(p)
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	return p
}

// Equal reports whether a and b refer to the same directory for session matching.
func Equal(a, b string) bool {
	return equalWith(a, b, Normalize)
}

func equalWith(a, b string, normalize func(string) string) bool {
	if a == b {
		return true
	}
	if a == "" || b == "" {
		return false
	}
	na, nb := normalize(a), normalize(b)
	if na == nb {
		return true
	}
	// macOS often records /var/... while tools see /private/var/...
	na, nb = stripPrivatePrefix(na), stripPrivatePrefix(nb)
	return na == nb && na != ""
}

// Matcher compares many candidate paths against one target, resolving each
// distinct path at most once. Session scans compare thousands of rollouts
// that share a handful of cwds; Normalize walks symlinks with one lstat per
// path component, so uncached comparisons dominate a scan.
type Matcher struct {
	target     string
	normalized map[string]string
}

// NewMatcher returns a Matcher for target. It is not safe for concurrent use.
func NewMatcher(target string) *Matcher {
	return &Matcher{target: target, normalized: map[string]string{}}
}

func (m *Matcher) normalize(p string) string {
	if n, ok := m.normalized[p]; ok {
		return n
	}
	n := Normalize(p)
	m.normalized[p] = n
	return n
}

// Equal is Equal(p, target).
func (m *Matcher) Equal(p string) bool {
	return equalWith(p, m.target, m.normalize)
}

// SameProject is SameProject(p, target).
func (m *Matcher) SameProject(p string) bool {
	return m.Equal(p) || sameBaseName(p, m.target)
}

func stripPrivatePrefix(p string) string {
	const priv = "/private"
	if strings.HasPrefix(p, priv+"/") {
		return p[len(priv):]
	}
	return p
}

// BaseName is the last path element after Clean ("" for empty / root-like paths).
func BaseName(p string) string {
	p = filepath.Clean(strings.TrimSpace(p))
	if p == "" || p == "." || p == string(filepath.Separator) {
		return ""
	}
	base := filepath.Base(p)
	if base == "." || base == string(filepath.Separator) {
		return ""
	}
	return base
}

// SameProject reports a weaker match used only as a last-resort fallback:
// same directory base name (e.g. after a folder rename that kept the leaf name
// intent, or when parent path strings differ). Also treats archive-style
// renames as the same project: `my-app` ↔ `my-app-archived` / `my-app_old`.
// Callers must still rank by recency when multiple projects share a basename.
func SameProject(a, b string) bool {
	return Equal(a, b) || sameBaseName(a, b)
}

func sameBaseName(a, b string) bool {
	ba, bb := BaseName(a), BaseName(b)
	if ba == "" || bb == "" {
		return false
	}
	if ba == bb {
		return true
	}
	// Common renames: keep stem, append -suffix or _suffix.
	return relatedBaseName(ba, bb)
}

func relatedBaseName(a, b string) bool {
	// Prefer longer as the "renamed" side.
	if len(a) < len(b) {
		a, b = b, a
	}
	// a is longer; b is stem candidate.
	if !strings.HasPrefix(a, b) || len(a) <= len(b) {
		return false
	}
	sep := a[len(b)]
	return sep == '-' || sep == '_'
}
