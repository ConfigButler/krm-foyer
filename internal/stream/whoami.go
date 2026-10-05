package stream

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/ConfigButler/krm-foyer/internal/gate"
	"github.com/ConfigButler/krm-foyer/internal/interruption"
)

// whoami is /auth/whoami's answer: who Kubernetes takes the user to be, and the
// session's issuer and end. The groups here are the API server's mapping, which
// differ from the issuer's groups /auth/session shows. It never holds a token.
type whoami struct {
	UserInfo  authenticationv1.UserInfo `json:"userInfo"`
	Issuer    string                    `json:"issuer"`
	ExpiresAt time.Time                 `json:"expiresAt"`
}

// WhoAmI serves GET /auth/whoami: a fresh SelfSubjectReview, sent with the user's own
// token through the gate, so its bounds hold, and answered exactly as the API server
// answered. It lives here because the stream's shared watches ask the same question
// the same way (selfSubjectReview), and there is one implementation of it. Nothing is
// inferred from the token: a refusal is passed on, a failure is a 503, and there is no
// other credential to try.
func (s *Streams) WhoAmI() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a := s.gate.Admit(w, r)
		if a == nil {
			return
		}
		defer a.Close()
		u := &upstream{streams: s, userAgent: r.UserAgent()}
		info, err := u.selfSubjectReview(a.Request.Context(), a.Credential)
		var api apierrors.APIStatus
		switch {
		case err == nil && info.Username != "":
			writeWhoami(w, a.Credential, info)
		case err == nil:
			s.logger.Warn("the API server named no user for /auth/whoami")
			a.Interrupt(w, unknownIdentity)
		case errors.As(err, &api) && api.Status().Code >= 400 && api.Status().Code < 500:
			// The API server's own refusal, as it gave it: a token it does not accept
			// is its 401, not krm-foyer's.
			a.Refused(gate.ByKubernetes, "status", api.Status().Code, "reason", string(api.Status().Reason))
			st := api.Status()
			body, _ := json.Marshal(&st)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(int(st.Code))
			_, _ = w.Write(body)
		default:
			s.logger.Warn("could not find who the user is for /auth/whoami", "cause", class(err))
			a.Interrupt(w, unknownIdentity)
		}
	})
}

// unknownIdentity is the answer when the API server could not say who the user is.
var unknownIdentity = &interruption.Interruption{
	Status: http.StatusServiceUnavailable, Reason: "ServiceUnavailable",
	Message: "the API server could not say who you are; try again later",
}

func writeWhoami(w http.ResponseWriter, cred gate.Credential, info authenticationv1.UserInfo) {
	body, err := json.Marshal(whoami{UserInfo: info, Issuer: cred.Issuer, ExpiresAt: cred.Expires})
	if err != nil {
		panic(err) // strings, lists and a time: cannot fail
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
