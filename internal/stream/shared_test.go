package stream

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ConfigButler/krm-stream/gateway"
	"github.com/ConfigButler/krm-stream/gateway/kube"
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/client-go/kubernetes/scheme"

	"github.com/ConfigButler/krm-foyer/internal/gate"
	"github.com/ConfigButler/krm-foyer/internal/interruption"
	"github.com/ConfigButler/krm-foyer/internal/metrics"
)

const (
	sharedToken = "shared-token-91b3e5" //nolint:gosec // a marker to search for, not a credential
	// sharedName is who the API server takes the shared token for: its name must not
	// reach a browser.
	sharedName = "system:serviceaccount:krm-foyer:krm-foyer-shared"
	// configmaps is a resource that is not shared.
	configmaps = "/stream/v1?version=v1&resource=configmaps&namespace=hello"
)

// sharedAPI stands in for the API server of shared watches. It knows users by
// token, answers SelfSubjectReviews with who they are and SubjectAccessReviews by
// allowed, and serves watches of notes that change pushes to.
type sharedAPI struct {
	*apiServer
	users map[string]string // token -> username

	mu      sync.Mutex
	allowed map[string]bool // username -> may list and watch notes
	reviews []authorizationv1.SubjectAccessReviewSpec
	watches map[http.ResponseWriter]openWatch
	rv      int
	// sar answers a SubjectAccessReview instead, when set.
	sar http.HandlerFunc
	// watch answers a watch instead, when set.
	watch http.HandlerFunc
}

func newSharedAPI(t *testing.T) *sharedAPI {
	t.Helper()
	a := &sharedAPI{
		users: map[string]string{ //nolint:gosec // markers to search for, not credentials
			userToken: "oidc:alice@example.com", otherToken: "oidc:bob@example.com",
			"carol-token": "oidc:carol@example.com", sharedToken: sharedName,
		},
		allowed: map[string]bool{"oidc:alice@example.com": true, "oidc:bob@example.com": true},
		watches: map[http.ResponseWriter]openWatch{},
		rv:      100,
	}
	a.apiServer = newAPIServer(t, a.serve)
	return a
}

func (a *sharedAPI) allow(user string, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.allowed[user] = ok
}

func (a *sharedAPI) serve(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	user, known := a.users[token]
	if !known {
		status(http.StatusUnauthorized, "Unauthorized", "Unauthorized")(w, r)
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/apis/authentication.k8s.io/v1/selfsubjectreviews":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"apiVersion": "authentication.k8s.io/v1", "kind": "SelfSubjectReview",
			"status": map[string]any{"userInfo": map[string]any{
				"username": user, "uid": "uid-" + user, "groups": []string{"system:authenticated", "readers"},
				"extra": map[string]any{"scopes": []string{"openid"}},
			}},
		})
	case r.Method == http.MethodPost && r.URL.Path == "/apis/authorization.k8s.io/v1/subjectaccessreviews":
		if user != sharedName {
			status(http.StatusForbidden, "Forbidden", user+" cannot create subjectaccessreviews")(w, r)
			return
		}
		// client-go sends it as protobuf, as it does to a real API server.
		body, _ := io.ReadAll(r.Body)
		obj, _, err := scheme.Codecs.UniversalDeserializer().Decode(body, nil, nil)
		review, ok := obj.(*authorizationv1.SubjectAccessReview)
		if err != nil || !ok {
			status(http.StatusBadRequest, "BadRequest", fmt.Sprint("not a SubjectAccessReview: ", err))(w, r)
			return
		}
		a.mu.Lock()
		a.reviews = append(a.reviews, review.Spec)
		allowed := a.allowed[review.Spec.User] && review.Spec.ResourceAttributes.Resource == "notes"
		sar := a.sar
		a.mu.Unlock()
		if sar != nil {
			sar(w, r)
			return
		}
		review.Status = authorizationv1.SubjectAccessReviewStatus{Allowed: allowed}
		if !allowed {
			review.Status.Reason = "no RBAC policy matched"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(review)
	case r.URL.Query().Get("watch") == "true" || r.URL.Query().Get("watch") == "1":
		a.mu.Lock()
		watch := a.watch
		a.mu.Unlock()
		if watch != nil {
			watch(w, r)
			return
		}
		a.serveWatch(w, r)
	default:
		status(http.StatusNotFound, "NotFound", "not here")(w, r)
	}
}

