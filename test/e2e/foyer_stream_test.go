//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// noteStream is the /stream/v1 target for the notes of namespace ns.
func noteStream(ns string) string {
	return fx.foyerURL + "/stream/v1?" + url.Values{
		"group": {"hello.krm-foyer.example"}, "version": {"v1"}, "resource": {"notes"}, "namespace": {ns},
	}.Encode()
}

// createNotes creates notes with the given names in ns, as the fixture's admin.
func createNotes(ns string, names ...string) {
	GinkgoHelper()
	var manifest strings.Builder
	for _, name := range names {
		fmt.Fprintf(&manifest, "---\napiVersion: hello.krm-foyer.example/v1\nkind: Note\n"+
			"metadata: {name: %s, namespace: %s}\nspec: {text: note %s}\n", name, ns, name)
	}
	file := filepath.Join(GinkgoT().TempDir(), "notes.yaml")
	Expect(os.WriteFile(file, []byte(manifest.String()), 0o600)).To(Succeed())
	fx.kubectl("create", "-f", file)
}

// /stream: krm-stream's resource streams, each watch opened as the signed-in user.
// Kubernetes decides what a user may watch, exactly as for /k8s, and krm-foyer's
// service account is never used.
var _ = Describe("krm-foyer's streams", Label("foyer"), func() {
	It("streams what RBAC lets the user watch, live, as the user", func(ctx SpecContext) {
		ns := fx.namespace()
		fx.grant(ns, bobK8sName, "notes.hello.krm-foyer.example", "get", "list", "watch")
		createNotes(ns, "first", "second")
		bob := signIn(ctx, bob)

		s := bob.open(ctx, noteStream(ns))
		Expect(s.resp.StatusCode).To(Equal(http.StatusOK))
		Expect(s.resp.Header.Get("Content-Type")).To(HavePrefix("text/event-stream"))
		snapshot := s.until("synced")
		var names []string
		for _, e := range snapshot {
			if e.Type == "added" {
				names = append(names, e.Object.Metadata.Name)
			}
		}
		Expect(snapshot[0].Type).To(Equal("reset"))
		Expect(names).To(ConsistOf("first", "second"))

		By("following a change made elsewhere while the stream is open")
		fx.kubectl("-n", ns, "patch", "notes.hello.krm-foyer.example", "first", "--type=merge",
			"-p", `{"spec":{"text":"changed while bob watched"}}`)
		changed := s.until("modified")
		Expect(changed[len(changed)-1].Object.Metadata.Name).To(Equal("first"))
		Expect(changed[len(changed)-1].Object.Spec).To(HaveKeyWithValue("text", "changed while bob watched"))

		By("as bob, and nobody else, according to the API server")
		_ = s.resp.Body.Close()
		_, _ = s.end(10 * time.Second)
		var events []auditEvent
		eventually(ctx, func() []auditEvent { events = fx.audited(s.Marker); return events }).ShouldNot(BeEmpty())
		for _, e := range events {
			Expect(e.User.Username).To(Equal(bobK8sName), "%s %s", e.Verb, e.RequestURI)
			Expect(e.ImpersonatedUser).To(BeNil())
			Expect(e.RequestURI).To(HavePrefix("/apis/hello.krm-foyer.example/v1/namespaces/" + ns + "/notes?"))
		}
		Expect(events[0].Verb).To(Equal("watch"))
	})

	It("ends with Kubernetes' own refusal what RBAC does not let the user watch", func(ctx SpecContext) {
		ns := fx.namespace() // no grant for alice here
		alice := signIn(ctx, alice)
		s := alice.open(ctx, noteStream(ns))
		Expect(s.resp.StatusCode).To(Equal(http.StatusOK), "krm-stream answers a refusal as an event")
		e := s.krmEvent()
		Expect(e.Type).To(Equal("error"))
		Expect(e.Code).To(Equal("FORBIDDEN"))
		Expect(e.Terminal).To(BeTrue())
		Expect(e.Message).To(ContainSubstring(`User "` + aliceK8sName + `" cannot watch resource "notes"`))
		_, err := s.end(10 * time.Second)
		Expect(err).NotTo(HaveOccurred(), "a terminal event ends the stream cleanly")

		By("the API server refused alice herself: the service account never asked")
		var events []auditEvent
		eventually(ctx, func() []auditEvent { events = fx.audited(s.Marker); return events }).ShouldNot(BeEmpty())
		for _, ev := range events {
			Expect(ev.User.Username).To(Equal(aliceK8sName))
			Expect(ev.ResponseStatus.Code).To(Equal(http.StatusForbidden))
		}
	})

	It("opens no stream without a session, and asks the API server nothing", func(ctx SpecContext) {
		s := user{b: fx.browser()}.open(ctx, noteStream("hello"))
		Expect(s.resp.StatusCode).To(Equal(http.StatusUnauthorized))
		Expect(s.resp.Header.Get("Krm-Foyer-Interruption")).To(Equal("Unauthorized"))
		_, _ = s.end(10 * time.Second)
		// Give the audit log time to show a request, if one was made.
		time.Sleep(2 * time.Second)
		Expect(fx.audited(s.Marker)).To(BeEmpty())
	})

	// The "streams open at logout" and "token expiry" rows of the session lifecycle
	// table (docs/design.md), for krm-stream's streams.
	Context("end with their session", func() {
		It("aborts a stream when its session logs out, and cancels its watch at the API server", func(ctx SpecContext) {
			ns := fx.namespace()
			fx.grant(ns, aliceK8sName, "notes.hello.krm-foyer.example", "get", "list", "watch")
			createNotes(ns, "first")
			alice := signIn(ctx, alice)
			before := fx.metric(cutForSessionEnded)
			s := alice.open(ctx, noteStream(ns))
			s.until("synced")

			out := alice.b.do(ctx, http.MethodPost, "/auth/logout", nil, alice.proof())
			Expect(out.Code).To(Equal(http.StatusNoContent), "%s", out.Body)
			loggedOut := time.Now()

			By("within the session-check interval: 5 seconds by default, 10 at worst")
			at, err := s.end(30 * time.Second)
			Expect(err).To(HaveOccurred(), "the stream ended cleanly; a stream cut short is aborted")
			Expect(at.Sub(loggedOut)).To(BeNumerically("<", 12*time.Second))
			// The gateway names no timeoutSeconds, so the API server would keep the
			// watch open for 30 minutes or more: only krm-foyer can end it this soon.
			assertCancelledUpstream(ctx, s.Marker, aliceK8sName)
			Expect(fx.metric(cutForSessionEnded)).To(Equal(before + 1))

			By("and the stream, opened again, gets the 401 interruption")
			again := alice.open(ctx, noteStream(ns))
			Expect(again.resp.StatusCode).To(Equal(http.StatusUnauthorized))
			Expect(again.resp.Header.Get("Krm-Foyer-Interruption")).To(Equal("Unauthorized"))
			_, _ = again.end(10 * time.Second)
		})

		It("aborts a stream when its session expires, and cancels its watch at the API server", func(ctx SpecContext) {
			ns := fx.namespace()
			fx.grant(ns, aliceK8sName, "notes.hello.krm-foyer.example", "get", "list", "watch")
			createNotes(ns, "first")
			// The brief instance ends a session 45 seconds after login, checks open
			// responses every second, and cuts one short after 20 seconds.
			beforeLogin := time.Now()
			alice := signInBrief(ctx, alice)
			afterLogin := time.Now()
			before := fx.briefMetric(cutForSessionEnded)

			By("opening the stream late in the session, so nothing but its end can close it")
			select {
			case <-time.After(time.Until(beforeLogin.Add(32 * time.Second))):
			case <-ctx.Done():
				Fail("interrupted")
			}
			s := alice.open(ctx, noteStream(ns))
			s.until("synced")

			at, err := s.end(45 * time.Second)
			Expect(err).To(HaveOccurred(), "the stream ended cleanly; a stream cut short is aborted")
			Expect(at).To(BeTemporally(">=", beforeLogin.Add(45*time.Second)), "the stream ended before its session")
			Expect(at).To(BeTemporally("<", afterLogin.Add(45*time.Second+2*time.Second+3*time.Second)),
				"the stream outlived its session by more than two check intervals")
			assertCancelledUpstream(ctx, s.Marker, aliceK8sName)
			Expect(fx.briefMetric(cutForSessionEnded)).To(Equal(before + 1))

			By("and the stream, opened again, gets the 401 interruption")
			again := alice.open(ctx, noteStream(ns))
			Expect(again.resp.StatusCode).To(Equal(http.StatusUnauthorized))
			_, _ = again.end(10 * time.Second)
		})
	})

	It("bounds a session's streams apart from its requests", func(ctx SpecContext) {
		ns := fx.namespace()
		fx.grant(ns, aliceK8sName, "notes.hello.krm-foyer.example", "get", "list", "watch")
		fx.grant(ns, aliceK8sName, "configmaps", "get", "list", "watch")
		createNotes(ns, "first")
		// The brief instance allows a session two streams and two requests at once.
		alice := signInBrief(ctx, alice)
		before := fx.briefMetric("krm_foyer_upstream_watches_open")
		for range 2 {
			alice.open(ctx, noteStream(ns)).until("synced")
		}
		Expect(fx.briefMetric("krm_foyer_upstream_watches_open")).To(BeNumerically(">=", before+2))

		By("its requests are not counted with its streams")
		for range 2 {
			w := alice.watch(ctx, "/api/v1/namespaces/"+ns+"/configmaps?watch=1&timeoutSeconds=60")
			Expect(w.resp.StatusCode).To(Equal(http.StatusOK))
		}

		By("a third stream is krm-foyer's 429, and never reaches the API server")
		third := alice.open(ctx, noteStream(ns))
		Expect(third.resp.StatusCode).To(Equal(http.StatusTooManyRequests))
		Expect(third.resp.Header.Get("Krm-Foyer-Interruption")).To(Equal("TooManyStreams"))
		_, _ = third.end(10 * time.Second)
		time.Sleep(2 * time.Second) // time for the audit log to show a request, if one was made
		Expect(fx.audited(third.Marker)).To(BeEmpty())
	})
})
