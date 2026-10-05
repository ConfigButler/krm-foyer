package auth

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// dexLike and otherIssuer configure the same need, a choice of connector and a hint,
// under two issuers' own names. The same code forwards both: krm-foyer knows neither.
var (
	dexLike = LoginConfig{AuthorizationParameters: map[string]Parameter{
		"connector_id": {Default: "audience", AllowFromRequest: true, AllowedValues: []string{"audience", "operator"}},
		"login_hint":   {AllowFromRequest: true},
	}}
	otherIssuer = LoginConfig{AuthorizationParameters: map[string]Parameter{
		"kc_idp_hint": {AllowFromRequest: true},
		"prompt":      {Default: "login"},
	}}
)

// loginWith opens /auth/login with this raw query and returns the answer.
func (b *browser) loginWith(rawQuery string) response {
	b.h.t.Helper()
	return b.get("/auth/login?" + rawQuery)
}

// authorized is the query of the authorization request the issuer last saw.
func (h *harness) authorized() url.Values {
	h.t.Helper()
	h.issuer.mu.Lock()
	defer h.issuer.mu.Unlock()
	if len(h.issuer.authorized) == 0 {
		h.t.Fatal("no authorization request reached the issuer")
	}
	return h.issuer.authorized[len(h.issuer.authorized)-1]
}

// extra is what an authorization request carries besides krm-foyer's own parameters.
func extra(q url.Values) url.Values {
	out := url.Values{}
	for k, v := range q {
		switch k {
		case "client_id", "redirect_uri", "response_type", "scope", "state", "nonce", "code_challenge", "code_challenge_method":
		default:
			out[k] = v
		}
	}
	return out
}

// A login link carries what the application chose under oidc.<name>; krm-foyer sends
// it to the issuer under <name>, with the configured defaults, decoded once and
// otherwise unchanged, and the login completes as usual. Two issuers' configurations
// with different names go through the same code.
func TestParametersReachTheIssuer(t *testing.T) {
	// Spaces, a plus, a slash, an ampersand, an equals sign, a percent sign and
	// non-ASCII, which survive only if the value is encoded exactly once.
	const hint = "Room 7/a+b & c=d %41 é"
	for name, tc := range map[string]struct {
		login LoginConfig
		query url.Values
		want  url.Values
	}{
		"Dex-like, defaults only": {dexLike, url.Values{}, url.Values{"connector_id": {"audience"}}},
		"Dex-like, chosen": {dexLike,
			url.Values{"oidc.connector_id": {"operator"}, "oidc.login_hint": {hint}},
			url.Values{"connector_id": {"operator"}, "login_hint": {hint}}},
		"another issuer's names": {otherIssuer,
			url.Values{"oidc.kc_idp_hint": {hint}},
			url.Values{"kc_idp_hint": {hint}, "prompt": {"login"}}},
		"nothing configured": {LoginConfig{}, url.Values{}, url.Values{}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarnessWith(t, tc.login)
			b := h.browser()
			tc.query.Set("return_to", "/after?x=1")
			resp := b.loginWith(tc.query.Encode())
			if resp.code != http.StatusFound {
				t.Fatalf("login answered %d %s", resp.code, resp.body)
			}
			callback := h.issuer.authorize(resp.header.Get("Location"), alice)
			if got := extra(h.authorized()); !equalValues(got, tc.want) {
				t.Errorf("the issuer received %q besides krm-foyer's own, want %q", got, tc.want)
			}
			if done := b.get(callback); done.code != http.StatusSeeOther || done.header.Get("Location") != "/after?x=1" {
				t.Fatalf("callback answered %d to %q", done.code, done.header.Get("Location"))
			}
		})
	}
}

func equalValues(a, b url.Values) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !slices.Equal(v, b[k]) {
			return false
		}
	}
	return true
}

