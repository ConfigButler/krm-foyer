// Package auth is krm-foyer's login half: OIDC authorization code with PKCE, state and
// nonce against one configured issuer, ending in a session sealed into a cookie. It serves
// /auth/login, /auth/callback, /auth/session, /auth/logout and /auth/logged-out, and
// gives the API half its credential: the token of the session a request may use, or
// the answer to give instead. docs/design.md, "Login and sessions", is the contract.
package auth

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/ConfigButler/krm-foyer/internal/gate"
	"github.com/ConfigButler/krm-foyer/internal/interruption"
	"github.com/ConfigButler/krm-foyer/internal/pages"
	"github.com/ConfigButler/krm-foyer/internal/session"
)

// DefaultScopes are asked for when the configuration names none. There is no
// offline_access: krm-foyer holds no refresh token until it can refresh.
var DefaultScopes = []string{oidc.ScopeOpenID, "email", "profile"}

// Config is what login needs.
type Config struct {
	// PublicURL is krm-foyer's origin as browsers reach it. The redirect URI is
	// PublicURL + /auth/callback; no request header changes it.
	PublicURL string
	// Issuer is the OIDC issuer URL, exactly as it appears in its tokens.
	Issuer string
	// ClientID and ClientSecret are krm-foyer's client at the issuer. The API server
	// must accept ID tokens issued to ClientID.
	ClientID, ClientSecret string
	// Scopes are asked for at login. Nil means DefaultScopes. openid is required;
	// offline_access is refused.
	Scopes []string
	// Login is the deployment's login configuration: extra parameters for the
	// issuer, and the claims /auth/session shows.
	Login LoginConfig
	// RootCAs verifies the issuer's certificate. Nil means the system roots.
	RootCAs *x509.CertPool
	// Sessions is where a login ends.
	Sessions *session.Manager
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// Logger receives one line per refused login. Nil discards them.
	Logger *slog.Logger
}

// Auth serves the login routes. Create it with New, then call Run or Discover
// before it can sign anyone in.
type Auth struct {
	cfg          Config
	redirectURI  string
	scopes       []string
	client       *http.Client
	now          func() time.Time
	logger       *slog.Logger
	transactions *transactions
	parameters   parameters
	claims       claimPaths
	issuer       atomic.Pointer[issuer]
}

// issuer is what discovery yields.
type issuer struct {
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
}

// New checks cfg and returns an Auth that is not ready yet. It makes no request.
func New(cfg Config) (*Auth, error) {
	public, err := url.Parse(cfg.PublicURL)
	if err != nil || public.Scheme != "https" || public.Host == "" || (public.Path != "" && public.Path != "/") ||
		public.RawQuery != "" || public.Fragment != "" || public.User != nil {
		return nil, fmt.Errorf("public URL must be an https URL with no path, query or user info, got %q", cfg.PublicURL)
	}
	if u, err := url.Parse(cfg.Issuer); err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("issuer must be an https URL, got %q", cfg.Issuer)
	}
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, errors.New("an OIDC client ID and secret are required")
	}
	if cfg.Sessions == nil {
		return nil, errors.New("no session manager")
	}
	scopes := cfg.Scopes
	if scopes == nil {
		scopes = DefaultScopes
	}
	if !slices.Contains(scopes, oidc.ScopeOpenID) {
		return nil, fmt.Errorf("scopes must include %q, got %q", oidc.ScopeOpenID, scopes)
	}
	if slices.Contains(scopes, oidc.ScopeOfflineAccess) {
		return nil, fmt.Errorf("scope %q asks for a refresh token, which krm-foyer does not use yet", oidc.ScopeOfflineAccess)
	}
	params, err := newParameters(cfg.Login.AuthorizationParameters)
	if err != nil {
		return nil, err
	}
	claims, err := newClaimPaths(cfg.Login.SessionClaims)
	if err != nil {
		return nil, err
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Auth{
		cfg:          cfg,
		redirectURI:  "https://" + public.Host + "/auth/callback",
		scopes:       slices.Clone(scopes),
		client:       issuerClient(cfg.RootCAs),
		now:          now,
		logger:       logger,
		transactions: newTransactions(now),
		parameters:   params,
		claims:       claims,
	}, nil
}

// issuerClient reaches the OIDC issuer. Like the API server's, the destination is
// pinned: no proxy from the environment.
func issuerClient(roots *x509.CertPool) *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			TLSClientConfig:     &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout: 10 * time.Second,
			ForceAttemptHTTP2:   true,
		},
	}
}

