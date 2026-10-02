// Package auth is krm-foyer's login half: OIDC authorization code with PKCE, state and
// nonce against one configured issuer, ending in a server-side session. It serves
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

	"github.com/ConfigButler/krm-foyer/internal/interruption"
	"github.com/ConfigButler/krm-foyer/internal/pages"
	"github.com/ConfigButler/krm-foyer/internal/proxy"
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
	errNotLocal       = loginError{http.StatusBadRequest, "return-path-not-local", "The page to return to after signing in must be a path on this site."}
	errNotReady       = loginError{http.StatusServiceUnavailable, "issuer-unavailable", "The sign-in service has not been reached yet. Try again in a moment."}
	errBusy           = loginError{http.StatusServiceUnavailable, "too-many-logins", "Too many sign-ins are in progress. Try again in a moment."}
	errNoTransaction  = loginError{http.StatusBadRequest, "login-not-in-progress", "This sign-in was not started in this browser, has expired, or has been used already."}
	errMalformed      = loginError{http.StatusBadRequest, "malformed-callback", "The sign-in service sent back an incomplete answer."}
	errState          = loginError{http.StatusBadRequest, "state-mismatch", "This answer does not belong to the sign-in this browser started."}
	errIssuerRefused  = loginError{http.StatusBadRequest, "issuer-refused", "The sign-in service did not sign you in."}
	errExchange       = loginError{http.StatusBadGateway, "token-exchange-failed", "The sign-in service did not issue a token."}
	errIDToken        = loginError{http.StatusBadGateway, "id-token-invalid", "The sign-in service issued a token krm-foyer cannot accept."}
	errNonce          = loginError{http.StatusBadGateway, "nonce-mismatch", "The sign-in service issued a token for a different sign-in."}
	errSessionStorage = loginError{http.StatusServiceUnavailable, "session-store-unavailable", "Your session could not be stored. Try again in a moment."}
)

// issuerErrors are the error codes RFC 6749 and OIDC define for an authorization
// response. Only these are shown; anything else the callback says is not repeated
// on krm-foyer's origin.
var issuerErrors = []string{
	"access_denied", "invalid_request", "unauthorized_client", "unsupported_response_type",
	"invalid_scope", "server_error", "temporarily_unavailable", "interaction_required",
	"login_required", "account_selection_required", "consent_required",
}

func (a *Auth) fail(w http.ResponseWriter, e loginError, retry, issuerError string) {
	a.logger.Info("login refused", "reason", e.reason, "issuer_error", issuerError)
	pages.Render(w, e.status, "login-error.html", struct {
		Reason, Message, IssuerError, Retry string
	}{e.reason, e.message, issuerError, "/auth/login?" + url.Values{"return_to": {retry}}.Encode()})
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
	returnTo := "/"
	if _, given := r.URL.Query()["return_to"]; given {
		v, ok := single(r.URL.Query(), "return_to")
		if !ok || !localPath(v) {
			a.fail(w, errNotLocal, "/", "")
			return
		}
		returnTo = v
	}
	iss := a.issuer.Load()
	if iss == nil {
		a.fail(w, errNotReady, returnTo, "")
		return
	}
	id, t, ok := a.transactions.begin(returnTo)
	if !ok {
		a.fail(w, errBusy, returnTo, "")
		return
	}
	http.SetCookie(w, transactionCookie(id, int(transactionLifetime/time.Second)))
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, iss.oauth.AuthCodeURL(t.state, oidc.Nonce(t.nonce), oauth2.S256ChallengeOption(t.verifier)), http.StatusFound)
}

