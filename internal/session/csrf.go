package session

import (
	"crypto/subtle"
	"errors"
	"net/http"
)

// CSRFHeader carries a mutation's CSRF proof: the session's CSRFToken, which a page
// on krm-foyer's origin reads from /auth/session.
const CSRFHeader = "X-CSRF-Token"

// The refusals CheckMutation gives. Each is a request the session will not act on,
// whoever's it is; neither is an access decision, which is the API server's.
var (
	// ErrCrossOrigin means the request did not show that it came from krm-foyer's
	// origin.
	ErrCrossOrigin = errors.New("the request did not come from krm-foyer's origin")
	// ErrNoCSRFProof means the request did not carry the session's CSRF token.
	ErrNoCSRFProof = errors.New("the request carries no valid CSRF proof")
)

// CheckMutation decides whether r may change state on behalf of s. GET and HEAD
// may; every other method needs both of:
//
//   - CSRF proof: exactly one CSRFHeader field, equal to the session's token. A page
//     on another site cannot read the token, so cannot send it.
//   - Same origin: exactly one Origin field, equal to the configured origin, or with
//     no Origin at all, exactly one Sec-Fetch-Site field saying same-origin. Page
//     scripts can set neither.
//
// A refusal is ErrCrossOrigin or ErrNoCSRFProof. Header fields are read as a list: a
// repeated field is a refusal, never a choice between its values.
func (m *Manager) CheckMutation(r *http.Request, s Session) error {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return nil
	}
	if !m.sameOrigin(r.Header) {
		return ErrCrossOrigin
	}
	proof := r.Header.Values(CSRFHeader)
	if len(proof) != 1 || s.CSRFToken == "" ||
		subtle.ConstantTimeCompare([]byte(proof[0]), []byte(s.CSRFToken)) != 1 {
		return ErrNoCSRFProof
	}
	return nil
}

func (m *Manager) sameOrigin(h http.Header) bool {
	if origins, ok := h["Origin"]; ok {
		return len(origins) == 1 && origins[0] == m.origin
	}
	site := h.Values("Sec-Fetch-Site")
	return len(site) == 1 && site[0] == "same-origin"
}