// Discover reads the issuer's discovery document once. Until it succeeds, login
// answers 503.
func (a *Auth) Discover(ctx context.Context) error {
	ctx = oidc.ClientContext(ctx, a.client)
	provider, err := oidc.NewProvider(ctx, a.cfg.Issuer)
	if err != nil {
		return fmt.Errorf("discovering the OIDC issuer: %w", err)
	}
	// The verifier fetches keys with this context's client later, so it must not be
	// one that ends when this call does.
	keys := oidc.ClientContext(context.Background(), a.client)
	a.issuer.Store(&issuer{
		oauth: oauth2.Config{
			ClientID: a.cfg.ClientID, ClientSecret: a.cfg.ClientSecret,
			Endpoint: provider.Endpoint(), RedirectURL: a.redirectURI, Scopes: a.scopes,
		},
		verifier: provider.VerifierContext(keys, &oidc.Config{ClientID: a.cfg.ClientID, Now: a.now}),
	})
	return nil
}

// Run retries Discover until it succeeds or ctx ends.
func (a *Auth) Run(ctx context.Context) {
	wait := time.Second
	for {
		err := a.Discover(ctx)
		if err == nil {
			a.logger.Info("OIDC issuer discovered", "issuer", a.cfg.Issuer)
			return
		}
		a.logger.Warn("OIDC issuer not reachable yet", "err", err, "retry_in", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(2*wait, 30*time.Second)
	}
}

// Ready reports whether discovery has succeeded.
func (a *Auth) Ready() bool { return a.issuer.Load() != nil }

// Handler serves the routes under /auth/.
func (a *Auth) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/login", a.login)
	mux.HandleFunc("GET /auth/callback", a.callback)
	mux.HandleFunc("GET /auth/session", a.session)
	mux.HandleFunc("POST /auth/logout", a.logout)
	mux.HandleFunc("GET /auth/logged-out", func(w http.ResponseWriter, _ *http.Request) {
		pages.Render(w, http.StatusOK, "logged-out.html", nil)
	})
	return mux
}

// A loginError is a stable reason for a refused login, shown on the error page and
// logged. It never carries anything the request supplied.
type loginError struct {
	status  int
	reason  string
	message string
}

var (
	errNotLocal      = loginError{http.StatusBadRequest, "return-path-not-local", "The page to return to after signing in must be a path on this site."}
	errNotReady      = loginError{http.StatusServiceUnavailable, "issuer-unavailable", "The sign-in service has not been reached yet. Try again in a moment."}
	errNoTransaction = loginError{http.StatusBadRequest, "login-not-in-progress", "This sign-in was not started in this browser, has expired, or has been used already."}
	errMalformed     = loginError{http.StatusBadRequest, "malformed-callback", "The sign-in service sent back an incomplete answer."}
	errState         = loginError{http.StatusBadRequest, "state-mismatch", "This answer does not belong to the sign-in this browser started."}
	errIssuerRefused = loginError{http.StatusBadRequest, "issuer-refused", "The sign-in service did not sign you in."}
	errExchange      = loginError{http.StatusBadGateway, "token-exchange-failed", "The sign-in service did not issue a token."}
	errIDToken       = loginError{http.StatusBadGateway, "id-token-invalid", "The sign-in service issued a token krm-foyer cannot accept."}
	errNonce         = loginError{http.StatusBadGateway, "nonce-mismatch", "The sign-in service issued a token for a different sign-in."}
	errTooLarge      = loginError{http.StatusBadGateway, "session-too-large", "The sign-in service issued a token too large to keep in a session cookie."}
	errParameter     = loginError{http.StatusBadRequest, "login-parameter-refused", "This sign-in link asks for an option this site does not offer."}
	errLoginTooLarge = loginError{http.StatusBadRequest, "login-too-large", "This sign-in link is too long to keep while you sign in."}
	errClaims        = loginError{http.StatusBadGateway, "session-claims-invalid", "The sign-in service issued a token whose claims krm-foyer cannot read."}
)

// issuerErrors are the error codes RFC 6749 and OIDC define for an authorization
// response. Only these are shown; anything else the callback says is not repeated
// on krm-foyer's origin.
var issuerErrors = []string{
	"access_denied", "invalid_request", "unauthorized_client", "unsupported_response_type",
	"invalid_scope", "server_error", "temporarily_unavailable", "interaction_required",
	"login_required", "account_selection_required", "consent_required",
}

