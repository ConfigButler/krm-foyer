//go:build e2e

package e2e

import (
	"bufio"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// apiServerWatches is how many watches of resource the API server is serving now,
// from its own metrics: what the API server holds, counted apart from krm-foyer.
func (f *fixture) apiServerWatches(resource string) float64 {
	GinkgoHelper()
	var n float64
	sc := bufio.NewScanner(strings.NewReader(f.kubectl("get", "--raw", "/metrics")))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "apiserver_longrunning_requests{") ||
			!strings.Contains(line, `resource="`+resource+`"`) || !strings.Contains(line, `verb="WATCH"`) {
			continue
		}
		v, err := strconv.ParseFloat(line[strings.LastIndex(line, " ")+1:], 64)
		Expect(err).NotTo(HaveOccurred(), line)
		n += v
	}
	return n
}

// inParallel runs work for 0..n-1, at most width at once, and waits for all.
func inParallel(n, width int, work func(i int)) {
	var wg sync.WaitGroup
	slots := make(chan struct{}, width)
	for i := range n {
		slots <- struct{}{}
		wg.Go(func() {
			defer GinkgoRecover()
			defer func() { <-slots }()
			work(i)
		})
	}
	wg.Wait()
}

// quantile returns the q-quantile of ds, which it sorts.
func quantile(ds []time.Duration, q float64) time.Duration {
	slices.Sort(ds)
	return ds[min(len(ds)-1, int(q*float64(len(ds))))]
}

// The rehearsal: as many signed-in identities as the fixture has rehearsal users (200),
// each with streamsEach live streams, on one replica of krm-foyer: 1800 in all, near
// the 2000 a replica allows by default, so the cost of a stream stands out of the noise
// of the garbage collector. It measures what the per-replica bounds in docs/bounds.md
// otherwise only assume, and checks what must hold at that scale: one change reaching
// every stream, and every stream ended, and its watch released, when its session ends.
//
// It runs twice: with streams of a resource that is not shared (ConfigMaps), each a
// watch of its own user's, and with streams of one that is (notes), all reading one
// watch while the API server is asked about each user. The numbers are printed with
// the spec's report; docs/bounds.md records runs.
const streamsEach = 9

// rehearsal is what differs between the two runs.
type rehearsal struct {
	shared bool
	// resource is the resource streamed, as the API server's metrics name it.
	resource, kind string
	stream         func(ns string) string
	create         func(ns string)
	change         func(ns string)
	changed        func(krmEvent) map[string]any
}

var rehearsals = map[string]rehearsal{
	"with a watch of each user's own": {
		resource: "configmaps", kind: "configmaps", stream: configMapStream,
		create: func(ns string) { createConfigMaps(ns, "shared") },
		change: func(ns string) {
			fx.kubectl("-n", ns, "patch", "configmap", "shared", "--type=merge", "-p", `{"data":{"text":"seen by everyone"}}`)
		},
		changed: func(e krmEvent) map[string]any { return e.Object.Data },
	},
	"with one shared watch": {
		shared: true, resource: "notes", kind: "notes.hello.krm-foyer.example", stream: noteStream,
		create: func(ns string) { createNotes(ns, "shared") },
		change: func(ns string) {
			fx.kubectl("-n", ns, "patch", "notes.hello.krm-foyer.example", "shared", "--type=merge",
				"-p", `{"spec":{"text":"seen by everyone"}}`)
		},
		changed: func(e krmEvent) map[string]any { return e.Object.Spec },
	},
}

var _ = Describe("The rehearsal", Label("foyer", "rehearsal"), func() {
	for name, r := range rehearsals {
		It("holds 9 live streams for each of 200 identities on one replica, and lets them all go, "+name, func(ctx SpecContext) {
			rehearse(ctx, r, name)
		}, SpecTimeout(10*time.Minute))
	}
})