// openWatch is a watch the fake is serving: where its changes go, and closed once
// it has ended.
type openWatch struct {
	changes chan string
	ended   chan struct{}
}

// serveWatch serves a watch of notes: the snapshot, then every change.
func (a *sharedAPI) serveWatch(w http.ResponseWriter, r *http.Request) {
	{
		changes, ended := make(chan string, 16), make(chan struct{})
		defer close(ended)
		a.mu.Lock()
		a.watches[w] = openWatch{changes, ended}
		a.mu.Unlock()
		defer func() {
			a.mu.Lock()
			delete(a.watches, w)
			a.mu.Unlock()
		}()
		snapshotOf(w, 10, "note first")
		for {
			select {
			case <-r.Context().Done():
				return
			case line := <-changes:
				_, _ = w.Write([]byte(line))
				_ = http.NewResponseController(w).Flush()
			}
		}
	}
}

// change sends every open watch a new text for the note first.
func (a *sharedAPI) change(text string) {
	a.mu.Lock()
	a.rv++
	line, _ := json.Marshal(map[string]any{"type": "MODIFIED", "object": map[string]any{
		"apiVersion": "hello.krm-foyer.example/v1", "kind": "Note",
		"metadata": map[string]any{
			"name": "first", "namespace": "hello", "uid": "uid-first", "resourceVersion": fmt.Sprint(a.rv),
		},
		"spec": map[string]any{"text": text},
	}})
	open := make([]openWatch, 0, len(a.watches))
	for _, o := range a.watches {
		open = append(open, o)
	}
	a.mu.Unlock()
	// Sent without the lock, which a watch ending takes, and not to one that ended.
	for _, o := range open {
		select {
		case o.changes <- string(line) + "\n":
		case <-o.ended:
		}
	}
}

// openWatches is how many watches the API server is serving now.
func (a *sharedAPI) openWatches() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.watches)
}

func (a *sharedAPI) subjectReviews() []authorizationv1.SubjectAccessReviewSpec {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]authorizationv1.SubjectAccessReviewSpec(nil), a.reviews...)
}

// requestsWith returns the paths of the requests that carried token.
func (a *sharedAPI) requestsWith(token string) []string {
	var out []string
	for _, r := range a.received() {
		if r.Header.Get("Authorization") == "Bearer "+token {
			out = append(out, r.Method+" "+r.URL.Path)
		}
	}
	return out
}