func (a *Auth) callback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	// The transaction is used up by this request, whatever happens next: a second
	// callback with the same cookie, state or code finds nothing.
	http.SetCookie(w, transactionCookie("", -1))
	var t transaction
	found := 0
	for _, c := range r.CookiesNamed(transactionCookieName) {
		if got, ok := a.transactions.take(c.Value); ok {
			t, found = got, found+1
		}
	}
	if found != 1 || len(r.CookiesNamed(transactionCookieName)) != 1 {
		a.fail(w, errNoTransaction, "/", "")
		return
	}

	q := r.URL.Query()
	if _, ok := q["error"]; ok {
		code, _ := single(q, "error")
		if !slices.Contains(issuerErrors, code) {
			code = "unknown"
		}
		a.fail(w, errIssuerRefused, t.returnTo, code)
		return
	}
	state, okState := single(q, "state")
	code, okCode := single(q, "code")
	if !okState || !okCode || code == "" {
		a.fail(w, errMalformed, t.returnTo, "")
		return
	}
	if subtle.ConstantTimeCompare([]byte(state), []byte(t.state)) != 1 {
		a.fail(w, errState, t.returnTo, "")
		return
	}

	iss := a.issuer.Load()
	if iss == nil { // cannot happen: begin needs a discovered issuer
		a.fail(w, errNotReady, t.returnTo, "")
		return
	}
	ctx := context.WithValue(r.Context(), oauth2.HTTPClient, a.client)
	token, err := iss.oauth.Exchange(ctx, code, oauth2.VerifierOption(t.verifier))
	if err != nil {
		a.logger.Warn("token exchange failed", exchangeFailure(err)...)
		a.fail(w, errExchange, t.returnTo, "")
		return
	}
	raw, _ := token.Extra("id_token").(string)
	if raw == "" {
		a.fail(w, errIDToken, t.returnTo, "")
		return
	}
	idToken, err := iss.verifier.Verify(ctx, raw)
	if err != nil {
		a.logger.Warn("ID token refused", idTokenFailure(err)...)
		a.fail(w, errIDToken, t.returnTo, "")
		return
	}
	if subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(t.nonce)) != 1 {
		a.fail(w, errNonce, t.returnTo, "")
		return
	}
	var claims struct {
		Email string `json:"email"`
	}
	if err := idToken.Claims(&claims); err != nil {
		a.fail(w, errIDToken, t.returnTo, "")
		return
	}

	err = a.cfg.Sessions.Start(r.Context(), w, r, session.Session{
		Issuer: idToken.Issuer, Subject: idToken.Subject, Email: claims.Email,
		IDToken: raw, TokenExpiry: idToken.Expiry,
	})
	if err != nil {
		a.logger.Error("starting a session failed", "err", err)
		a.fail(w, errSessionStorage, t.returnTo, "")
		return
	}
	// Not http.Redirect: it cleans a relative target with path.Clean, and the
	// application gets back exactly the path it asked for. localPath allows no
	// character that could end the header.
	w.Header().Set("Location", t.returnTo)
	w.WriteHeader(http.StatusSeeOther)
}

// sessionState is /auth/session's answer. It never holds a token.
type sessionState struct {
	Authenticated bool       `json:"authenticated"`
	Issuer        string     `json:"issuer,omitempty"`
	Subject       string     `json:"subject,omitempty"`
	Email         string     `json:"email,omitempty"`
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
		expires := a.cfg.Sessions.ExpiresAt(s)
		writeJSON(w, http.StatusOK, sessionState{
			Authenticated: true, Issuer: s.Issuer, Subject: s.Subject, Email: s.Email,
			ExpiresAt: &expires, CSRFToken: s.CSRFToken, CSRFHeader: session.CSRFHeader,
		})
	}
}

// logout ends the session and answers 204. It needs the same proof as any
// mutation, so another site cannot sign the user out. Without a session there is
// nothing to protect: the cookie is cleared and the answer is the same.
func (a *Auth) logout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if _, err := a.cfg.Sessions.Use(r); err != nil && !errors.Is(err, session.ErrNoSession) {
		a.refusal(err).Write(w)
		return
	}
	if err := a.cfg.Sessions.End(r.Context(), w, r); err != nil {
		a.refusal(err).Write(w)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Token is the API half's credential (proxy.Credentials): the ID token of the session
// r may use, or the interruption to answer r with instead. Its Live asks the store
// again, by r's cookie, whether that session still exists.
func (a *Auth) Token(r *http.Request) (proxy.Credential, *interruption.Interruption) {
	s, err := a.cfg.Sessions.Use(r)
	if err != nil {
		return proxy.Credential{}, a.refusal(err)
	}
	return proxy.Credential{Token: s.IDToken, Live: func(ctx context.Context) bool {
		err := a.cfg.Sessions.Check(r.WithContext(ctx))
		if err != nil && !errors.Is(err, session.ErrNoSession) {
			a.logger.Warn("session store failed during a session check", "err", err)
		}
		// An answer that came too late is no answer: fail closed.
		return err == nil && ctx.Err() == nil
	}}, nil
}

// refusal is the answer to a request the session manager did not let through. This
// is the one place a session error becomes HTTP. Its refusals are 403s with reasons of
// their own, never RBAC's Forbidden; any error that is not one of them means the
// store could not say, and nothing is decided from it.
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
		a.logger.Error("session store failed", "err", err)
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