func rehearse(ctx SpecContext, r rehearsal, name string) {
	n := fx.rehearsalUsers
	total := n * streamsEach
	Expect(n).To(BeNumerically(">=", 200))
	ns := fx.namespace()
	r.create(ns)
	fx.kubectl("-n", ns, "create", "role", "reader", "--verb=get,list,watch", "--resource="+r.kind)
	args := []string{"-n", ns, "create", "rolebinding", "readers", "--role=reader"}
	for i := range n {
		args = append(args, fmt.Sprintf("--user=oidc:rehearsal-%03d@example.com", i+1))
	}
	fx.kubectl(args...)

	By("starting from a fresh krm-foyer, whose resident memory only grows while this runs")
	fx.kubectl("-n", fx.foyerNamespace, "rollout", "restart", "deployment/krm-foyer")
	fx.kubectl("-n", fx.foyerNamespace, "rollout", "status", "deployment/krm-foyer", "--timeout=120s")
	// The old pod stops listening as soon as it is told to stop, and may still be
	// routed to for a moment: wait until it is gone, and the new one answers.
	eventually(ctx, func() string {
		return fx.kubectl("-n", fx.foyerNamespace, "get", "pods", "-l", "app.kubernetes.io/instance=krm-foyer", "-o", "jsonpath={.items[*].status.phase}")
	}).WithTimeout(time.Minute).Should(Equal("Running"))
	eventually(ctx, func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fx.foyerURL+"/auth/session", nil)
		if err != nil {
			return err
		}
		resp, err := fx.browser().client.Do(req)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			return fmt.Errorf("/auth/session answered %d", resp.StatusCode)
		}
		return nil
	}).Should(Succeed())
	idleResident := fx.metric("process_resident_memory_bytes")
	baseStreams := fx.metric("krm_foyer_streams_open")
	baseUser, baseShared := fx.metric(userWatches), fx.metric(sharedWatches)
	baseAPIWatches := fx.apiServerWatches(r.resource)

	By(fmt.Sprintf("signing in %d identities through Dex's login form, 20 at a time", n))
	users := make([]user, n)
	start := time.Now()
	inParallel(n, 20, func(i int) { users[i] = signIn(ctx, fmt.Sprintf("rehearsal-%03d@example.com", i+1)) })
	signInAll := time.Since(start)

	By(fmt.Sprintf("opening %d streams for each, all at once, until every snapshot is complete", streamsEach))
	baseGoroutines := fx.metric("go_goroutines")
	signedInResident := fx.metric("process_resident_memory_bytes")
	baseAsked, baseReused, baseSubjects := fx.metric(checksAsked), fx.metric(checksReused), fx.metric(subjectsResolved)
	streams := make([]*stream, total)
	start = time.Now()
	inParallel(total, total, func(i int) {
		streams[i] = users[i/streamsEach].open(ctx, r.stream(ns))
		Expect(streams[i].resp.StatusCode).To(Equal(200))
		streams[i].until("synced")
	})
	openAll := time.Since(start)
	askedOpening, reusedOpening := fx.metric(checksAsked)-baseAsked, fx.metric(checksReused)-baseReused

	goroutines := fx.metric("go_goroutines")
	resident := fx.metric("process_resident_memory_bytes")
	Expect(fx.metric("krm_foyer_streams_open")).To(Equal(baseStreams + float64(total)))
	apiWatches := fx.apiServerWatches(r.resource)
	if r.shared {
		Expect(fx.metric(sharedWatches)).To(Equal(baseShared+1), "one shared watch for every stream")
		Expect(fx.metric(userWatches)).To(Equal(baseUser))
		Expect(fx.metric(sharedSubscriptions)).To(Equal(float64(total)))
		Expect(apiWatches-baseAPIWatches).To(BeNumerically("<=", 1),
			"the API server serves more than one new watch of notes for streams of one scope")
		Expect(fx.metric(subjectsResolved) - baseSubjects).To(Equal(float64(total)))
	} else {
		Expect(fx.metric(userWatches)).To(Equal(baseUser + float64(total)))
		Expect(apiWatches-baseAPIWatches).To(BeNumerically(">=", float64(total)),
			"the API server serves fewer watches than there are streams")
	}

	By("one change, and how long until every stream has it")
	// Measured from just before kubectl starts, so each time includes kubectl's own;
	// the spread from the first stream to the slowest is the cost of fanning out.
	arrivals := make([]time.Time, total)
	var wg sync.WaitGroup
	for i := range total {
		wg.Go(func() {
			defer GinkgoRecover()
			events := streams[i].until("modified")
			arrivals[i] = time.Now()
			Expect(r.changed(events[len(events)-1])).To(HaveKeyWithValue("text", "seen by everyone"))
		})
	}
	changed := time.Now()
	r.change(ns)
	wg.Wait()
	arrived := make([]time.Duration, total)
	for i, at := range arrivals {
		arrived[i] = at.Sub(changed)
	}
	first, p50, p99, slowest := quantile(arrived, 0), quantile(arrived, 0.5), quantile(arrived, 0.99), quantile(arrived, 1)
	Expect(slowest).To(BeNumerically("<", 10*time.Second), "a change took that long to reach every stream")

	// While shared streams stay open, each is authorized again every recheck
	// interval (30 seconds): the steady load on the API server sharing costs.
	var steady string
	if r.shared {
		By("holding every stream open past a recheck interval, counting the API server's reviews")
		asked, reused := fx.metric(checksAsked), fx.metric(checksReused)
		hold := 31 * time.Second
		select {
		case <-time.After(hold):
		case <-ctx.Done():
			Fail("interrupted")
		}
		askedHold, reusedHold := fx.metric(checksAsked)-asked, fx.metric(checksReused)-reused
		Expect(askedHold).To(BeNumerically(">", 0), "no stream was authorized again in a recheck interval")
		Expect(fx.metric("krm_foyer_streams_open")).To(Equal(baseStreams+float64(total)), "a stream ended while held")
		steady = fmt.Sprintf(`
  access checks, opening:         %.0f asked the API server (2 SubjectAccessReviews each), %.0f reused
  access checks, held %v:        %.0f asked (%.1f reviews a second), %.0f reused`,
			askedOpening, reusedOpening, hold, askedHold, 2*askedHold/hold.Seconds(), reusedHold)
	}

	By("every identity signing out: each stream ends within the session-check interval")
	ended := make([]time.Duration, total)
	inParallel(n, n, func(i int) {
		out := users[i].b.do(ctx, "POST", "/auth/logout", nil, users[i].proof())
		Expect(out.Code).To(Equal(204), "%s", out.Body)
		loggedOut := time.Now()
		for j := i * streamsEach; j < (i+1)*streamsEach; j++ {
			at, err := streams[j].end(30 * time.Second)
			Expect(err).To(HaveOccurred(), "a stream ended cleanly; a stream cut short is aborted")
			ended[j] = at.Sub(loggedOut)
		}
	})
	endSlowest := quantile(ended, 1)
	Expect(endSlowest).To(BeNumerically("<", 12*time.Second))

	By("and nothing is left behind: streams, watches and goroutines back where they were")
	eventually(ctx, func() float64 { return fx.metric("krm_foyer_streams_open") }).Should(Equal(baseStreams))
	eventually(ctx, func() float64 { return fx.metric(userWatches) }).Should(Equal(baseUser))
	eventually(ctx, func() float64 { return fx.metric(sharedWatches) }).Should(Equal(baseShared))
	eventually(ctx, func() float64 { return fx.apiServerWatches(r.resource) }).Should(BeNumerically("<=", baseAPIWatches))
	eventually(ctx, func() float64 { return fx.metric("go_goroutines") }).Should(BeNumerically("<", baseGoroutines+50))

	report := fmt.Sprintf(`%s: %d identities, %d streams each, %d streams on one replica
  sign-in, 20 at a time:          %v
  opening every stream, at once:  %v until every snapshot was complete
  one change reaching all:        first %v, p50 %v, p99 %v, slowest %v (from kubectl's start)
  logout to stream ended:         slowest %v
  per stream, while open:         %.1f goroutines, at most %.0f KiB resident
  resident memory:                %.0f MiB idle, %.0f MiB with %d sessions, %.0f MiB with every stream open
  watches at the API server:      %.0f more while open%s`,
		name, n, streamsEach, total, signInAll.Round(time.Millisecond), openAll.Round(time.Millisecond),
		first.Round(time.Millisecond), p50.Round(time.Millisecond), p99.Round(time.Millisecond), slowest.Round(time.Millisecond),
		endSlowest.Round(time.Millisecond),
		(goroutines-baseGoroutines)/float64(total), (resident-signedInResident)/float64(total)/1024,
		idleResident/(1<<20), signedInResident/(1<<20), n, resident/(1<<20),
		apiWatches-baseAPIWatches, steady)
	AddReportEntry("rehearsal", report, ReportEntryVisibilityAlways)
}
