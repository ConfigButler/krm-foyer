//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// stream is a response from krm-foyer that stays open, such as a native watch, read
// as it arrives.
type stream struct {
	resp   *http.Response
	body   *bufio.Reader
	read   bytes.Buffer
	target string
	// Marker is the request's User-Agent, which finds its audit events.
	Marker string
}

// watch opens a GET through krm-foyer with the user's session and returns once its
// head has arrived. The browser's recorder reads every body to the end, so a stream
// goes around it, and records what it read when it ends.
func (u user) watch(ctx context.Context, path string) *stream {
	GinkgoHelper()
	target := fx.foyerURL + "/k8s" + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	Expect(err).NotTo(HaveOccurred())
	marker := "krm-foyer-e2e/" + randomID()
	req.Header.Set("User-Agent", marker)
	for _, c := range u.b.jar.Cookies(req.URL) {
		req.AddCookie(c)
	}
	resp, err := u.b.transport.RoundTrip(req) //nolint:bodyclose // closed by DeferCleanup below, after the spec
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() { _ = resp.Body.Close() })
	return &stream{resp: resp, body: bufio.NewReader(resp.Body), target: target, Marker: marker}
}

// event reads the next event, one line of a JSON watch.
func (s *stream) event() string {
	GinkgoHelper()
	line, err := s.body.ReadString('\n')
	s.read.WriteString(line)
	Expect(err).NotTo(HaveOccurred(), "the stream ended before an event")
	return line
}

// end waits up to within for the stream to end, and returns when, and how it ended:
// nil for a clean end. It fails the spec if the stream is still open then.
func (s *stream) end(within time.Duration) (time.Time, error) {
	GinkgoHelper()
	type ending struct {
		err error
		at  time.Time
	}
	ended := make(chan ending, 1)
	go func() {
		_, err := io.Copy(&s.read, s.body)
		ended <- ending{err, time.Now()}
	}()
	var e ending
	select {
	case e = <-ended:
	case <-time.After(within):
		_ = s.resp.Body.Close()
		e = <-ended
		Fail("the stream was still open after " + within.String())
	}
	fx.seenMu.Lock()
	fx.seen = append(fx.seen, seenResponse{target: s.target, header: s.resp.Header.Clone(), body: s.read.Bytes()})
	fx.seenMu.Unlock()
	return e.at, e.err
}
