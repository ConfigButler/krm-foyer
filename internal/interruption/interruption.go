// Package interruption renders the answers krm-foyer gives instead of the API
// server's: a Kubernetes Status for code, and a page for a person browsing. Every
// kind is a row of the interruptions table in docs/design.md. It decides nothing:
// the proxy and the login half build the Interruption, and this package only writes
// it.
package interruption

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	"github.com/ConfigButler/krm-foyer/internal/pages"
)

// Header names the reason on every interruption, in both forms. The proxy passes no
// upstream header but its allowlist, so the API server, or an aggregated API, cannot
// send it: code that sees it knows krm-foyer answered, whatever the body says.
const Header = "Krm-Foyer-Interruption"

// An Interruption is an answer krm-foyer gives instead of the API server's. Every
// kind is a row of the interruptions table in docs/design.md, and nothing else may
// stand between a user and the API server's answer.
type Interruption struct {
	Status  int
	Reason  string
	Message string
	// Causes go into the Status details, for example the target of a redirect.
	Causes []Cause
	// RetryAfter, when positive, is sent as Retry-After in seconds, in both forms:
	// how long to wait before the request can succeed.
	RetryAfter int
}

// Cause is a Kubernetes StatusCause.
type Cause struct {
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	Field   string `json:"field,omitempty"`
}

func (i *Interruption) Error() string {
	return strconv.Itoa(i.Status) + " " + i.Reason + ": " + i.Message
}

// status is the Kubernetes metav1.Status shape, so code reads an interruption the
// way it reads any API error.
type status struct {
	Kind       string         `json:"kind"`
	APIVersion string         `json:"apiVersion"`
	Metadata   struct{}       `json:"metadata"`
	Status     string         `json:"status"`
	Message    string         `json:"message"`
	Reason     string         `json:"reason"`
	Details    *statusDetails `json:"details,omitempty"`
	Code       int            `json:"code"`
}

type statusDetails struct {
	Causes []Cause `json:"causes,omitempty"`
}

// NotSignedIn answers a request that has no signed-in user.
func NotSignedIn() *Interruption {
	return &Interruption{Status: http.StatusUnauthorized, Reason: "Unauthorized", Message: "not signed in"}
}

// SetHeaders sets what every answer under /k8s carries, proxied or not, replacing
// anything the API server sent.
func SetHeaders(h http.Header) {
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
}

// Serve answers r with the interruption: as a page when a person opened r's URL in a
// tab, and as a Status otherwise. The status code is the same either way.
func (i *Interruption) Serve(w http.ResponseWriter, r *http.Request) {
	if isNavigation(r) {
		i.page(w, r)
		return
	}
	i.Write(w)
}

// isNavigation reports whether a person opened r's URL in a tab: a GET with exactly
// one Sec-Fetch-Dest saying document. Browsers set it and page scripts cannot, and
// clients that are not browsers do not send it, so code always gets the Status.
func isNavigation(r *http.Request) bool {
	dest := r.Header.Values("Sec-Fetch-Dest")
	return r.Method == http.MethodGet && len(dest) == 1 && dest[0] == "document"
}

// Write answers with the interruption as a Kubernetes Status.
func (i *Interruption) Write(w http.ResponseWriter) {
	s := status{
		Kind: "Status", APIVersion: "v1", Status: "Failure",
		Message: i.Message, Reason: i.Reason, Code: i.Status,
	}
	if len(i.Causes) > 0 {
		s.Details = &statusDetails{Causes: i.Causes}
	}
	body, err := json.Marshal(s)
	if err != nil {
		panic(err) // only strings and ints: cannot fail
	}
	h := w.Header()
	SetHeaders(h)
	h.Set(Header, i.Reason)
	i.setRetryAfter(h)
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(i.Status)
	_, _ = w.Write(body)
}

// titles name each kind of interruption for a person.
var titles = map[string]string{
	"Unauthorized":              "Sign in to continue",
	"ServiceUnavailable":        "Your session could not be checked",
	"BadRequest":                "Not a canonical path",
	"NotFound":                  "Not a Kubernetes API route",
	"NotImplemented":            "Not supported by krm-foyer",
	"BadGateway":                "Held back by krm-foyer",
	"CSRFProofRequired":         "Refused by krm-foyer",
	"CrossOriginRequest":        "Refused by krm-foyer",
	"TooManyConcurrentRequests": "Too many requests at once",
	"RequestRateExceeded":       "Too many requests",
	"ResponseTooLarge":          "Response too large",
}

// page answers with the interruption as a page, for a person browsing.
func (i *Interruption) page(w http.ResponseWriter, r *http.Request) {
	data := struct {
		Status                     int
		Title, Message, Reason     string
		SignIn, Target, TargetLink string
	}{Status: i.Status, Title: titles[i.Reason], Message: i.Message, Reason: i.Reason}
	if data.Title == "" {
		data.Title = "Interrupted by krm-foyer"
	}
	if i.Status == http.StatusUnauthorized {
		// RequestURI passed the path check, so it is a path on this origin. Login
		// checks it again before using it.
		data.SignIn = "/auth/login?" + url.Values{"return_to": {r.RequestURI}}.Encode()
	}
	for _, c := range i.Causes {
		if c.Reason == "Redirect" {
			data.Target = c.Message
			// A link only to an absolute web URL; anything else stays text. The person
			// follows it, if they want to; nothing is re-sent.
			if u, err := url.Parse(c.Message); err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.Opaque == "" {
				data.TargetLink = c.Message
			}
		}
	}
	w.Header().Set(Header, i.Reason)
	i.setRetryAfter(w.Header())
	pages.Render(w, i.Status, "interruption.html", data)
}

func (i *Interruption) setRetryAfter(h http.Header) {
	if i.RetryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(i.RetryAfter))
	}
}