// A login link cannot reach past the configuration: an unknown name, a value the
// configuration does not allow, a configuration-only parameter, a repeated or empty
// value, one too long, too many bytes in all, a malformed query, and the authorization
// request's own parameters all get the same 400 before any login starts. The value is
// never shown or logged.
func TestParametersRefused(t *testing.T) {
	const marker = "opaque-marker-5a1f"
	long := strings.Repeat("x", maxParameterValue+1)
	config := LoginConfig{AuthorizationParameters: map[string]Parameter{
		"connector_id": {Default: "audience", AllowFromRequest: true, AllowedValues: []string{"audience", "operator"}},
		"login_hint":   {AllowFromRequest: true},
		"prompt":       {Default: "login"},
		"a":            {AllowFromRequest: true},
		"b":            {AllowFromRequest: true},
		"c":            {AllowFromRequest: true},
	}}
	h := newHarnessWith(t, config)
	for name, query := range map[string]string{
		"an unknown name":          "oidc.acr_values=" + marker,
		"a value not allowed":      "oidc.connector_id=" + marker,
		"an allowed value, folded": "oidc.connector_id=Operator",
		"a configuration-only one": "oidc.prompt=" + marker,
		"repeated":                 "oidc.login_hint=" + marker + "&oidc.login_hint=" + marker,
		"repeated, the same value": "oidc.connector_id=operator&oidc.connector_id=operator",
		"empty":                    "oidc.login_hint=",
		"too long":                 "oidc.login_hint=" + long,
		"too much in all":          "oidc.a=" + long[:500] + "&oidc.b=" + long[:500] + "&oidc.c=" + long[:100],
		"a malformed query":        "oidc.login_hint=" + marker + ";x=y",
		"a malformed escape":       "oidc.login_hint=%zz" + marker,
		"state":                    "oidc.state=" + marker,
		"nonce":                    "oidc.nonce=" + marker,
		"client_id":                "oidc.client_id=" + marker,
		"redirect_uri":             "oidc.redirect_uri=https%3A%2F%2Fevil.example%2F" + marker,
		"code_challenge":           "oidc.code_challenge=" + marker,
		"code_challenge_method":    "oidc.code_challenge_method=plain",
		"scope":                    "oidc.scope=openid+offline_access",
		"response_type":            "oidc.response_type=token",
		"response_mode":            "oidc.response_mode=fragment",
		"request":                  "oidc.request=" + marker,
		"request_uri":              "oidc.request_uri=https%3A%2F%2Fevil.example%2F" + marker,
		"client_secret":            "oidc.client_secret=" + marker,
		"the prefix alone":         "oidc.=" + marker,
		"return_to for the issuer": "oidc.return_to=%2F" + marker,
		"a good one beside a bad":  "oidc.connector_id=operator&oidc.acr_values=" + marker,
	} {
		t.Run(name, func(t *testing.T) {
			b := h.browser()
			resp := b.loginWith(query)
			assertLoginError(t, resp, http.StatusBadRequest, "login-parameter-refused")
			if len(b.loginCookies()) != 0 {
				t.Error("a login was started anyway")
			}
			if strings.Contains(resp.body, marker) || strings.Contains(resp.body, "Operator") {
				t.Error("the error page repeats the value")
			}
		})
	}
	h.issuer.mu.Lock()
	n := len(h.issuer.authorized)
	h.issuer.mu.Unlock()
	if n != 0 {
		t.Errorf("%d refused logins reached the issuer", n)
	}
	if strings.Contains(h.logs.String(), marker) {
		t.Errorf("a refused value was logged:\n%s", h.logs.String())
	}
}