// fail answers a refused login with the error page. retry and given are the login to
// try again: its return path, and those of its parameters that may be repeated.
func (a *Auth) fail(w http.ResponseWriter, e loginError, retry string, given map[string]string, issuerError string) {
	a.logger.Info("login refused", "reason", e.reason, "issuer_error", issuerError)
	pages.Render(w, e.status, "login-error.html", struct {
		Reason, Message, IssuerError, Retry string
	}{e.reason, e.message, issuerError, a.parameters.retry(retry, given)})
}

// tokenErrors are the error codes RFC 6749 defines for a token response. Only these
// are logged; anything else the issuer says is not.
var tokenErrors = []string{
	"invalid_request", "invalid_client", "invalid_grant", "unauthorized_client",
	"unsupported_grant_type", "invalid_scope",
}

// exchangeFailure describes a failed token exchange in fields that are safe to log.
// The error itself is never logged: oauth2 puts the token endpoint's answer in it
// (error_description, error_uri, or the whole body when there is no error code), and
// an issuer may echo the request it refused, which held the client secret, the code
// and the PKCE verifier.
func exchangeFailure(err error) []any {
	var refused *oauth2.RetrieveError
	var netErr net.Error
	switch {
	case errors.As(err, &refused):
		code := refused.ErrorCode
		switch {
		case code == "":
			code = "none"
		case !slices.Contains(tokenErrors, code):
			code = "unknown"
		}
		status := 0
		if refused.Response != nil {
			status = refused.Response.StatusCode
		}
		return []any{"issuer_status", status, "issuer_error", code}
	case errors.Is(err, context.Canceled):
		return []any{"cause", "canceled"}
	case errors.As(err, &netErr) && netErr.Timeout():
		return []any{"cause", "timeout"}
	case errors.As(err, new(*url.Error)):
		return []any{"cause", "unreachable"}
	default:
		// A 2xx answer krm-foyer could not read as a token response.
		return []any{"cause", "invalid-response"}
	}
}

// idTokenCauses name go-oidc's refusals by a phrase of its error text. The rest of
// that text quotes the token's claims and the issuer's key-set response, which hold
// whatever the issuer put there. TestIDTokenErrorIsNotLogged has a case for each, so
// a go-oidc upgrade that rewords one fails there instead of logging "invalid".
var idTokenCauses = []struct{ phrase, cause string }{
	{"fetching keys", "keys-unavailable"},
	{"issued by a different provider", "issuer-mismatch"},
	{"expected audience", "audience-mismatch"},
	{"before the nbf", "not-yet-valid"},
	{"failed to verify", "signature"},
}

// idTokenFailure describes an ID token the verifier refused in fields that are safe to
// log. Like exchangeFailure, it never logs the error itself.
func idTokenFailure(err error) []any {
	if errors.As(err, new(*oidc.TokenExpiredError)) {
		return []any{"cause", "expired"}
	}
	for _, c := range idTokenCauses {
		if strings.Contains(err.Error(), c.phrase) {
			return []any{"cause", c.cause}
		}
	}
	return []any{"cause", "invalid"}
}

// single returns the one value of key in q, or false if there is not exactly one.
func single(q url.Values, key string) (string, bool) {
	v := q[key]
	if len(v) != 1 {
		return "", false
	}
	return v[0], true
}

func (a *Auth) login(w http.ResponseWriter, r *http.Request) {
	// Strictly: a query Go would read only in part is refused, not half read.
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		a.fail(w, errParameter, "/", nil, "")
		return
	}
	returnTo := "/"
	if _, given := q["return_to"]; given {
		v, ok := single(q, "return_to")
		if !ok || !localPath(v) {
			a.fail(w, errNotLocal, "/", nil, "")
			return
		}
		returnTo = v
	}
	given, err := a.parameters.fromRequest(q)
	if err != nil {
		a.fail(w, errParameter, returnTo, nil, "")
		return
	}
	iss := a.issuer.Load()
	if iss == nil {
		a.fail(w, errNotReady, returnTo, given, "")
		return
	}
	cookie, t, err := a.transactions.begin(returnTo, given)
	if err != nil {
		// Within each bound, but too large together once sealed: refused here, not
		// dropped by the browser and lost at the callback.
		a.fail(w, errLoginTooLarge, "/", nil, "")
		return
	}
	for _, name := range a.transactions.excess(r) {
		http.SetCookie(w, transactionCookie(name, "", -1))
	}
	http.SetCookie(w, cookie)
	w.Header().Set("Cache-Control", "no-store")
	// The extra parameters go first: oauth2 applies options in order, so nonce and
	// PKCE, which come after, could not be replaced even by a name New had let
	// through. state, client_id, redirect_uri and scope are set before any option,
	// which is why New refuses those names.
	var opts []oauth2.AuthCodeOption
	for name, v := range a.parameters.send(given) {
		opts = append(opts, oauth2.SetAuthURLParam(name, v))
	}
	opts = append(opts, oidc.Nonce(t.Nonce), oauth2.S256ChallengeOption(t.Verifier))
	http.Redirect(w, r, iss.oauth.AuthCodeURL(t.State, opts...), http.StatusFound)
}