// sharedFoyer serves streams with notes shared, as the shared token, rechecking
// every recheck.
func sharedFoyer(t *testing.T, api *sharedAPI, creds gate.Credentials, m *metrics.Metrics, recheck time.Duration,
	adjust ...func(*Config),
) foyer {
	t.Helper()
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte(sharedToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resources, err := ParseResources("notes.hello.krm-foyer.example")
	if err != nil {
		t.Fatal(err)
	}
	return newFoyer(t, api.apiServer, creds, func(c *Config, g *gate.Config) {
		g.Metrics = m
		c.Shared = &SharedConfig{TokenFile: file, Resources: resources, RecheckInterval: recheck, DecisionTTL: recheck / 2}
		for _, a := range adjust {
			a(c)
		}
	})
}

// as is the header that makes a byHeader session of token.
func as(token string) http.Header { return http.Header{"Test-Token": {token}} }

// eventuallyEqual waits until got() is want.
func eventuallyEqual(t *testing.T, what string, got func() string, want string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for got() != want {
		if time.Now().After(deadline) {
			t.Fatalf("%s: %s, want %s", what, got(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Streams of one scope share one watch at the API server, whoever opens them: three
// for alice and three for bob are one watch, opened with the shared identity's
// token. Each user is asked about once, not once a stream, and one change reaches
// every stream. When the last stream ends, the watch ends with it.
func TestStreamsOfOneScopeShareOneWatch(t *testing.T) {
	api := newSharedAPI(t)
	m := metrics.New()
	f := sharedFoyer(t, api, byHeader{}, m, time.Minute)
	ctx, cancel := context.WithCancel(t.Context())
	var streams []*sse
	for _, token := range []string{userToken, userToken, userToken, otherToken, otherToken, otherToken} {
		s := f.open(ctx, t, notes, as(token))
		if got := texts(t, s.until(t, "synced"))["first"]; got != "note first" {
			t.Fatalf("snapshot: first reads %q", got)
		}
		streams = append(streams, s)
	}

	if n := api.openWatches(); n != 1 {
		t.Errorf("the API server serves %d watches for six streams of one scope, want 1", n)
	}
	var watches []string
	for _, r := range api.received() {
		if r.URL.Query().Get("watch") != "" {
			watches = append(watches, r.Header.Get("Authorization"))
		}
	}
	if len(watches) != 1 || watches[0] != "Bearer "+sharedToken {
		t.Errorf("watches opened with %q, want one, with the shared token", watches)
	}
	if n := len(api.subjectReviews()); n != 4 {
		t.Errorf("%d SubjectAccessReviews for two users, want 4: list and watch, once each", n)
	}
	for series, want := range map[string]string{
		"krm_foyer_streams_open":                             "6",
		"krm_foyer_shared_subscriptions_open":                "6",
		`krm_foyer_upstream_watches_open{identity="shared"}`: "1",
		userWatches: "0",
		`krm_foyer_access_checks_total{result="allowed",source="api_server"}`: "2",
		`krm_foyer_access_checks_total{result="allowed",source="cache"}`:      "4",
		`krm_foyer_subject_reviews_total{result="resolved"}`:                  "6",
	} {
		if got := gauge(t, m, series); got != want {
			t.Errorf("%s %s, want %s", series, got, want)
		}
	}

	api.change("changed for everyone")
	for i, s := range streams {
		if got := texts(t, s.until(t, "modified"))["first"]; got != "changed for everyone" {
			t.Errorf("stream %d: first reads %q after the change", i, got)
		}
	}

	cancel()
	eventuallyEqual(t, "shared watches after every stream ended",
		func() string { return gauge(t, m, `krm_foyer_upstream_watches_open{identity="shared"}`) }, "0")
	eventuallyEqual(t, "subscriptions after every stream ended",
		func() string { return gauge(t, m, "krm_foyer_shared_subscriptions_open") }, "0")
	eventuallyEqual(t, "watches at the API server", func() string { return fmt.Sprint(api.openWatches()) }, "0")
}

// The shared watch's identity sees more than a user may, so each user is asked about
// before a stream is served from it, by who the API server says they are. Carol,
// whom RBAC does not allow, is refused with nothing from the shared watch, though
// alice's stream holds it open with the very notes she asked for.
func TestASharedStreamAsksAboutEachUser(t *testing.T) {
	api := newSharedAPI(t)
	f := sharedFoyer(t, api, byHeader{}, nil, time.Minute)
	f.open(t.Context(), t, notes, as(userToken)).until(t, "synced")

	carol := f.open(t.Context(), t, notes, as("carol-token"))
	e, err := carol.next()
	if err != nil || e.Type != "error" || e.Code != "FORBIDDEN" || !e.Terminal {
		t.Fatalf("carol: %+v, %v; want a terminal FORBIDDEN", e, err)
	}
	_ = carol.rest(t)
	if strings.Contains(carol.raw.String(), "note first") {
		t.Errorf("carol was sent the shared watch's notes: %s", carol.raw.String())
	}

	var carols []authorizationv1.SubjectAccessReviewSpec
	for _, spec := range api.subjectReviews() {
		if spec.User == "oidc:carol@example.com" {
			carols = append(carols, spec)
		}
	}
	if len(carols) == 0 {
		t.Fatal("nobody asked the API server about carol")
	}
	got := carols[0]
	if got.UID != "uid-oidc:carol@example.com" || strings.Join(got.Groups, ",") != "system:authenticated,readers" ||
		len(got.Extra["scopes"]) != 1 {
		t.Errorf("asked about %+v; want carol as the API server resolved her, groups, UID and extras", got)
	}
	attrs := got.ResourceAttributes
	if attrs == nil || attrs.Group != "hello.krm-foyer.example" || attrs.Resource != "notes" || attrs.Namespace != "hello" ||
		(attrs.Verb != "list" && attrs.Verb != "watch") {
		t.Errorf("asked about %+v; want list or watch of notes in hello", attrs)
	}
	// Decisions are reused across label selectors because the reviews ask about none
	// (see decisions.Authorize). Should they start to, the key must take them.
	if attrs != nil && (attrs.LabelSelector != nil || attrs.FieldSelector != nil) {
		t.Errorf("the reviews now ask about selectors (%+v): key decisions on them", attrs)
	}
	if paths := api.requestsWith("carol-token"); strings.Join(paths, ",") != "POST /apis/authentication.k8s.io/v1/selfsubjectreviews" {
		t.Errorf("carol's token was sent with %q; want her SelfSubjectReview alone", paths)
	}
}

// A grant taken away reaches an open shared stream at its next recheck: bob's ends
// with FORBIDDEN, and alice's goes on, on the same watch, with what changes next.
func TestARevokedGrantEndsOnlyThatSharedStream(t *testing.T) {
	api := newSharedAPI(t)
	f := sharedFoyer(t, api, byHeader{}, nil, 200*time.Millisecond)
	alice := f.open(t.Context(), t, notes, as(userToken))
	alice.until(t, "synced")
	// Bob's stream gets a deadline, so a grant that is never rechecked fails the test
	// rather than hanging it.
	bobCtx, cancel := context.WithTimeout(t.Context(), within)
	defer cancel()
	bob := f.open(bobCtx, t, notes, as(otherToken))
	bob.until(t, "synced")

	api.allow("oidc:bob@example.com", false)
	revoked := time.Now()
	var last event
	for {
		e, err := bob.next()
		if err != nil {
			break
		}
		last = e
	}
	if last.Type != "error" || last.Code != "FORBIDDEN" || !last.Terminal {
		t.Errorf("bob's stream ended with %+v; want a terminal FORBIDDEN", last)
	}
	// The recheck interval, the decision's lifetime, and a margin.
	if took := time.Since(revoked); took > 2*time.Second {
		t.Errorf("bob's stream outlived his grant by %v", took)
	}

	api.change("after bob left")
	if got := texts(t, alice.until(t, "modified"))["first"]; got != "after bob left" {
		t.Errorf("alice's stream reads %q", got)
	}
	if n := api.openWatches(); n != 1 {
		t.Errorf("%d watches at the API server, want alice's shared one still", n)
	}
}

// Tokens do not mix: a user's token goes with the user's own requests alone (the
// SelfSubjectReview, and the watch of a resource that is not shared), and the shared
// token with the reviews and the shared watch alone. The shared identity's name
// reaches no browser.
func TestTokensDoNotMix(t *testing.T) {
	api := newSharedAPI(t)
	f := sharedFoyer(t, api, byHeader{}, nil, time.Minute)
	shared := f.open(t.Context(), t, notes, as(userToken))
	shared.until(t, "synced")
	own := f.open(t.Context(), t, configmaps, as(userToken))
	own.until(t, "synced")

	for _, r := range api.received() {
		auth := r.Header.Get("Authorization")
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/selfsubjectreviews") || strings.HasSuffix(path, "/configmaps"):
			if auth != "Bearer "+userToken {
				t.Errorf("%s %s was sent with %q, want the user's token", r.Method, path, auth)
			}
		case strings.HasSuffix(path, "/subjectaccessreviews") || strings.HasSuffix(path, "/notes"):
			if auth != "Bearer "+sharedToken {
				t.Errorf("%s %s was sent with %q, want the shared token", r.Method, path, auth)
			}
		default:
			t.Errorf("unexpected %s %s", r.Method, path)
		}
	}
	for _, s := range []*sse{shared, own} {
		if raw := s.raw.String(); strings.Contains(raw, sharedToken) || strings.Contains(raw, sharedName) {
			t.Errorf("the shared identity reached the browser: %s", raw)
		}
	}
}

// What the shared watch needs that goes wrong ends the stream without serving it:
// a token the API server does not take, the API server unable to say who the user
// is or whether they may, and the shared identity itself refused. None is an allow,
// and the last is krm-foyer's configuration, not the user's permissions.
func TestASharedStreamFailsClosed(t *testing.T) {
	for name, tc := range map[string]struct {
		token  string
		adjust func(*sharedAPI)
		code   string
		// terminal: whether the browser's client should stop trying.
		terminal bool
	}{
		"a token the API server does not take": {token: "unknown-token", code: "UNAUTHENTICATED", terminal: true},
		"SubjectAccessReviews failing": {token: userToken, code: "UPSTREAM_UNAVAILABLE", adjust: func(a *sharedAPI) {
			a.sar = status(http.StatusServiceUnavailable, "ServiceUnavailable", "etcd is down")
		}},
		"SubjectAccessReviews answered without a decision": {token: userToken, code: "FORBIDDEN", terminal: true, adjust: func(a *sharedAPI) {
			a.sar = func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"apiVersion":"authorization.k8s.io/v1","kind":"SubjectAccessReview","status":{}}`))
			}
		}},
		"the shared identity refused": {token: userToken, code: "INTERNAL", terminal: true, adjust: func(a *sharedAPI) {
			a.watch = status(http.StatusForbidden, "Forbidden",
				`notes is forbidden: User "`+sharedName+`" cannot watch resource "notes"`)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			api := newSharedAPI(t)
			if tc.adjust != nil {
				tc.adjust(api)
			}
			f := sharedFoyer(t, api, byHeader{}, nil, time.Minute)
			s := f.open(t.Context(), t, notes, as(tc.token))
			var seen []event
			for {
				e, err := s.next()
				if err != nil {
					break
				}
				seen = append(seen, e)
			}
			if len(seen) == 0 {
				t.Fatal("the stream ended with no event")
			}
			last := seen[len(seen)-1]
			if last.Type != "error" || last.Code != tc.code || last.Terminal != tc.terminal {
				t.Errorf("ended with %+v; want %s, terminal %v", last, tc.code, tc.terminal)
			}
			if strings.Contains(s.raw.String(), "note first") {
				t.Errorf("served though it failed: %s", types(seen))
			}
			if strings.Contains(s.raw.String(), sharedName) || strings.Contains(f.logs.String(), sharedName) {
				t.Errorf("the shared identity's name was passed on: %s\n%s", s.raw.String(), f.logs.String())
			}
		})
	}
}

// A session that ends closes its own shared streams and no one else's: the watch
// stays open for the others.
func TestASessionEndingLeavesTheSharedWatchToOthers(t *testing.T) {
	api := newSharedAPI(t)
	var live atomic.Bool
	live.Store(true)
	ending := credentials{token: otherToken, session: "ending", live: &live}
	creds := credsFunc(func(r *http.Request) (gate.Credential, *interruption.Interruption) {
		if r.Header.Get("Test-Token") == otherToken {
			return ending.Token(r)
		}
		return byHeader{}.Token(r)
	})
	f := sharedFoyer(t, api, creds, nil, time.Minute)
	alice := f.open(t.Context(), t, notes, as(userToken))
	alice.until(t, "synced")
	bob := f.open(t.Context(), t, notes, as(otherToken))
	bob.until(t, "synced")

	live.Store(false)
	if err := bob.rest(t); err == nil {
		t.Error("bob's stream ended cleanly; a stream cut short is aborted")
	}
	api.change("after bob's session")
	if got := texts(t, alice.until(t, "modified"))["first"]; got != "after bob's session" {
		t.Errorf("alice's stream reads %q", got)
	}
}

// credsFunc is a gate.Credentials from a function.
type credsFunc func(*http.Request) (gate.Credential, *interruption.Interruption)

func (f credsFunc) Token(r *http.Request) (gate.Credential, *interruption.Interruption) { return f(r) }

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func mustGate(t *testing.T) *gate.Gate {
	t.Helper()
	g, err := gate.New(gate.Config{Credentials: byHeader{}})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// Resources are configured as kubectl names them, and nothing else is taken.
func TestParseResources(t *testing.T) {
	got, err := ParseResources(" notes.hello.krm-foyer.example, configmaps,,configmaps")
	if err != nil {
		t.Fatal(err)
	}
	want := []gateway.GroupResource{sharedResource("hello.krm-foyer.example", "notes"), sharedResource("", "configmaps")}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got %v, want %v", got, want)
	}
	for _, bad := range []string{"hello.krm-foyer.example/notes", "Notes", ".example", "notes v1"} {
		if _, err := ParseResources(bad); err == nil {
			t.Errorf("%q was taken", bad)
		}
	}
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"", filepath.Join(t.TempDir(), "missing"), empty} {
		if _, err := New(Config{Server: mustURL(t, "https://api.example"), Gate: mustGate(t),
			Shared: &SharedConfig{TokenFile: file, Resources: got}}); err == nil {
			t.Errorf("shared watches with token file %q were taken", file)
		}
	}
}

// A decision is reused for the very question it answered, for its lifetime, and
// one asked twice at once is asked once. An error is not kept.
func TestDecisionsAreReusedForTheSameQuestionOnly(t *testing.T) {
	var asked atomic.Int32
	var fail atomic.Bool
	release := make(chan struct{})
	check := gateway.AuthorizerFunc(func(_ context.Context, p gateway.Principal, _ gateway.Scope) error {
		asked.Add(1)
		<-release
		if fail.Load() {
			return errors.New("the API server did not answer")
		}
		if s, _ := subjectOf(p); strings.Join(s.Groups, ",") != "readers" {
			return gateway.Forbidden("no")
		}
		return nil
	})
	now := time.Now()
	var clock sync.Mutex
	d := &decisions{check: check, ttl: time.Minute, metrics: nil, entries: map[string]*decision{},
		now: func() time.Time { clock.Lock(); defer clock.Unlock(); return now }}
	reader := subscriber{subject: kube.Subject{User: "alice", Groups: []string{"readers"}}}
	other := subscriber{subject: kube.Subject{User: "alice", Groups: []string{"writers"}}}
	scope := gateway.Scope{Group: "hello.krm-foyer.example", Version: "v1", Resource: "notes", Namespace: "hello"}

	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			if err := d.Authorize(t.Context(), reader, scope); err != nil {
				t.Errorf("reader: %v", err)
			}
		})
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if n := asked.Load(); n != 1 {
		t.Errorf("ten checks at once asked %d times, want 1", n)
	}
	if err := d.Authorize(t.Context(), other, scope); !isDenial(err) {
		t.Errorf("alice with other groups: %v; want her own answer, a denial", err)
	}
	elsewhere := scope
	elsewhere.Namespace = "other"
	_ = d.Authorize(t.Context(), reader, elsewhere)
	if n := asked.Load(); n != 3 {
		t.Errorf("asked %d times, want 3: other groups and another namespace are other questions", n)
	}
	selected := scope
	selected.LabelSelector = "app=notes"
	if err := d.Authorize(t.Context(), reader, selected); err != nil || asked.Load() != 3 {
		t.Errorf("another label selector: %v, asked %d times; want the decision reused, as the reviews ask about no selector", err, asked.Load())
	}

	clock.Lock()
	now = now.Add(time.Minute)
	clock.Unlock()
	fail.Store(true)
	if err := d.Authorize(t.Context(), reader, scope); err == nil || isDenial(err) {
		t.Errorf("after its lifetime: %v; want the question asked again, and its error", err)
	}
	fail.Store(false)
	if err := d.Authorize(t.Context(), reader, scope); err != nil {
		t.Errorf("after an error: %v; want the question asked again", err)
	}
	if n := asked.Load(); n != 5 {
		t.Errorf("asked %d times, want 5: an error is not kept", n)
	}
}

// Reusing decisions never changes an answer: whatever the order of questions and
// however the clock moves, each check answers exactly what the API server would say
// to that question asked directly. The oracle knows nothing of the cache; questions
// differ in one field of the subject or the scope at a time, the cases a key that
// left something out would confuse.
func FuzzDecisionsAreTransparent(f *testing.F) {
	f.Add([]byte{0, 0, 1, 0, 2, 1, 255, 0, 0, 0, 3, 4, 0, 4, 8})
	f.Add([]byte{17, 17, 33, 33, 200, 17, 49, 49})
	users := []string{"alice", "bob"}
	groups := [][]string{{"readers"}, {"writers"}, {"readers", "writers"}, nil}
	apiGroups := []string{"hello.krm-foyer.example", "other.example"}
	namespaces := []string{"hello", "other", ""}
	// question builds the subscriber one byte stands for, and the scope the next, each
	// field from bits of its own.
	question := func(who, what byte) (subscriber, gateway.Scope) {
		sub := subscriber{subject: kube.Subject{User: users[who&1], Groups: groups[(who>>1)&3]}}
		if who&8 != 0 {
			sub.subject.UID = "uid-1"
		}
		if who&16 != 0 {
			sub.subject.Extra = map[string]authorizationv1.ExtraValue{"scopes": {"openid"}}
		}
		scope := gateway.Scope{Group: apiGroups[what&1], Version: "v1", Resource: "notes", Namespace: namespaces[int(what>>5)%3]}
		if what&2 != 0 {
			scope.Name = "first"
		}
		if what&4 != 0 {
			scope.LabelSelector = "app=notes"
		}
		if what&8 != 0 {
			scope.Resource = "configmaps"
		}
		if what&16 != 0 {
			scope.Version = "v2"
		}
		return sub, scope
	}
	// oracle is the API server's answer, a fixed function of the whole question the
	// reviews ask: the subject, and the scope but for its label selector.
	oracle := func(sub subscriber, scope gateway.Scope) error {
		key, _ := json.Marshal([]any{sub.subject, scope.Group, scope.Version, scope.Resource, scope.Namespace, scope.Name})
		if sha256.Sum256(key)[0]&1 == 0 {
			return gateway.Forbidden("no")
		}
		return nil
	}
	f.Fuzz(func(t *testing.T, ops []byte) {
		now := time.Unix(0, 0)
		d := &decisions{
			check: gateway.AuthorizerFunc(func(_ context.Context, p gateway.Principal, s gateway.Scope) error {
				return oracle(p.(subscriber), s)
			}),
			ttl: 10 * time.Second, now: func() time.Time { return now }, entries: map[string]*decision{},
		}
		for i := 0; i+1 < len(ops); i += 2 {
			if ops[i] == 255 { // time passes
				now = now.Add(7 * time.Second)
				continue
			}
			sub, scope := question(ops[i], ops[i+1])
			got, want := d.Authorize(t.Context(), sub, scope), oracle(sub, scope)
			if (got == nil) != (want == nil) {
				t.Fatalf("question %08b %08b (%+v, %+v): got %v, want %v", ops[i], ops[i+1], sub.subject, scope, got, want)
			}
		}
	})
}

// A browser that stops reading cannot hold a shared stream open past a revoked
// grant: krm-stream delivers and rechecks one at a time, so a write blocked on a
// full connection would hold the recheck off for as long as it blocks. The write
// timeout ends the write, and the stream with it.
func TestABrowserThatStopsReadingCannotHoldOffARecheck(t *testing.T) {
	api := newSharedAPI(t)
	m := metrics.New()
	f := sharedFoyer(t, api, byHeader{}, m, 200*time.Millisecond, func(c *Config) { c.WriteTimeout = 300 * time.Millisecond })
	stalled := f.open(t.Context(), t, notes, as(otherToken))
	stalled.until(t, "synced")

	// Changes, many and large, that the browser never reads: they fill the buffers
	// between krm-foyer and the browser, and krm-foyer's next write blocks.
	big := strings.Repeat("x", 128<<10)
	for i := range 160 {
		api.change(fmt.Sprint(i, big))
	}
	api.allow("oidc:bob@example.com", false)
	revoked := time.Now()
	// The recheck interval, the decision's lifetime, the write timeout, and a margin.
	eventuallyEqual(t, "streams open after the grant was revoked",
		func() string { return gauge(t, m, "krm_foyer_streams_open") }, "0")
	if took := time.Since(revoked); took > 3*time.Second {
		t.Errorf("the stalled stream outlived its grant by %v", took)
	}
}

// One shared watch stuck opening holds up nothing else: a stream of another scope
// is served meanwhile, and the stuck opening is cancelled at the API server once
// the stream that asked for it leaves, which frees its slot.
func TestAStuckOpeningHoldsUpNoOtherScope(t *testing.T) {
	api := newSharedAPI(t)
	stuck, released := make(chan struct{}, 1), make(chan struct{}, 1)
	api.watch = func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/namespaces/stuck/") {
			api.serveWatch(w, r)
			return
		}
		stuck <- struct{}{}
		<-r.Context().Done() // no answer, not even headers, until krm-foyer gives up
		released <- struct{}{}
	}
	m := metrics.New()
	f := sharedFoyer(t, api, byHeader{}, m, time.Minute)

	leaving, leave := context.WithCancel(t.Context())
	f.open(leaving, t, strings.Replace(notes, "namespace=hello", "namespace=stuck", 1), as(userToken))
	select {
	case <-stuck:
	case <-time.After(within):
		t.Fatal("the stuck watch was never asked for")
	}

	ctx, cancel := context.WithTimeout(t.Context(), within)
	defer cancel()
	other := f.open(ctx, t, notes, as(otherToken))
	if got := texts(t, other.until(t, "synced"))["first"]; got != "note first" {
		t.Errorf("another scope's snapshot reads %q", got)
	}

	leave()
	select {
	case <-released:
	case <-time.After(within):
		t.Error("the stuck opening was never cancelled at the API server")
	}
	eventuallyEqual(t, "streams open after the stuck one left",
		func() string { return gauge(t, m, "krm_foyer_streams_open") }, "1")
}
