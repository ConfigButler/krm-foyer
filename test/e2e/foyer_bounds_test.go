//go:build e2e

package e2e

import (
	"bufio"
	"net/http"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// metric reads one sample from the main krm-foyer's metrics: series is the name with
// its labels as scraped, such as krm_foyer_interruptions_total{reason="BadRequest"}.
// A series not yet recorded is 0.
func (f *fixture) metric(series string) float64 { return f.metricOf("krm-foyer-metrics", series) }

// briefMetric reads one sample from the brief krm-foyer's metrics.
func (f *fixture) briefMetric(series string) float64 {
	return f.metricOf("krm-foyer-brief-metrics", series)
}

// metricOf reads the metrics of the Service named service through the API server's
// service proxy, as admin: the way a monitoring system in the cluster reaches them.
func (f *fixture) metricOf(service, series string) float64 {
	GinkgoHelper()
	out := f.kubectl("get", "--raw", "/api/v1/namespaces/krm-foyer/services/http:"+service+":metrics/proxy/metrics")
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		if value, ok := strings.CutPrefix(scanner.Text(), series+" "); ok {
			v, err := strconv.ParseFloat(value, 64)
			Expect(err).NotTo(HaveOccurred(), series)
			return v
		}
	}
	return 0
}

// assertCancelledUpstream checks that the API server completed the request marked
// marker within a minute of receiving it. The watches here ask for timeoutSeconds=600,
// so only a cancellation from krm-foyer can complete them that soon.
func assertCancelledUpstream(ctx SpecContext, marker, username string) {
	GinkgoHelper()
	var events []auditEvent
	eventually(ctx, func() []auditEvent { events = fx.audited(marker); return events }).Should(HaveLen(1))
	Expect(events[0].User.Username).To(Equal(username))
	Expect(events[0].StageTimestamp.Sub(events[0].RequestReceivedTimestamp)).To(BeNumerically("<", time.Minute),
		"the API server kept the watch open: the cancellation did not reach it")
}

const cutForSessionEnded = `krm_foyer_responses_cut_short_total{cause="session_ended"}`

