package proxy

import (
	"bytes"
	"compress/gzip"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/ConfigButler/krm-foyer/internal/metrics"
)

// sizedAPIServer answers with size bytes of JSON-ish text, as written in chunks of
// at most chunk bytes, gzip-encoded when compress is set and the request asked for
// it. With a known length it sends Content-Length.
func sizedAPIServer(t *testing.T, http2 bool, size, chunk int, compress bool) *apiServer {
	t.Helper()
	return newAPIServerWith(t, http2, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := bytes.Repeat([]byte("a"), size)
		var out io.Writer = w
		if compress {
			if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
				t.Error("the transport did not ask for gzip")
			}
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			defer func() { _ = gz.Close() }()
			out = gz
		} else {
			w.Header().Set("Content-Length", strconv.Itoa(size))
		}
		for len(body) > 0 {
			n := min(chunk, len(body))
			_, _ = out.Write(body[:n])
			body = body[n:]
		}
	})
}

// fetch reads a response to its end, and returns its status, what arrived, and how
// it ended: nil for a clean end.
func (f foyer) fetch(t *testing.T) (int, []byte, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.url+"/k8s/api/v1/configmaps", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := f.client.RoundTrip(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	return resp.StatusCode, got, err
}

// The bound counts decoded bytes, exactly: a response of up to the limit passes
// whole; one past it gets a 502 when its length is known in advance, and is cut
// short otherwise, after exactly the limit and never more.
func TestResponseBytesBoundary(t *testing.T) {
	const limit = 1000
	for upName, upstream := range protocols {
		for _, compress := range []bool{false, true} {
			for _, size := range []int{limit - 1, limit, limit + 1} {
				name := upName + " " + strconv.Itoa(size) + " bytes"
				if compress {
					name += " gzip"
				}
				t.Run(name, func(t *testing.T) {
					m := metrics.New()
					f := newFoyerWith(t, sizedAPIServer(t, upstream, size, 64, compress), credentials{token: userToken},
						frontOptions{config: func(c *testConfig) { c.MaxResponseBytes, c.Metrics = limit, m }})
					code, got, err := f.fetch(t)
					switch {
					case size <= limit:
						if code != http.StatusOK || len(got) != size || err != nil {
							t.Fatalf("%d, %d bytes, %v; want the whole %d bytes", code, len(got), err, size)
						}
						assertMetric(t, m, `krm_foyer_bound_reached_total{bound="response_bytes"} 0`)
					case !compress:
						if code != http.StatusBadGateway {
							t.Fatalf("%d, %d bytes; want 502 before anything was sent", code, len(got))
						}
						s := readStatus(t, answer{StatusCode: code, Header: http.Header{"Content-Type": {"application/json"},
							"Krm-Foyer-Interruption": {"ResponseTooLarge"}}, Body: string(got)})
						if s.Reason != "ResponseTooLarge" || s.Details == nil || s.Details.Causes[0].Field != metrics.BoundResponseBytes ||
							s.Details.Causes[0].Message != strconv.Itoa(limit) {
							t.Errorf("Status %+v does not name the bound and its limit", s)
						}
						assertMetric(t, m, `krm_foyer_bound_reached_total{bound="response_bytes"} 1`)
					default:
						if err == nil || len(got) != limit {
							t.Fatalf("%d bytes, %v; want exactly %d bytes and then an abort", len(got), err, limit)
						}
						assertMetric(t, m, `krm_foyer_bound_reached_total{bound="response_bytes"} 1`)
						assertMetric(t, m, `krm_foyer_responses_cut_short_total{cause="response_bytes"} 1`)
					}
				})
			}
		}
	}
}

// A small compressed body that expands far past the bound is stopped at the bound:
// the browser gets no more than the limit, and the request to the API server is
// cancelled while it is still sending.
func TestACompressedBombIsStopped(t *testing.T) {
	const limit = 1 << 20
	cancelled := make(chan struct{})
	api := newAPIServerWith(t, true, func(w http.ResponseWriter, r *http.Request) {
		defer close(cancelled)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		zeros := make([]byte, 64<<10)
		for r.Context().Err() == nil { // a thousandfold, for as long as anyone reads
			if _, err := gz.Write(zeros); err != nil {
				return
			}
		}
	})
	f := newFoyerWith(t, api, credentials{token: userToken}, frontOptions{config: func(c *testConfig) { c.MaxResponseBytes = limit }})
	_, got, err := f.fetch(t)
	if err == nil || len(got) > limit {
		t.Fatalf("%d bytes, %v; want at most %d and then an abort", len(got), err, limit)
	}
	closedWithin(t, cancelled, "the request to the API server was not cancelled")
}

// A HEAD response has no body; its Content-Length describes the GET, however large.
func TestHeadIsNotTooLarge(t *testing.T) {
	api := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "1000000")
	})
	f := newFoyerWith(t, api, credentials{token: userToken}, frontOptions{config: func(c *testConfig) { c.MaxResponseBytes = 10 }})
	if a := f.request(t, http.MethodHead, "/k8s/api/v1/configmaps", nil, nil); a.StatusCode != http.StatusOK {
		t.Fatalf("HEAD: %d", a.StatusCode)
	}
}

// The property, from the browser's side, over random sizes around the limit, random
// chunking and both encodings: a response that ends cleanly is complete and no larger
// than the limit; one larger than the limit never ends cleanly; and no response ever
// delivers more than the limit.
func TestResponseBytesProperty(t *testing.T) {
	const limit = 512
	for seed := range uint64(60) {
		rng := rand.New(rand.NewPCG(seed, 1)) //nolint:gosec // reproducible sizes, not secrets
		size := limit - 20 + rng.IntN(40)
		chunk := 1 + rng.IntN(200)
		compress := rng.IntN(2) == 0
		f := newFoyerWith(t, sizedAPIServer(t, rng.IntN(2) == 0, size, chunk, compress), credentials{token: userToken},
			frontOptions{config: func(c *testConfig) { c.MaxResponseBytes = limit }})
		code, got, err := f.fetch(t)
		desc := "seed " + strconv.FormatUint(seed, 10) + ": " + strconv.Itoa(size) + " bytes in chunks of " + strconv.Itoa(chunk)
		if code == http.StatusOK && len(got) > limit {
			t.Fatalf("%s: delivered %d bytes, past the limit", desc, len(got))
		}
		if code == http.StatusOK && err == nil && len(got) != size {
			t.Fatalf("%s: ended cleanly after %d bytes; an incomplete response passed for complete", desc, len(got))
		}
		if size > limit && code == http.StatusOK && err == nil {
			t.Fatalf("%s: a response past the limit ended cleanly", desc)
		}
		if size <= limit && (code != http.StatusOK || err != nil) {
			t.Fatalf("%s: a response within the limit was refused: %d, %v", desc, code, err)
		}
	}
}