// The configuration cannot replace the authorization request's own parameters or
// carry a credential either, nor configure something that could never be sent or a
// default its own list does not allow. New refuses it, so krm-foyer does not start.
func TestParametersConfigRefused(t *testing.T) {
	refused := map[string]map[string]Parameter{}
	for _, name := range reservedParameters {
		refused["reserved "+name] = map[string]Parameter{name: {AllowFromRequest: true}}
	}
	refused["reserved, other case"] = map[string]Parameter{"State": {AllowFromRequest: true}}
	refused["a name with a space"] = map[string]Parameter{"login hint": {AllowFromRequest: true}}
	refused["a name with ="] = map[string]Parameter{"a=b": {AllowFromRequest: true}}
	refused["an empty name"] = map[string]Parameter{"": {AllowFromRequest: true}}
	refused["never sent"] = map[string]Parameter{"prompt": {}}
	refused["a default not allowed"] = map[string]Parameter{"connector_id": {Default: "x", AllowedValues: []string{"y"}}}
	refused["a repeated allowed value"] = map[string]Parameter{"c": {AllowFromRequest: true, AllowedValues: []string{"y", "y"}}}
	refused["an empty allowed value"] = map[string]Parameter{"c": {AllowFromRequest: true, AllowedValues: []string{""}}}
	refused["a long default"] = map[string]Parameter{"c": {Default: strings.Repeat("d", maxParameterValue+1)}}
	many := map[string]Parameter{}
	for i := range maxParameters + 1 {
		many[strings.Repeat("p", i+1)] = Parameter{AllowFromRequest: true}
	}
	refused["too many"] = many
	for name, params := range refused {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			cfg := Config{
				PublicURL: h.foyer.URL, Issuer: h.issuer.URL, ClientID: clientID, ClientSecret: clientSecret,
				Sessions: h.sessions, Login: LoginConfig{AuthorizationParameters: params},
			}
			if _, err := New(cfg); err == nil {
				t.Fatal("New accepted it")
			}
		})
	}
}

// A refused login offers to try again with the same choice of connector, a value from
// the configured list, and never with the login hint, which is opaque: the
// application gives a fresh link for that.
func TestRetryKeepsOnlyListedChoices(t *testing.T) {
	const hint = "hint-only-for-the-issuer-81c2"
	h := newHarnessWith(t, dexLike)
	b := h.browser()
	resp := b.loginWith(url.Values{"return_to": {"/room"}, "oidc.connector_id": {"operator"}, "oidc.login_hint": {hint}}.Encode())
	if resp.code != http.StatusFound {
		t.Fatalf("login answered %d", resp.code)
	}
	h.issuer.authorize(resp.header.Get("Location"), alice)
	q := h.authorized()
	refusal := q.Get("redirect_uri") + "?" + url.Values{"error": {"access_denied"}, "state": {q.Get("state")}}.Encode()
	failed := b.get(refusal)
	assertLoginError(t, failed, http.StatusBadRequest, "issuer-refused")
	want := `href="/auth/login?oidc.connector_id=operator&amp;return_to=%2Froom"`
	if !strings.Contains(failed.body, want) {
		t.Errorf("the retry link is not %s:\n%s", want, failed.body)
	}
	if strings.Contains(failed.body, hint) || strings.Contains(h.logs.String(), hint) {
		t.Error("the opaque hint was repeated on the error page or logged")
	}
}

// /auth/session shows the display name, groups and, when configured, the connector,
// from the verified ID token alone. Missing claims are empty, in the same shape; a
// claim of the wrong type fails the login. The connector the login link asked for is
// never what the session says: only the token's own claim is.
func TestSessionClaims(t *testing.T) {
	withConnector := dexLike
	withConnector.SessionClaims = SessionClaims{Connector: "/federated_claims/connector_id"}
	for name, tc := range map[string]struct {
		login  LoginConfig
		claims map[string]any
		ask    string
		want   string // the session's JSON from "displayName" on, before "expiresAt"
		reason string // the login error, if the login fails
	}{
		"defaults": {LoginConfig{}, map[string]any{"name": "Alice Example", "groups": []any{"voters", "staff"}}, "",
			`"displayName":"Alice Example","groups":["voters","staff"]`, ""},
		"no such claims": {LoginConfig{}, nil, "", `"displayName":"","groups":[]`, ""},
		"null claims":    {LoginConfig{}, map[string]any{"name": nil, "groups": nil}, "", `"displayName":"","groups":[]`, ""},
		"other paths": {LoginConfig{SessionClaims: SessionClaims{DisplayName: "/profile/full~1name", Groups: "/roles/0/members"}},
			map[string]any{"name": "ignored", "profile": map[string]any{"full/name": "Alice"}, "roles": []any{map[string]any{"members": []any{"a"}}}},
			"", `"displayName":"Alice","groups":["a"]`, ""},
		"the verified connector, not the asked one": {withConnector,
			map[string]any{"federated_claims": map[string]any{"connector_id": "audience"}}, "operator",
			`"displayName":"","groups":[],"connector":"audience"`, ""},
		"a connector configured, none in the token": {withConnector, nil, "operator", `"displayName":"","groups":[]`, ""},
		"groups as a string":                        {LoginConfig{}, map[string]any{"groups": "voters"}, "", "", "session-claims-invalid"},
		"a group that is a number":                  {LoginConfig{}, map[string]any{"groups": []any{"a", 7}}, "", "", "session-claims-invalid"},
		"a name that is an object":                  {LoginConfig{}, map[string]any{"name": map[string]any{"first": "A"}}, "", "", "session-claims-invalid"},
		"a connector that is a list": {withConnector,
			map[string]any{"federated_claims": map[string]any{"connector_id": []any{"audience"}}}, "", "", "session-claims-invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarnessWith(t, tc.login)
			h.issuer.mu.Lock()
			h.issuer.tamper = func(claims map[string]any) {
				for k, v := range tc.claims {
					claims[k] = v
				}
			}
			h.issuer.mu.Unlock()
			b := h.browser()
			query := url.Values{}
			if tc.ask != "" {
				query.Set("oidc.connector_id", tc.ask)
			}
			start := b.loginWith(query.Encode())
			done := b.get(h.issuer.authorize(start.header.Get("Location"), alice))
			if tc.reason != "" {
				assertLoginError(t, done, http.StatusBadGateway, tc.reason)
				return
			}
			if done.code != http.StatusSeeOther {
				t.Fatalf("callback answered %d %s", done.code, done.body)
			}
			body := b.get("/auth/session").body
			if !strings.Contains(body, `"email":"`+alice+`",`+tc.want+`,"expiresAt"`) {
				t.Errorf("/auth/session = %s, want %s", body, tc.want)
			}
		})
	}
}

