//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Parked: a rollout that swallows no connection. docs/investigations/rollout-connections.md
// records what was measured and why this is not pursued yet; the roadmap keeps the item.
//
// A rollout replaces krm-foyer's one pod (the chart's Recreate), so between the two pods
// a new connection is refused, at once. What it should not do is leave a connection
// unanswered, during the rollout or after it. Today it does, now and then, at the
// moment the old pod's address goes away; the spec below shows it, and stays pending
// until a rollout without that is designed. The suite's own replacements forget the
// gone pod's connection tracking (fixture.forgetGonePods), which is what kept the
// rehearsal's 1,800 connections at once from timing out.
var _ = Describe("krm-foyer's rollout", Label("foyer"), func() {
	PIt("refuses rather than swallows a connection while its pod is replaced, and leaves none to be swallowed afterwards", func(ctx SpecContext) {
		// From a clean slate: nothing left over from pods replaced before.
		fx.forgetGonePods()
		// Two at a time, each a new connection that the client closes as soon as it is
		// answered, until told to stop. Whether a connection is answered is the
		// question; a request on it would add the server's own closing to the node's
		// connection tracking.
		var (
			mu       sync.Mutex
			failures []string
			answered int
			refused  int
			// A refusal can follow a retransmitted SYN: an answer all the same.
			slowestRefusal time.Duration
		)
		probing, stop := context.WithCancel(ctx)
		var wg sync.WaitGroup
		for range 2 {
			wg.Go(func() {
				for probing.Err() == nil {
					started := time.Now()
					c, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", fx.foyerAddr)
					mu.Lock()
					switch {
					case errors.Is(err, syscall.ECONNREFUSED):
						refused++
						slowestRefusal = max(slowestRefusal, time.Since(started))
					case err != nil:
						failures = append(failures, fmt.Sprintf("%s: %v", started.Format("15:04:05.000"), err))
					default:
						answered++
						_ = c.Close()
					}
					mu.Unlock()
					time.Sleep(20 * time.Millisecond)
				}
			})
		}

		pods := func() string {
			return fx.kubectl("-n", fx.foyerNamespace, "get", "pods", "-l", "app.kubernetes.io/instance=krm-foyer",
				"-o", `jsonpath={range .items[*]}{.metadata.name}={.status.phase}{" "}{end}`)
		}
		old := pods()
		fx.kubectl("-n", fx.foyerNamespace, "rollout", "restart", "deployment/krm-foyer")
		fx.kubectl("-n", fx.foyerNamespace, "rollout", "status", "deployment/krm-foyer", "--timeout=120s")
		eventually(ctx, func() error {
			now := strings.Fields(pods())
			if len(now) != 1 || strings.Contains(old, now[0]) {
				return fmt.Errorf("pods %q, before %q", now, old)
			}
			return nil
		}).WithTimeout(2 * time.Minute).Should(Succeed())
		// And a little past the old pod's end, when an address that is gone would show.
		time.Sleep(3 * time.Second)
		stop()
		wg.Wait()
		fx.forgetGonePods()
		Expect(answered).To(BeNumerically(">", 100), "too few probes to say anything")
		Expect(failures).To(BeEmpty(), "%d connections answered, %d refused", answered, refused)
		AddReportEntry("rollout", fmt.Sprintf("%d connections answered, %d refused while no pod was ready, the slowest refusal after %v",
			answered, refused, slowestRefusal.Round(time.Millisecond)))

		By("then opening 1,800 connections at once, as the rehearsal does: none is swallowed")
		var dialFailures []string
		var conns []net.Conn
		inParallel(1800, 1800, func(int) {
			c, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", fx.foyerAddr)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				dialFailures = append(dialFailures, err.Error())
				return
			}
			conns = append(conns, c)
		})
		for _, c := range conns {
			_ = c.Close()
		}
		Expect(dialFailures).To(BeEmpty())
	}, SpecTimeout(4*time.Minute))
})
