package auth

import (
	"net/url"
	"strings"
	"testing"
)

// browserResolve approximates how a browser resolves a Location header against the
// page it is on, following the WHATWG URL standard's leniencies: leading and
// trailing C0 controls and spaces are trimmed, tabs and newlines anywhere are
// removed, a backslash counts as a slash in http(s) URLs, and a reference starting
// with two or more slashes names a host after skipping all of them ("///evil" is
// evil, where url.Parse sees a path). It is written from the standard, not from
// localPath.
func browserResolve(base *url.URL, location string) (*url.URL, bool) {
	location = strings.TrimFunc(location, func(r rune) bool { return r <= ' ' })
	location = strings.Map(func(r rune) rune {
		switch r {
		case '\t', '\n', '\r':
			return -1
		case '\\':
			return '/'
		}
		return r
	}, location)
	if strings.HasPrefix(location, "//") {
		location = "//" + strings.TrimLeft(location, "/")
	}
	ref, err := url.Parse(location)
	if err != nil {
		return nil, false
	}
	return base.ResolveReference(ref), true
}

// FuzzLocalPath: whatever localPath accepts, a browser sent there stays on
// krm-foyer's origin, and the path is used exactly as given.
func FuzzLocalPath(f *testing.F) {
	for _, seed := range []string{
		"/", "/a/b?c#d", "//evil.example", "/\\evil.example", "\\/evil.example", "///evil.example", "//", "/\\/evil.example", "/\t/evil.example",
		"https://evil.example", "/%2F%2Fevil.example", "/..//evil.example", " //evil.example",
		"/@evil.example", "/:@evil.example", "javascript:x", "/a/../../b", "/\x00/evil",
	} {
		f.Add(seed)
	}
	base, err := url.Parse("https://foyer.example.test/apps/editor/page")
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, p string) {
		if !localPath(p) {
			return
		}
		got, ok := browserResolve(base, p)
		if !ok {
			t.Fatalf("accepted %q, which a browser could not parse", p)
		}
		if got.Scheme != "https" || got.Host != base.Host || got.User != nil {
			t.Fatalf("accepted %q, which a browser resolves to %s", p, got)
		}
		if !strings.HasPrefix(p, "/") {
			t.Fatalf("accepted %q, which is relative to the current page", p)
		}
	})
}
