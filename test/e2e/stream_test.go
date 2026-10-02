//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
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

// watch opens a GET of path through /k8s with the user's session and returns once
// its head has arrived.
func (u user) watch(ctx context.Context, path string) *stream {
	GinkgoHelper()
	return u.open(ctx, fx.foyerURL+"/k8s"+path)
}

// open sends a GET for target with the user's session and returns once its head has
// arrived. The browser's recorder reads every body to the end, so a stream goes
// around it, and records what it read when it ends, for the token scan.
func (u user) open(ctx context.Context, target string) *stream {
	GinkgoHelper()
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

// krmEvent is one event of a krm-stream, as the browser receives it.
type krmEvent struct {
	Type     string `json:"type"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	Terminal bool   `json:"terminal"`
	Object   struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec map[string]any `json:"spec"`
		Data map[string]any `json:"data"`
	} `json:"object"`
}

// krmEvent reads the next event of a krm-stream, past heartbeats and blank lines.
func (s *stream) krmEvent() krmEvent {
	GinkgoHelper()
	for {
		line, err := s.body.ReadString('\n')
		s.read.WriteString(line)
		Expect(err).NotTo(HaveOccurred(), "the stream ended before an event; read %s", s.read.String())
		if data, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "data: "); ok {
			var e krmEvent
			Expect(json.Unmarshal([]byte(data), &e)).To(Succeed(), data)
			return e
		}
	}
}

// until reads krm-stream events until one of type typ, and returns them all.
func (s *stream) until(typ string) []krmEvent {
	GinkgoHelper()
	var seen []krmEvent
	for {
		e := s.krmEvent()
		seen = append(seen, e)
		if e.Type == typ {
			return seen
		}
		Expect(e.Terminal).To(BeFalse(), "a terminal event before %q: %+v", typ, e)
	}
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
