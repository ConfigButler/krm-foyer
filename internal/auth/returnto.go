package auth

import "net/url"

// maxReturnTo bounds the return path stored in a login transaction.
const maxReturnTo = 2048

// localPath reports whether p is safe to send the browser to after login: a path on
// krm-foyer's own origin, and nothing a browser could read as another host.
//
// Browsers are more lenient than url.Parse: they read a backslash as a slash, drop
// tabs and newlines anywhere and trim control characters and spaces at the ends, so
// "/\evil.example" and "/\t/evil.example" both lead off-site. Rather than model
// that, p may hold only printable ASCII other than a backslash, and must not start
// with two slashes.
func localPath(p string) bool {
	if p == "" || len(p) > maxReturnTo || p[0] != '/' || len(p) > 1 && p[1] == '/' {
		return false
	}
	for i := range len(p) {
		if c := p[i]; c <= ' ' || c > '~' || c == '\\' {
			return false
		}
	}
	u, err := url.Parse(p)
	return err == nil && u.Scheme == "" && u.Host == "" && u.User == nil && u.Opaque == ""
}