func (a *Auth) callback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	state, ok := single(q, "state")
	if !ok || state == "" {
		a.fail(w, errMalformed, "/", nil, "")
		return
	}
	// The state names the login this answers. It is used up by this request,
	// whatever happens next; other logins in progress in this browser stay.
	name := transactionCookieName(state)
	http.SetCookie(w, transactionCookie(name, "", -1))
	cookies := r.CookiesNamed(name)
	if len(cookies) != 1 {
		a.fail(w, errNoTransaction, "/", nil, "")
		return
	}
	t, ok := a.transactions.open(name, cookies[0].Value)
	if !ok {
		a.fail(w, errNoTransaction, "/", nil, "")
		return
	}
	if subtle.ConstantTimeCompare([]byte(state), []byte(t.State)) != 1 {
		a.fail(w, errState, t.ReturnTo, t.Params, "")
		return
	}
	// Only now is the answer known to belong to this login, error or not.
	if _, ok := q["error"]; ok {
		code, _ := single(q, "error")
		if !slices.Contains(issuerErrors, code) {
			code = "unknown"
		}
		a.fail(w, errIssuerRefused, t.ReturnTo, t.Params, code)
		return
	}
	code, okCode := single(q, "code")
	if !okCode || code == "" {
		a.fail(w, errMalformed, t.ReturnTo, t.Params, "")
		return
	}

	iss := a.issuer.Load()
	if iss == nil { // cannot happen: begin needs a discovered issuer
		a.fail(w, errNotReady, t.ReturnTo, t.Params, "")
		return
	}
	ctx := context.WithValue(r.Context(), oauth2.HTTPClient, a.client)
	token, err := iss.oauth.Exchange(ctx, code, oauth2.VerifierOption(t.Verifier))
	if err != nil {
		a.logger.Warn("token exchange failed", exchangeFailure(err)...)
		a.fail(w, errExchange, t.ReturnTo, t.Params, "")
		return
	}
	raw, _ := token.Extra("id_token").(string)
	if raw == "" {
		a.fail(w, errIDToken, t.ReturnTo, t.Params, "")
		return
	}
	idToken, err := iss.verifier.Verify(ctx, raw)
	if err != nil {
		a.logger.Warn("ID token refused", idTokenFailure(err)...)
		a.fail(w, errIDToken, t.ReturnTo, t.Params, "")
		return
	}
	if subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(t.Nonce)) != 1 {
		a.fail(w, errNonce, t.ReturnTo, t.Params, "")
		return
	}
	var claims struct {
		Email string `json:"email"`
	}
	if err := idToken.Claims(&claims); err != nil {
		a.fail(w, errIDToken, t.ReturnTo, t.Params, "")
		return
	}
	// What /auth/session will show must be readable now, or the login fails here
	// rather than every page later.
	all, err := tokenClaims(raw)
	if err == nil {
		_, err = a.claims.extract(all)
	}
	if err != nil {
		a.logger.Warn("the ID token's claims do not match sessionClaims")
		a.fail(w, errClaims, t.ReturnTo, t.Params, "")
		return
	}

	err = a.cfg.Sessions.Start(w, r, session.Session{
		Issuer: idToken.Issuer, Subject: idToken.Subject, Email: claims.Email,
		IDToken: raw, TokenExpiry: idToken.Expiry,
	})
	if errors.Is(err, session.ErrTooLarge) {
		// The issuer's token, with its groups, is too large for a cookie: an
		// integration problem to fix there, not here. The error says by how much.
		a.logger.Warn("session does not fit in a cookie", "err", err)
		a.fail(w, errTooLarge, t.ReturnTo, t.Params, "")
		return
	}
	if err != nil {
		a.fail(w, errIDToken, t.ReturnTo, t.Params, "")
		return
	}
	// Not http.Redirect: it cleans a relative target with path.Clean, and the
	// application gets back exactly the path it asked for. localPath allows no
	// character that could end the header.
	w.Header().Set("Location", t.ReturnTo)
	w.WriteHeader(http.StatusSeeOther)
}

