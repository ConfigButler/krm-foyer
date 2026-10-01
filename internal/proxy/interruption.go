package proxy

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// An Interruption is an answer krm-foyer gives instead of the API server's. Every
// kind is a row of the interruptions table in docs/design.md, and nothing else may
// stand between a user and the API server's answer.
type Interruption struct {
	Status  int
	Reason  string
	Message string
	// Causes go into the Status details, for example the target of a redirect.
	Causes []Cause
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
	setResponseHeaders(h)
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(i.Status)
	_, _ = w.Write(body)
}
