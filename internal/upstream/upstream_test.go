package upstream

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"
	"syscall"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const secret = "user-token-4f1c9a" //nolint:gosec // a marker to search for, not a credential

// A class is one of a fixed set of words, whatever the error says: what an API server
// or anything in between wrote never reaches it.
func TestAClassNeverCarriesTheErrorsText(t *testing.T) {
	words := regexp.MustCompile(`^(none|redirect|cancelled|deadline|timeout|tls|connection|connection_closed|other|status_[1-5][0-9][0-9])$`)
	gr := schema.GroupResource{Group: "hello.krm-foyer.example", Resource: "notes"}
	for want, err := range map[string]error{
		"redirect":          &url.Error{Op: "Get", URL: "https://x/" + secret, Err: ErrRedirect},
		"cancelled":         fmt.Errorf("watch %s: %w", secret, context.Canceled),
		"deadline":          fmt.Errorf("watch %s: %w", secret, context.DeadlineExceeded),
		"status_503":        apierrors.NewServiceUnavailable("you sent Bearer " + secret),
		"status_403":        apierrors.NewForbidden(gr, secret, errors.New(secret)),
		"status_410":        apierrors.NewResourceExpired(secret),
		"tls":               &url.Error{Op: "Get", URL: "https://x", Err: x509.UnknownAuthorityError{}},
		"connection":        &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED},
		"connection_closed": fmt.Errorf("read %s: %w", secret, io.ErrUnexpectedEOF),
		"other":             fmt.Errorf(`net/http: HTTP/1.x transport connection broken: malformed HTTP response "Bearer %s"`, secret),
	} {
		got := Class(err)
		if got != want {
			t.Errorf("Class(%v) = %q, want %q", err, got, want)
		}
		if !words.MatchString(got) {
			t.Errorf("Class(%v) = %q, which is not one of the fixed words", err, got)
		}
	}
}
