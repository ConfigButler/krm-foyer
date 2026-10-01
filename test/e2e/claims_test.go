//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// user is someone signed in to krm-foyer, who also holds their own Dex token for
// asking the API server directly.
type user struct {
	name    string
	b       *browser
	csrf    string
	token   string
	k8sName string
}

func signIn(ctx context.Context, email string) user {
	b := fx.browser()
	csrf := b.signedIn(ctx, email)
	return user{name: email, b: b, csrf: csrf, token: fx.login(ctx, email, foyerClient, foyerSecret), k8sName: "oidc:" + email}
}

// proof is what the user's own page sends with a mutation.
func (u user) proof() http.Header {
	return http.Header{"Origin": {fx.foyerURL}, "X-Csrf-Token": {u.csrf}}
}

// viaFoyer sends a request through krm-foyer with the user's session, with CSRF proof
// unless it is a read.
func (u user) viaFoyer(ctx context.Context, method, path string, body []byte, header http.Header) answer {
	h := http.Header{}
	if method != http.MethodGet && method != http.MethodHead {
		h = u.proof()
	}
	for k, v := range header {
		h[k] = v
	}
	return u.b.do(ctx, method, "/k8s"+path, body, h)
}

// compare asks the API server directly with the user's own token, then krm-foyer with
// the user's session, and expects the same answer: status, content type and, for a
// Status, the whole body. If krm-foyer made any decision of its own, they would differ.
func (u user) compare(ctx context.Context, method, path string, body []byte, header http.Header) (direct, via answer) {
	GinkgoHelper()
	direct = fx.directWith(ctx, u.token, method, path, body, header)
	via = u.viaFoyer(ctx, method, path, body, header)
	Expect(via.Code).To(Equal(direct.Code), "%s %s\ndirect: %s\nvia krm-foyer: %s", method, path, direct.Body, via.Body)
	Expect(via.Header.Get("Content-Type")).To(Equal(direct.Header.Get("Content-Type")), "%s %s", method, path)
	if direct.status().Kind == "Status" {
		var d, v map[string]any
		Expect(json.Unmarshal(direct.Body, &d)).To(Succeed())
		Expect(json.Unmarshal(via.Body, &v)).To(Succeed(), "%s", via.Body)
		Expect(v).To(Equal(d), "%s %s: krm-foyer's Status differs from the API server's", method, path)
	}
	return direct, via
}

func configMap(name string) []byte {
	return fmt.Appendf(nil, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":%q},"data":{"k":"v"}}`, name)
}

// credentialLeaks scans every response krm-foyer sent the suite, and everything
// krm-foyer logged, for credentials: anything shaped like a JWT (which covers the ID
// tokens krm-foyer holds and never showed the suite, and its own service-account
// token), every token the suite obtained, the client secrets, and every session ID
// krm-foyer issued. A session ID may appear only as the value of its own Set-Cookie.
func (f *fixture) credentialLeaks() (leaks []string, responses int) {
	f.seenMu.Lock()
	seen := slices.Clone(f.seen)
	tokens := slices.Clone(f.tokens)
	f.seenMu.Unlock()

	secrets := map[string]string{foyerSecret: "the client secret", otherSecret: "another client's secret"}
	for _, t := range tokens {
		secrets[t] = "a token the suite obtained"
		if i := strings.LastIndexByte(t, '.'); i > 0 {
			secrets[t[i+1:]] = "a token's signature"
		}
	}
	var sessionIDs []string
	for _, r := range seen {
		for _, c := range cookiesSet(answer{Header: r.header}) {
			if c.Name == sessionCookie && c.Value != "" {
				sessionIDs = append(sessionIDs, c.Value)
			}
		}
	}
	check := func(where, text string) {
		if m := jwt.FindString(text); m != "" {
			leaks = append(leaks, fmt.Sprintf("%s: something shaped like a JWT (%.24s...)", where, m))
		}
		for s, what := range secrets {
			if strings.Contains(text, s) {
				leaks = append(leaks, where+": "+what)
			}
		}
		for _, id := range sessionIDs {
			if strings.Contains(text, id) {
				leaks = append(leaks, where+": a session ID")
			}
		}
	}
	for _, r := range seen {
		check("the body of "+r.target, string(r.body))
		for k, vs := range r.header {
			for _, v := range vs {
				if k == "Set-Cookie" {
					// The one place a session ID belongs: as the value of the cookie
					// that issues it.
					if rest, ok := strings.CutPrefix(v, sessionCookie+"="); ok {
						_, v, _ = strings.Cut(rest, ";")
					}
				}
				check(k+" of "+r.target, k+": "+v)
			}
		}
	}
	check("krm-foyer's log", fx.kubectl("-n", f.foyerNamespace, "logs", "deployment/krm-foyer", "--tail=-1"))
	return leaks, len(seen)
}

// jwt matches a JWT: two base64url JSON segments and a signature.
var jwt = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]+`)
