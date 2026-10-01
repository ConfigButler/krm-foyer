package proxy

import (
	"net/http"
	"strings"
	"testing"
)

// FuzzCheckResponse looks at what checkResponse lets through the way a browser
// would read it, independently of how checkResponse decides. Whatever upstream
// sends, an approved response carries only allowlisted headers, krm-foyer's own
// security headers, no encoding, and either no body or a single content type
// whose every comma-separated candidate is an allowed media type.
func FuzzCheckResponse(f *testing.F) {
	f.Add(200, "GET", int64(-1), uint8(1), "application/json", "", "", "X-Extra", "1")
	f.Add(200, "GET", int64(12), uint8(2), "application/json", "text/html", "", "", "")
	f.Add(200, "HEAD", int64(12), uint8(0), "", "", "", "", "")
	f.Add(200, "GET", int64(-1), uint8(1), `application/json; a="x,text/html"`, "", "", "", "")
	f.Add(200, "GET", int64(-1), uint8(1), "application/json", "", "gzip", "Set-Cookie", "a=b")
	f.Add(200, "GET", int64(-1), uint8(4), "application/json", "", "br", "", "")
	f.Add(302, "GET", int64(0), uint8(0), "", "", "", "Location", "https://evil.example/")
	f.Add(204, "DELETE", int64(0), uint8(1), "text/html", "", "", "", "")
	f.Fuzz(func(t *testing.T, code int, method string, contentLength int64, fields uint8,
		ct1, ct2, encoding, extraKey, extraValue string) {
		status := 100 + ((code-100)%500+500)%500 // 100-599; seeds keep their code
		header := http.Header{}
		if n := int(fields % 3); n > 0 { // zero, one or two Content-Type fields
			header["Content-Type"] = []string{ct1, ct2}[:n]
		}
		if encoding != "" {
			header["Content-Encoding"] = []string{encoding}
			if fields&4 != 0 { // hidden behind an empty first field
				header["Content-Encoding"] = []string{"", encoding}
			}
		}
		if extraKey != "" {
			header[http.CanonicalHeaderKey(extraKey)] = []string{extraValue}
		}
		resp := &http.Response{
			StatusCode: status, Header: header, ContentLength: contentLength,
			Request: &http.Request{Method: method},
		}
		if checkResponse(resp) != nil {
			return
		}

		if encoding != "" {
			// The body would reach the browser still encoded, with nothing saying so.
			t.Fatalf("Content-Encoding %q approved", encoding)
		}
		if status >= 300 && status < 400 && status != http.StatusNotModified {
			t.Fatalf("a %d redirect was approved", status)
		}
		allowed := map[string]bool{"X-Content-Type-Options": true, "Content-Security-Policy": true,
			"Cache-Control": true, "Referrer-Policy": true}
		for _, k := range responseHeaders {
			allowed[k] = true
		}
		for k := range resp.Header {
			if !allowed[k] {
				t.Fatalf("header %s approved: %q", k, resp.Header[k])
			}
		}
		if len(resp.Trailer) != 0 {
			t.Fatalf("trailers approved: %v", resp.Trailer)
		}
		for k, v := range map[string]string{"X-Content-Type-Options": "nosniff",
			"Content-Security-Policy": "default-src 'none'; sandbox", "Cache-Control": "no-store"} {
			if got := resp.Header.Values(k); len(got) != 1 || got[0] != v {
				t.Fatalf("%s = %q, want %q", k, got, v)
			}
		}

		cts := resp.Header.Values("Content-Type")
		bodyless := method == http.MethodHead || contentLength == 0 ||
			status == http.StatusNoContent || status == http.StatusNotModified
		switch {
		case len(cts) > 1:
			t.Fatalf("Content-Type approved as %d fields: %q", len(cts), cts)
		case len(cts) == 0 || cts[0] == "":
			if !bodyless {
				t.Fatalf("a %s %d with a body and no content type was approved", method, status)
			}
		default:
			// The browser's view: any comma-separated part with a "/" could be the
			// type it picks.
			for _, part := range strings.Split(cts[0], ",") {
				essence := strings.ToLower(strings.TrimSpace(strings.SplitN(part, ";", 2)[0]))
				if strings.Contains(essence, "/") && !contentTypes[essence] {
					t.Fatalf("Content-Type %q approved; a browser may read it as %q", cts[0], essence)
				}
			}
		}
	})
}
