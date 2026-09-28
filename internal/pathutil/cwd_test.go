/**
 * Tests for cwd normalization and equality.
 */
package pathutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEqualExact(t *testing.T) {
	if !Equal("/a/b", "/a/b") {
		t.Fatal("exact")
	}
}

func TestEqualTrailingSlash(t *testing.T) {
	if !Equal("/Users/me/proj", "/Users/me/proj/") {
		t.Fatal("trailing slash")
	}
}

func TestEqualPrivatePrefix(t *testing.T) {
	a := "/var/folders/xy/tmp"
	b := "/private/var/folders/xy/tmp"
	if !Equal(a, b) {
		t.Fatalf("private prefix: %q vs %q (norm %q %q)", a, b, Normalize(a), Normalize(b))
	}
}

func TestEqualSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real-project")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link-project")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlink not supported")
	}
	if !Equal(real, link) {
		t.Fatalf("symlink: real=%q link=%q", Normalize(real), Normalize(link))
	}
}

func TestSameProjectBasename(t *testing.T) {
	if !SameProject("/old/parent/my-app", "/new/parent/my-app") {
		t.Fatal("basename")
	}
	if SameProject("/a/foo", "/b/bar") {
		t.Fatal("different bases")
	}
}

func TestBaseName(t *testing.T) {
	if BaseName("/x/y/z") != "z" {
		t.Fatal(BaseName("/x/y/z"))
	}
	if BaseName("") != "" {
		t.Fatal("empty")
	}
}

func TestSameProjectArchiveRename(t *testing.T) {
	if !SameProject("/Users/x/herdr-usagebar", "/Users/y/herdr-usagebar-ts-archived") {
		t.Fatal("archive rename")
	}
	if SameProject("/Users/x/app", "/Users/y/apple") {
		t.Fatal("false positive")
	}
}

func TestMatcherAgreesWithEqualAndSameProject(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real-project")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link-project")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlink not supported")
	}
	candidates := []string{
		"", real, real + "/", link, "/var/folders/xy/tmp", "/private/var/folders/xy/tmp",
		"/elsewhere/real-project", "/elsewhere/real-project-archived", "/elsewhere/other",
	}
	for _, target := range []string{"", real, link, "/var/folders/xy/tmp", "/elsewhere/real-project"} {
		matcher := NewMatcher(target)
		for _, candidate := range candidates {
			// Ask twice so the memoized path is exercised too.
			for range 2 {
				if got, want := matcher.Equal(candidate), Equal(candidate, target); got != want {
					t.Fatalf("Equal(%q, %q): matcher=%v want %v", candidate, target, got, want)
				}
				if got, want := matcher.SameProject(candidate), SameProject(candidate, target); got != want {
					t.Fatalf("SameProject(%q, %q): matcher=%v want %v", candidate, target, got, want)
				}
			}
		}
	}
}

func TestMatcherResolvesEachPathOnce(t *testing.T) {
	matcher := NewMatcher("/a/target")
	for range 3 {
		for _, candidate := range []string{"/a/one", "/a/two", "/a/one"} {
			matcher.SameProject(candidate)
		}
	}
	if got := len(matcher.normalized); got != 3 {
		t.Fatalf("normalized %d distinct paths, want 3 (target + 2 candidates)", got)
	}
}
