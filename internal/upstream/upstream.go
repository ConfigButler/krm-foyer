// Package upstream names what went wrong reaching the API server, in words krm-foyer
// chooses. An error's own text is never logged: the API server, an aggregated API
// behind it, or whatever answered instead writes part of it, and it could hold
// anything, a token among it. Logs say which kind of failure it was, and the rest
// stays out.
package upstream

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// ErrRedirect is what a client that refuses redirects answers one with. krm-foyer
// follows no redirect from the API server: one could take the user's token elsewhere.
var ErrRedirect = errors.New("the API server answered with a redirect, which krm-foyer does not follow")

// Class names the kind of failure err is, from a fixed set: redirect, cancelled,
// deadline, timeout, tls, connection, connection_closed, or other; or status_NNN for an
// answer from the API server with that status code.
func Class(err error) string {
	var api apierrors.APIStatus
	var netErr net.Error
	var opErr *net.OpError
	var record tls.RecordHeaderError
	var verify *tls.CertificateVerificationError
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, ErrRedirect):
		return "redirect"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.As(err, &api) && api.Status().Code >= 100 && api.Status().Code < 600:
		return "status_" + strconv.Itoa(int(api.Status().Code))
	case errors.As(err, &verify), errors.As(err, &authority), errors.As(err, &hostname),
		errors.As(err, &invalid), errors.As(err, &record):
		return "tls"
	case errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	case errors.As(err, &opErr):
		return "connection"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "connection_closed"
	}
	return "other"
}
