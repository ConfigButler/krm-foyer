package proxy

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"

	"github.com/ConfigButler/krm-foyer/internal/interruption"
	"github.com/ConfigButler/krm-foyer/internal/metrics"
)

// errTooLarge ends the copy of a response that went past the byte limit, so that
// ReverseProxy aborts it.
var errTooLarge = errors.New("response past its byte limit")

// tooLarge answers a response whose length is known, before any of it is sent, to
// be past the limit. The request reached Kubernetes and may have taken effect.
func tooLarge(limit int64) *interruption.Interruption {
	l := strconv.FormatInt(limit, 10)
	return &interruption.Interruption{
		Status: http.StatusBadGateway, Reason: "ResponseTooLarge",
		Message: "the API server's answer is larger than the " + l + " bytes krm-foyer passes on; ask for less, with limit and continue",
		Causes:  []interruption.Cause{{Reason: "BoundReached", Field: metrics.BoundResponseBytes, Message: l}},
	}
}

// limitedBody passes on a response body up to limit decoded bytes. When byte
// limit+1 arrives it is not passed on: over is called, and the copy ends in an error,
// which aborts the response. Close records how much of the limit was used.
type limitedBody struct {
	body    io.ReadCloser
	limit   int64
	over    func()
	metrics *metrics.Metrics

	read  int64
	past  bool
	close sync.Once
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.past {
		return 0, errTooLarge
	}
	n, err := b.body.Read(p)
	b.read += int64(n)
	if b.read > b.limit {
		n -= int(b.read - b.limit)
		b.past = true
		b.over()
		return n, errTooLarge
	}
	return n, err
}

func (b *limitedBody) Close() error {
	b.close.Do(func() { b.metrics.BoundUsage(metrics.BoundResponseBytes, float64(b.read)/float64(b.limit)) })
	return b.body.Close()
}
