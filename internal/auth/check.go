package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"

	"github.com/ConfigButler/krm-foyer/internal/interruption"
	"github.com/ConfigButler/krm-foyer/internal/session"
)

// IdentityHeader carries /auth/check's identity, when the ingress asks for it: the
// base64url encoding (unpadded) of a JSON CheckIdentity. One field, so an ingress
// copies it whole, and a backend parses one value, never a choice between several.
const IdentityHeader = "Krm-Foyer-Identity"

// The headers an ingress sends /auth/check to say which request it is checking.
// Traefik's ForwardAuth sets both; nginx's auth_request needs them set in the recipe.
const (
	forwardedMethod = "X-Forwarded-Method"
	forwardedURI    = "X-Forwarded-Uri"
	originalURI     = "X-Original-Uri"
)

// CheckIdentity is who /auth/check says the user is, for a domain backend on the same
// origin: the API server's own answer to a SelfSubjectReview with the user's token,
// and the session's display name and connector as /auth/session shows them. It never
// holds a token.
type CheckIdentity struct {
	UserInfo    authenticationv1.UserInfo `json:"userInfo"`
	DisplayName string                    `json:"displayName"`
	Connector   string                    `json:"connector,omitempty"`
	Issuer      string                    `json:"issuer"`
	ExpiresAt   time.Time                 `json:"expiresAt"`
}

// Identify answers who Kubernetes takes the user of r to be. It is given the request
// as the ingress forwarded it (the method the browser used, the page it asked for),
// and answers r itself when it cannot say, returning false: the session refused, a
// bound reached, the API server's refusal, or a failure.
type Identify func(w http.ResponseWriter, r *http.Request) (authenticationv1.UserInfo, bool)

// Check serves GET /auth/check for an ingress: is the browser behind this request
// signed in, and may it make it? See docs/ingress.md, "The check".
//
//   - 204 when it may. With ?identity=true, the answer carries IdentityHeader, which
//     identify fills.
//   - The refusal /k8s would give otherwise: 401 with no session, 403 for a mutation
//     without CSRF proof or from another origin, as a Status for code and a page for a
//     person.
//   - With ?redirect=true, a page load with no session is a 302 to the login instead,
//     at the public URL, which returns to the page the ingress names.
//
// The request checked is the one the ingress forwards: its method in
// X-Forwarded-Method, and its page in X-Forwarded-Uri or X-Original-Uri. Neither
// grants anything a browser could not get by asking for itself, so neither is trusted
// for more: a page that is not a local path becomes /.
func (a *Auth) Check(identify Identify) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		q, err := url.ParseQuery(r.URL.RawQuery)
		redirect, okRedirect := flag(q, "redirect")
		withIdentity, okIdentity := flag(q, "identity")
		unknown := false
		for key := range q {
			unknown = unknown || key != "redirect" && key != "identity"
		}
		if err != nil || !okRedirect || !okIdentity || unknown {
			badCheck.Write(w)
			return
		}
		checked, ok := forwarded(r)
		if !ok {
			badCheck.Write(w)
			return
		}
		s, err := a.cfg.Sessions.Use(checked)
		if err != nil {
			if redirect && isPageLoad(checked) && errors.Is(err, session.ErrNoSession) {
				// Absolute, on the configured public origin: an ingress resolves a
				// relative Location against the check's own address (Traefik does), which
				// is krm-foyer's Service, not the browser's origin. Not http.Redirect: it
				// would clean the path. localPath allows nothing that could end the header.
				w.Header().Set("Location", strings.TrimSuffix(a.redirectURI, "/auth/callback")+
					"/auth/login?"+url.Values{"return_to": {checked.RequestURI}}.Encode())
				w.WriteHeader(http.StatusFound)
				return
			}
			refused := a.refusal(err)
			a.logger.Info("refused", "by", "krm-foyer", "status", refused.Status, "reason", refused.Reason,
				"method", checked.Method, "route", "/auth/check")
			refused.Serve(w, checked)
			return
		}
		if !withIdentity {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		info, ok := identify(w, checked)
		if !ok {
			return
		}
		var id identity
		claims, err := tokenClaims(s.IDToken)
		if err == nil {
			id, err = a.claims.extract(claims)
		}
		if err != nil {
			// sessionClaims changed since this login: the claims show nothing rather
			// than a guess, as in /auth/session. Who Kubernetes says the user is stands.
			a.logger.Warn("a session's claims do not match sessionClaims")
			id = identity{}
		}
		body, err := json.Marshal(CheckIdentity{
			UserInfo: info, DisplayName: id.DisplayName, Connector: id.Connector,
			Issuer: s.Issuer, ExpiresAt: s.Expires,
		})
		if err != nil {
			panic(err) // strings, lists and a time: cannot fail
		}
		w.Header().Set(IdentityHeader, base64.RawURLEncoding.EncodeToString(body))
		w.WriteHeader(http.StatusNoContent)
	})
}

// badCheck answers a check the ingress asked wrongly: an unknown option, or a
// forwarded method that is not one.
var badCheck = &interruption.Interruption{
	Status: http.StatusBadRequest, Reason: "BadRequest",
	Message: "/auth/check takes only redirect=true and identity=true, and one X-Forwarded-Method",
}

// flag reads an option of /auth/check: absent, or exactly once as true.
func flag(q url.Values, key string) (on, ok bool) {
	v, given := q[key]
	if !given {
		return false, true
	}
	return true, len(v) == 1 && v[0] == "true"
}

// forwarded is the request the ingress is checking, as krm-foyer checks its own: the
// browser's method, the page it asked for, and the browser's own headers, Cookie,
// Origin and the CSRF proof among them. With no X-Forwarded-Method, the check is of
// a GET. More than one, or one that is not a method, is no request at all.
func forwarded(r *http.Request) (*http.Request, bool) {
	checked := r.Clone(r.Context())
	if methods, given := r.Header[forwardedMethod]; given {
		if len(methods) != 1 || !isToken(methods[0]) {
			return nil, false
		}
		checked.Method = methods[0]
	} else {
		checked.Method = http.MethodGet
	}
	checked.RequestURI = "/"
	for _, name := range []string{forwardedURI, originalURI} {
		if v := r.Header.Values(name); len(v) == 1 && localPath(v[0]) {
			checked.RequestURI = v[0]
			break
		}
	}
	return checked, true
}

// isPageLoad reports whether the browser is loading a page: a GET or HEAD that is not
// a fetch from a script, as far as the browser says. Only a page load is sent to the
// login; a script gets the 401 it can act on.
func isPageLoad(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	mode := r.Header.Values("Sec-Fetch-Mode")
	return len(mode) == 0 || len(mode) == 1 && mode[0] == "navigate"
}

// isToken reports whether s is an HTTP method: a token of RFC 9110.
func isToken(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if c <= ' ' || c > '~' || c == '"' || c == '(' || c == ')' || c == ',' || c == '/' || c == ':' ||
			c == ';' || c == '<' || c == '=' || c == '>' || c == '?' || c == '@' || c == '[' || c == '\\' ||
			c == ']' || c == '{' || c == '}' {
			return false
		}
	}
	return true
}
