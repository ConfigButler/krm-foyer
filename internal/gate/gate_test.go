package gate

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ConfigButler/krm-foyer/internal/interruption"
)

type anyone struct{}

func (anyone) Token(*http.Request) (Credential, *interruption.Interruption) {
	return Credential{Token: "t"}, nil
}

// A gate needs a credential source, and bounds that are positive or left at their
// defaults.
func TestNewRejectsBadConfig(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no credential source":          {},
		"negative check interval":       {Credentials: anyone{}, SessionCheckInterval: -1},
		"negative response duration":    {Credentials: anyone{}, MaxResponseDuration: -1},
		"negative per-session requests": {Credentials: anyone{}, MaxSessionConcurrentRequests: -1},
		"negative replica requests":     {Credentials: anyone{}, MaxConcurrentRequests: -1},
		"negative rate":                 {Credentials: anyone{}, SessionRequestRate: -1},
		"negative burst":                {Credentials: anyone{}, SessionRequestBurst: -1},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: New accepted it", name)
		}
	}
	if _, err := New(Config{Credentials: anyone{}}); err != nil {
		t.Errorf("defaults: %v", err)
	}
}

// bySession hands each request the session named by its Test-Session header.
type bySession struct{}

func (bySession) Token(r *http.Request) (Credential, *interruption.Interruption) {
	return Credential{Token: "t", Session: r.Header.Get("Test-Session"), Live: func(context.Context) bool { return true }}, nil
}

// admitAs asks g to let a request of session through, as a stream or not, and
// returns the admission (nil when refused) and the answer the gate wrote.
func admitAs(t *testing.T, g *Gate, session string, stream bool) (*Admission, *httptest.ResponseRecorder) {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", nil)
	r.Header.Set("Test-Session", session)
	if stream {
		return g.AdmitStream(w, r), w
	}
	return g.Admit(w, r), w
}

// assertRefused checks a 429 interruption with reason and the bound it names.
func assertRefused(t *testing.T, a *Admission, w *httptest.ResponseRecorder, reason, bound string) {
	t.Helper()
	if a != nil {
		a.Close()
		t.Fatalf("let through; want a %s refusal for %s", reason, bound)
	}
	if w.Code != http.StatusTooManyRequests || w.Header().Get(interruption.Header) != reason ||
		!strings.Contains(w.Body.String(), `"field":"`+bound+`"`) {
		t.Errorf("%d %s %q, want 429 %s naming %s", w.Code, w.Header().Get(interruption.Header), w.Body.String(), reason, bound)
	}
}

// Streams are counted apart from requests: a session at its limit of requests may
// still open a stream, and one at its limit of streams may still send requests.
func TestStreamsAreCountedApartFromRequests(t *testing.T) {
	g, err := New(Config{Credentials: bySession{}, MaxSessionConcurrentRequests: 1, MaxSessionStreams: 1})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := admitAs(t, g, "a", false)
	stream, w := admitAs(t, g, "a", true)
	if req == nil || stream == nil {
		t.Fatalf("a request and a stream of one session: %v %v (%d %s)", req, stream, w.Code, w.Body)
	}
	a, w := admitAs(t, g, "a", false)
	assertRefused(t, a, w, "TooManyConcurrentRequests", "session_concurrent_requests")
	a, w = admitAs(t, g, "a", true)
	assertRefused(t, a, w, "TooManyStreams", "session_streams")
	req.Close()
	stream.Close()
	for _, isStream := range []bool{false, true} {
		if a, _ := admitAs(t, g, "a", isStream); a == nil {
			t.Errorf("stream %v: a slot was not given back", isStream)
		} else {
			a.Close()
		}
	}
}

// One session may have so many streams open, and the replica so many in all.
// Another session is not affected by the first limit.
func TestStreamLimits(t *testing.T) {
	g, err := New(Config{Credentials: bySession{}, MaxSessionStreams: 2, MaxStreams: 3})
	if err != nil {
		t.Fatal(err)
	}
	var open []*Admission
	for _, s := range []string{"a", "a"} {
		a, _ := admitAs(t, g, s, true)
		open = append(open, a)
	}
	a, w := admitAs(t, g, "a", true)
	assertRefused(t, a, w, "TooManyStreams", "session_streams")
	b, _ := admitAs(t, g, "b", true)
	if b == nil {
		t.Fatal("another session was refused for the first one's limit")
	}
	open = append(open, b)
	c, w := admitAs(t, g, "c", true)
	assertRefused(t, c, w, "TooManyStreams", "streams")
	for _, a := range open {
		a.Close()
	}
}

// Opening a stream draws on the session's request rate, the same budget as /k8s,
// so a page that reconnects in a loop meets it.
func TestStreamsShareTheRequestRate(t *testing.T) {
	g, err := New(Config{Credentials: bySession{}, SessionRequestRate: 1, SessionRequestBurst: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, isStream := range []bool{false, true} {
		a, _ := admitAs(t, g, "a", isStream)
		if a == nil {
			t.Fatal("refused within the burst")
		}
		a.Close()
	}
	a, w := admitAs(t, g, "a", true)
	assertRefused(t, a, w, "RequestRateExceeded", "session_request_burst")
}

// A Credential handed to a logger or to fmt shows its user, never its token or its
// session.
func TestCredentialIsNotPrinted(t *testing.T) {
	c := Credential{Token: "token-7c1e", User: "alice@example.test", Session: "session-9d2f"}
	var out strings.Builder
	slog.New(slog.NewJSONHandler(&out, nil)).Info("x", "credential", c)
	slog.New(slog.NewTextHandler(&out, nil)).Info("x", "credential", c)
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		out.WriteString(fmt.Sprintf(verb, c) + "\n")
	}
	if s := out.String(); strings.Contains(s, "token-7c1e") || strings.Contains(s, "session-9d2f") || !strings.Contains(s, "alice@example.test") {
		t.Errorf("a credential printed as:\n%s", s)
	}
}