// The bounds of docs/bounds.md, and the metrics that show how close real traffic
// comes to them.
var _ = Describe("krm-foyer's bounds", Label("foyer"), func() {
	It("serves its metrics apart from the origin, and counts its interruptions there", func(ctx SpecContext) {
		series := `krm_foyer_interruptions_total{reason="BadRequest"}`
		before := fx.metric(series)
		a := signIn(ctx, alice).viaFoyer(ctx, http.MethodGet, "/api/v1//namespaces", nil, nil)
		Expect(a.Code).To(Equal(http.StatusBadRequest), "%s", a.Body)
		Expect(fx.metric(series)).To(Equal(before + 1))

		By("and nowhere on the origin")
		for _, path := range []string{"/metrics", "/k8s/metrics"} {
			a := fx.browser().do(ctx, http.MethodGet, path, nil, nil)
			Expect(string(a.Body)).NotTo(ContainSubstring("krm_foyer_"), path)
		}
	})

	// The "stream open across logout and expiry" row of the session lifecycle table
	// (docs/design.md), for native watches through /k8s.
	Context("ends every open response with its session", func() {
		It("aborts a native watch when its session logs out, and cancels it at the API server", func(ctx SpecContext) {
			ns := fx.namespace()
			fx.grant(ns, aliceK8sName, "configmaps", "get", "list", "watch")
			alice := signIn(ctx, alice)
			before := fx.metric(cutForSessionEnded)
			w := alice.watch(ctx, "/api/v1/namespaces/"+ns+"/configmaps?watch=1&timeoutSeconds=600")
			Expect(w.resp.StatusCode).To(Equal(http.StatusOK))
			// Every namespace holds kube-root-ca.crt, so a watch starts with an event.
			Expect(w.event()).To(ContainSubstring(`"type":"ADDED"`))

			out := alice.b.do(ctx, http.MethodPost, "/auth/logout", nil, alice.proof())
			Expect(out.Code).To(Equal(http.StatusNoContent), "%s", out.Body)
			loggedOut := time.Now()

			By("within the session-check interval: 5 seconds by default, 10 at worst")
			at, err := w.end(30 * time.Second)
			Expect(err).To(HaveOccurred(), "the watch ended cleanly; a response cut short is aborted")
			Expect(at.Sub(loggedOut)).To(BeNumerically("<", 12*time.Second))
			assertCancelledUpstream(ctx, w.Marker, aliceK8sName)
			Expect(fx.metric(cutForSessionEnded)).To(Equal(before + 1))

			By("and the watch, resumed, gets the 401 interruption")
			again := alice.viaFoyer(ctx, http.MethodGet, "/api/v1/namespaces/"+ns+"/configmaps?watch=1&timeoutSeconds=1", nil, nil)
			Expect(again.Code).To(Equal(http.StatusUnauthorized), "%s", again.Body)
			Expect(again.Header.Get("Krm-Foyer-Interruption")).To(Equal("Unauthorized"))
		})

		It("aborts a native watch when its session expires, and cancels it at the API server", func(ctx SpecContext) {
			ns := fx.namespace()
			fx.grant(ns, aliceK8sName, "configmaps", "get", "list", "watch")
			// The brief instance ends a session 45 seconds after login, checks open
			// responses every second, and cuts one short after 20 seconds.
			beforeLogin := time.Now()
			alice := signInBrief(ctx, alice)
			afterLogin := time.Now()
			before := fx.briefMetric(cutForSessionEnded)

			By("opening the watch late in the session, so nothing but its end can close it")
			select {
			case <-time.After(time.Until(beforeLogin.Add(32 * time.Second))):
			case <-ctx.Done():
				Fail("interrupted")
			}
			w := alice.watch(ctx, "/api/v1/namespaces/"+ns+"/configmaps?watch=1&timeoutSeconds=600")
			Expect(w.resp.StatusCode).To(Equal(http.StatusOK))
			// Every namespace holds kube-root-ca.crt, so a watch starts with an event.
			Expect(w.event()).To(ContainSubstring(`"type":"ADDED"`))

			at, err := w.end(45 * time.Second)
			Expect(err).To(HaveOccurred(), "the watch ended cleanly; a response cut short is aborted")
			Expect(at).To(BeTemporally(">=", beforeLogin.Add(45*time.Second)), "the watch ended before its session")
			Expect(at).To(BeTemporally("<", afterLogin.Add(45*time.Second+2*time.Second+3*time.Second)),
				"the watch outlived its session by more than two check intervals")
			assertCancelledUpstream(ctx, w.Marker, aliceK8sName)
			Expect(fx.briefMetric(cutForSessionEnded)).To(Equal(before + 1))

			By("and the watch, resumed, gets the 401 interruption")
			again := alice.viaFoyer(ctx, http.MethodGet, "/api/v1/namespaces/"+ns+"/configmaps?watch=1&timeoutSeconds=1", nil, nil)
			Expect(again.Code).To(Equal(http.StatusUnauthorized), "%s", again.Body)
		})
	})

	It("aborts a response open longer than its duration, and cancels it at the API server", func(ctx SpecContext) {
		ns := fx.namespace()
		fx.grant(ns, aliceK8sName, "configmaps", "get", "list", "watch")
		alice := signInBrief(ctx, alice)
		reached := `krm_foyer_bound_reached_total{bound="response_duration"}`
		cut := `krm_foyer_responses_cut_short_total{cause="response_duration"}`
		beforeReached, beforeCut := fx.briefMetric(reached), fx.briefMetric(cut)
		Expect(fx.briefMetric(`krm_foyer_bound_limit{bound="response_duration"}`)).To(Equal(20.0))

		// The watch asks the API server for ten minutes; the brief instance allows 20
		// seconds.
		opened := time.Now()
		w := alice.watch(ctx, "/api/v1/namespaces/"+ns+"/configmaps?watch=1&timeoutSeconds=600")
		Expect(w.resp.StatusCode).To(Equal(http.StatusOK))
		Expect(w.event()).To(ContainSubstring(`"type":"ADDED"`))
		at, err := w.end(40 * time.Second)
		Expect(err).To(HaveOccurred(), "the watch ended cleanly; a response cut short is aborted")
		Expect(at.Sub(opened)).To(BeNumerically(">=", 20*time.Second), "cut short before its duration")
		Expect(at.Sub(opened)).To(BeNumerically("<", 25*time.Second))
		assertCancelledUpstream(ctx, w.Marker, aliceK8sName)
		Expect(fx.briefMetric(reached)).To(Equal(beforeReached + 1))
		Expect(fx.briefMetric(cut)).To(Equal(beforeCut + 1))
	})

	It("refuses a session's request past its concurrency limit with its own 429, before it reaches the API server", func(ctx SpecContext) {
		ns := fx.namespace()
		fx.grant(ns, aliceK8sName, "configmaps", "get", "list", "watch")
		alice := signInBrief(ctx, alice)
		reached := `krm_foyer_bound_reached_total{bound="session_concurrent_requests"}`
		before := fx.briefMetric(reached)
		Expect(fx.briefMetric(`krm_foyer_bound_limit{bound="session_concurrent_requests"}`)).To(Equal(2.0))

		// The brief instance lets a session have two requests in flight.
		path := "/api/v1/namespaces/" + ns + "/configmaps"
		watches := []*stream{
			alice.watch(ctx, path+"?watch=1&timeoutSeconds=600"),
			alice.watch(ctx, path+"?watch=1&timeoutSeconds=600"),
		}
		for _, w := range watches {
			Expect(w.resp.StatusCode).To(Equal(http.StatusOK))
		}
		refused := alice.viaFoyer(ctx, http.MethodGet, path, nil, nil)
		Expect(refused.Code).To(Equal(http.StatusTooManyRequests), "%s", refused.Body)
		Expect(refused.Header.Get("Krm-Foyer-Interruption")).To(Equal("TooManyConcurrentRequests"))
		Expect(refused.Header.Values("Retry-After")).To(BeEmpty())
		Expect(refused.status().Reason).To(Equal("TooManyConcurrentRequests"))
		assertNeverAudited(ctx, refused.Marker)
		Expect(fx.briefMetric(reached)).To(Equal(before + 1))

		By("and lets one through again once a watch has ended")
		_ = watches[0].resp.Body.Close()
		eventually(ctx, func() int { return alice.viaFoyer(ctx, http.MethodGet, path, nil, nil).Code }).
			Should(Equal(http.StatusOK))
	})

	It("refuses a session sending faster than its rate with its own 429 and Retry-After, before it reaches the API server", func(ctx SpecContext) {
		ns := fx.namespace()
		fx.grant(ns, aliceK8sName, "configmaps", "get", "list")
		alice := signInBrief(ctx, alice)
		reached := `krm_foyer_bound_reached_total{bound="session_request_burst"}`
		before := fx.briefMetric(reached)
		Expect(fx.briefMetric(`krm_foyer_bound_limit{bound="session_request_burst"}`)).To(Equal(10.0))
		Expect(fx.briefMetric(`krm_foyer_bound_limit{bound="session_request_rate"}`)).To(Equal(1.0))

		// The brief instance lets a session send ten requests at once, then one a
		// second. Twenty in a row take well under ten seconds.
		path := "/api/v1/namespaces/" + ns + "/configmaps"
		var refused *answer
		for i := range 20 {
			a := alice.viaFoyer(ctx, http.MethodGet, path, nil, nil)
			if i < 10 {
				Expect(a.Code).To(Equal(http.StatusOK), "request %d of the burst: %s", i+1, a.Body)
			} else if a.Code == http.StatusTooManyRequests && refused == nil {
				refused = &a
			}
		}
		Expect(refused).NotTo(BeNil(), "twenty requests in a row were all let through")
		Expect(refused.Header.Get("Krm-Foyer-Interruption")).To(Equal("RequestRateExceeded"))
		Expect(refused.Header.Get("Retry-After")).To(Equal("1"))
		Expect(refused.status().Reason).To(Equal("RequestRateExceeded"))
		assertNeverAudited(ctx, refused.Marker)
		Expect(fx.briefMetric(reached)).To(BeNumerically(">", before))

		By("and lets the session through again after Retry-After")
		time.Sleep(time.Second)
		Expect(alice.viaFoyer(ctx, http.MethodGet, path, nil, nil).Code).To(Equal(http.StatusOK))
	})

	It("cuts short a response past its byte bound, counting what the API server compressed as it decodes", func(ctx SpecContext) {
		const limit = 128 << 10 // the brief instance's
		ns := fx.namespace()
		fx.grant(ns, aliceK8sName, "configmaps", "get", "list")
		// A list of some 200 KiB of one letter: past the API server's threshold for
		// compressing (128 KiB), and a few hundred bytes once compressed. Two keys,
		// since one argument may hold at most 128 KiB.
		letters := strings.Repeat("a", 100<<10)
		fx.kubectl("-n", ns, "create", "configmap", "big", "--from-literal=a="+letters, "--from-literal=b="+letters)
		alice := signInBrief(ctx, alice)
		path := "/api/v1/namespaces/" + ns + "/configmaps"
		Expect(fx.briefMetric(`krm_foyer_bound_limit{bound="response_bytes"}`)).To(Equal(float64(limit)))

		By("the API server sends it compressed, far below the bound")
		compressed := fx.directWith(ctx, alice.token, http.MethodGet, path, nil, http.Header{"Accept-Encoding": {"gzip"}})
		Expect(compressed.Code).To(Equal(http.StatusOK))
		Expect(compressed.Header.Get("Content-Encoding")).To(Equal("gzip"))
		Expect(len(compressed.Body)).To(BeNumerically("<", limit))

		By("and krm-foyer counts it decoded, so it never passes as a complete answer")
		before := fx.briefMetric(`krm_foyer_bound_reached_total{bound="response_bytes"}`)
		s := alice.watch(ctx, path)
		if s.resp.StatusCode == http.StatusBadGateway {
			Expect(s.resp.Header.Get("Krm-Foyer-Interruption")).To(Equal("ResponseTooLarge"))
			_, _ = s.end(30 * time.Second)
		} else {
			Expect(s.resp.StatusCode).To(Equal(http.StatusOK))
			_, err := s.end(30 * time.Second)
			Expect(err).To(HaveOccurred(), "a response past the byte bound ended cleanly")
			Expect(s.read.Len()).To(BeNumerically("<=", limit))
		}
		Expect(fx.briefMetric(`krm_foyer_bound_reached_total{bound="response_bytes"}`)).To(Equal(before + 1))

		By("while a small answer passes whole")
		Expect(alice.viaFoyer(ctx, http.MethodGet, path+"/kube-root-ca.crt", nil, nil).Code).To(Equal(http.StatusOK))
	})
})
