package session

import (
	"crypto/subtle"
	"net/http"

	"github.com/ConfigButler/krm-foyer/internal/proxy"
)

// CSRFHeader carries a mutation's CSRF proof: the session's CSRFToken, which a page
// on krm-foyer's origin reads from /auth/session.
const CSRFHeader = "X-CSRF-Token"

// CheckMutation decides whether r may change state on behalf of s. GET and HEAD
// may; every other method needs both of:
//
//   - CSRF proof: exactly one CSRFHeader field, equal to the session's token. A page
//     on another site cannot read the token, so cannot send it.
//   - Same origin: exactly one Origin field, equal to the configured origin, or with
//     no Origin at all, exactly one Sec-Fetch-Site field saying same-origin. Page
//     scripts can set neither.
//
// A refusal is an *proxy.Interruption with status 403 and a reason that is not
// RBAC's Forbidden. Header fields are read as a list: a repeated field is a refusal,
// never a choice between its values.
func (m *Manager) CheckMutation(r *http.Request, s Session) error {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return nil
	}
	if !m.sameOrigin(r.Header) {
		return &proxy.Interruption{
			Status: http.StatusForbidden, Reason: "CrossOriginRequest",
			Message: "krm-foyer only accepts this request from its own origin",
		}
	}
	proof := r.Header.Values(CSRFHeader)
	if len(proof) != 1 || s.CSRFToken == "" ||
		subtle.ConstantTimeCompare([]byte(proof[0]), []byte(s.CSRFToken)) != 1 {
		return &proxy.Interruption{
			Status: http.StatusForbidden, Reason: "CSRFProofRequired",
			Message: "this request needs the session's CSRF token in the " + CSRFHeader + " header; see /auth/session",
		}
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