func TestPointer(t *testing.T) {
	doc := map[string]any{
		"a": map[string]any{"b/c": "slash", "d~e": "tilde", "": "empty key"},
		"l": []any{"zero", map[string]any{"x": "one.x"}},
	}
	for ptr, want := range map[string]any{
		"/a/b~1c": "slash", "/a/d~0e": "tilde", "/a/": "empty key", "/l/0": "zero", "/l/1/x": "one.x",
	} {
		p, err := parsePointer(ptr)
		if err != nil {
			t.Fatalf("%s: %v", ptr, err)
		}
		if got, ok := p.lookup(doc); !ok || got != want {
			t.Errorf("%s = %v, %v, want %v", ptr, got, ok, want)
		}
	}
	for _, ptr := range []string{"/missing", "/l/2", "/l/-1", "/l/01", "/l/x", "/a/b~1c/deeper"} {
		p, err := parsePointer(ptr)
		if err != nil {
			t.Fatalf("%s: %v", ptr, err)
		}
		if got, ok := p.lookup(doc); ok {
			t.Errorf("%s = %v, want nothing", ptr, got)
		}
	}
	for _, ptr := range []string{"", "a", "name", "/a~", "/a~2", "/~x"} {
		if _, err := parsePointer(ptr); err == nil {
			t.Errorf("parsePointer(%q) accepted it", ptr)
		}
	}
}

func TestParseLoginConfig(t *testing.T) {
	c, err := ParseLoginConfig([]byte(`
authorizationParameters:
  connector_id:
    default: audience
    allowFromRequest: true
    allowedValues: [audience, operator]
  login_hint:
    allowFromRequest: true
sessionClaims:
  displayName: /name
  groups: /groups
  connector: /federated_claims/connector_id
`))
	if err != nil {
		t.Fatal(err)
	}
	if p := c.AuthorizationParameters["connector_id"]; p.Default != "audience" || !p.AllowFromRequest ||
		!slices.Equal(p.AllowedValues, []string{"audience", "operator"}) ||
		!c.AuthorizationParameters["login_hint"].AllowFromRequest || c.SessionClaims.Connector != "/federated_claims/connector_id" {
		t.Fatalf("%+v", c)
	}
	for name, text := range map[string]string{
		"a misspelt key":   "authorizationParameter: {}",
		"a misspelt field": "authorizationParameters: {x: {allowFromRequests: true}}",
		"a wrong type":     "authorizationParameters: {x: {allowedValues: audience}}",
	} {
		if _, err := ParseLoginConfig([]byte(text)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
