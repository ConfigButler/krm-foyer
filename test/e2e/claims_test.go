//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	return signInWith(ctx, fx.browser(), email)
}

// signInBrief signs in to the brief krm-foyer, whose sessions end within a minute.
func signInBrief(ctx context.Context, email string) user {
	return signInWith(ctx, fx.briefBrowser(), email)
}

func signInWith(ctx context.Context, b *browser, email string) user {
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
// the user's session, and expects the same answer: status, content type and body. If
// krm-foyer made any decision of its own, or changed what it passed on, they would
// differ. A JSON body is compared as JSON values (a watch is a stream of them), with
// what the API server generates afresh for each request set aside; see generated.
func (u user) compare(ctx context.Context, method, path string, body []byte, header http.Header) (direct, via answer) {
	GinkgoHelper()
	direct = fx.directWith(ctx, u.token, method, path, body, header)
	via = u.viaFoyer(ctx, method, path, body, header)
	Expect(via.Code).To(Equal(direct.Code), "%s %s\ndirect: %s\nvia krm-foyer: %s", method, path, direct.Body, via.Body)
	Expect(via.Header.Get("Content-Type")).To(Equal(direct.Header.Get("Content-Type")), "%s %s", method, path)
	if direct.Header.Get("Content-Type") != "application/json" {
		Expect(via.Body).To(Equal(direct.Body), "%s %s: krm-foyer's body differs from the API server's", method, path)
		return direct, via
	}
	write := method != http.MethodGet && method != http.MethodHead
	d, v := jsonValues(direct.Body), jsonValues(via.Body)
	Expect(d).NotTo(BeEmpty(), "%s %s: the API server sent no body to compare", method, path)
	for _, x := range append(slices.Clone(d), v...) {
		generated(x, write)
	}
	Expect(v).To(Equal(d), "%s %s: krm-foyer's body differs from the API server's\ndirect: %s\nvia krm-foyer: %s",
		method, path, direct.Body, via.Body)
	return direct, via
}

// jsonValues decodes every JSON value in body, in order.
func jsonValues(body []byte) []any {
	GinkgoHelper()
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var values []any
	for {
		var v any
		err := dec.Decode(&v)
		if errors.Is(err, io.EOF) {
			return values
		}
		Expect(err).NotTo(HaveOccurred(), "%s", body)
		values = append(values, v)
	}
}

// generated replaces, in a decoded body, the fields the API server fills in afresh
// for each request, so two answers to the same request can be compared. They are
// replaced rather than removed: a field krm-foyer dropped still shows as a difference.
//
//   - A list read has the store's current resourceVersion, which any write anywhere in
//     the cluster moves; leases renew every few seconds.
//   - A write creates a new object, or acts on one, at a new moment: uid,
//     creationTimestamp, resourceVersion and each managedFields time are new, and so is
//     the name when the request asked for a generateName.
//
// Nothing else is set aside: every other field, in every item of a list and every
// event of a watch, must be equal.
func generated(v any, write bool) {
	obj, _ := v.(map[string]any)
	meta, _ := obj["metadata"].(map[string]any)
	if meta == nil {
		return
	}
	set := func(field string) {
		if _, ok := meta[field]; ok {
			meta[field] = "<generated>"
		}
	}
	if kind, _ := obj["kind"].(string); strings.HasSuffix(kind, "List") {
		set("resourceVersion")
		return
	}
	if !write {
		return
	}
	set("uid")
	set("creationTimestamp")
	set("resourceVersion")
	if _, ok := meta["generateName"]; ok {
		set("name")
	}
	managed, _ := meta["managedFields"].([]any)
	for _, m := range managed {
		if entry, ok := m.(map[string]any); ok {
			if _, ok := entry["time"]; ok {
				entry["time"] = "<generated>"
			}
		}
	}
}

func configMap(name string) []byte {
	return fmt.Appendf(nil, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":%q},"data":{"k":"v"}}`, name)
}

// credentialLeaks scans every response krm-foyer sent the suite, and everything
// krm-foyer logged, for credentials: anything shaped like a JWT (which covers the ID
// tokens krm-foyer holds and never showed the suite, and its own service-account
// token), every token the suite obtained, the client secrets, and every session cookie
// krm-foyer issued. A session cookie may appear only as the value of its own
// Set-Cookie, and even there it is scanned, as is what it decodes to: it holds the
// user's token sealed, never in the clear.
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
	credentials := func(where, text string) {
		if m := jwt.FindString(text); m != "" {
			leaks = append(leaks, fmt.Sprintf("%s: something shaped like a JWT (%.24s...)", where, m))
		}
		for s, what := range secrets {
			if strings.Contains(text, s) {
				leaks = append(leaks, where+": "+what)
			}
		}
	}
	check := func(where, text string) {
		credentials(where, text)
		for _, id := range sessionIDs {
			if strings.Contains(text, id) {
				leaks = append(leaks, where+": a session cookie")
			}
		}
	}
	for _, r := range seen {
		check("the body of "+r.target, string(r.body))
		for k, vs := range r.header {
			for _, v := range vs {
				if k == "Set-Cookie" {
					// The one place a session cookie belongs: as the value of the
					// cookie that issues it. Still no credential in it, sealed or not.
					if rest, ok := strings.CutPrefix(v, sessionCookie+"="); ok {
						var value string
						value, v, _ = strings.Cut(rest, ";")
						credentials("the session cookie set by "+r.target, value)
						raw, err := base64.RawURLEncoding.DecodeString(value)
						if value != "" && err != nil {
							leaks = append(leaks, "the session cookie set by "+r.target+" is not base64url")
						}
						credentials("the session cookie set by "+r.target+", decoded", string(raw))
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