// sessionState is /auth/session's answer. It never holds a token. Signed in, it
// always has displayName and groups, empty when the token has no such claim, and
// connector only when one is configured and the token has it: a fixed shape, never a
// bag of whatever claims the token carries.
type sessionState struct {
	Authenticated bool       `json:"authenticated"`
	Issuer        string     `json:"issuer,omitempty"`
	Subject       string     `json:"subject,omitempty"`
	Email         string     `json:"email,omitempty"`
	DisplayName   *string    `json:"displayName,omitempty"`
	Groups        *[]string  `json:"groups,omitempty"`
	Connector     string     `json:"connector,omitempty"`
	ExpiresAt     *time.Time `json:"expiresAt,omitempty"`
	CSRFToken     string     `json:"csrfToken,omitempty"`
	CSRFHeader    string     `json:"csrfHeader,omitempty"`
}

func (a *Auth) session(w http.ResponseWriter, r *http.Request) {
	s, err := a.cfg.Sessions.Lookup(r)
	switch {
	case errors.Is(err, session.ErrNoSession):
		writeJSON(w, http.StatusUnauthorized, sessionState{})
	case err != nil:
		a.refusal(err).Write(w)
	default:
		expires := s.Expires
		// Read again from the token, which krm-foyer verified at login and sealed into
		// the session, rather than kept beside it: the cookie has no room for a second
		// copy of the groups. The login checked they can be read.
		var id identity
		claims, err := tokenClaims(s.IDToken)
		if err == nil {
			id, err = a.claims.extract(claims)
		}
		if err != nil {
			// sessionClaims changed since this login. Show nothing rather than a guess.
			a.logger.Warn("a session's claims do not match sessionClaims")
			id = identity{Groups: []string{}}
		}
		writeJSON(w, http.StatusOK, sessionState{
			Authenticated: true, Issuer: s.Issuer, Subject: s.Subject, Email: s.Email,
			DisplayName: &id.DisplayName, Groups: &id.Groups, Connector: id.Connector,
			ExpiresAt: &expires, CSRFToken: s.CSRFToken, CSRFHeader: session.CSRFHeader,
		})
	}
}

// logout clears the session cookie, ends the responses the session has open in this
// process, and answers 204. It needs the same proof as any mutation, so another site
// cannot sign the user out. Without a session there is nothing to protect: the
// cookie is cleared and the answer is the same. It revokes nothing: a copy of the
// cookie is a session until it expires (docs/design.md, "Sessions").
func (a *Auth) logout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if _, err := a.cfg.Sessions.Use(r); err != nil && !errors.Is(err, session.ErrNoSession) {
		a.refusal(err).Write(w)
		return
	}
	a.cfg.Sessions.End(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// Token is the API half's credential (gate.Credentials): the ID token of the session
// r may use, or the interruption to answer r with instead. Its Live says whether the
// session has expired, or been ended in this process, since.
func (a *Auth) Token(r *http.Request) (gate.Credential, *interruption.Interruption) {
	s, err := a.cfg.Sessions.Use(r)
	if err != nil {
		return gate.Credential{}, a.refusal(err)
	}
	user := s.Email
	if user == "" {
		user = s.Subject
	}
	live := a.cfg.Sessions.Watch(r.Context(), s)
	return gate.Credential{Token: s.IDToken, User: user, Session: s.Handle(), Issuer: s.Issuer, Expires: s.Expires, Live: func(ctx context.Context) bool {
		return live() && ctx.Err() == nil
	}}, nil
}

// refusal is the answer to a request the session manager did not let through. This
// is the one place a session error becomes HTTP. Its refusals are 403s with reasons of
// their own, never RBAC's Forbidden; any other error, which the manager does not
// give, decides nothing.
func (a *Auth) refusal(err error) *interruption.Interruption {
	switch {
	case errors.Is(err, session.ErrNoSession):
		return interruption.NotSignedIn()
	case errors.Is(err, session.ErrCrossOrigin):
		return &interruption.Interruption{
			Status: http.StatusForbidden, Reason: "CrossOriginRequest",
			Message: "krm-foyer only accepts this request from its own origin",
		}
	case errors.Is(err, session.ErrNoCSRFProof):
		return &interruption.Interruption{
			Status: http.StatusForbidden, Reason: "CSRFProofRequired",
			Message: "this request needs the session's CSRF token in the " + session.CSRFHeader + " header; see /auth/session",
		}
	default:
		a.logger.Error("session check failed", "err", err)
		return &interruption.Interruption{
			Status: http.StatusServiceUnavailable, Reason: "ServiceUnavailable",
			Message: "the session could not be checked; try again later",
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		panic(err) // only strings and times: cannot fail
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
