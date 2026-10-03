//go:build e2e

package e2e

import (
	"net/http"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The series of krm-foyer's metrics that shared watches are read by.
const (
	userWatches         = `krm_foyer_upstream_watches_open{identity="user"}`
	sharedWatches       = `krm_foyer_upstream_watches_open{identity="shared"}`
	sharedSubscriptions = "krm_foyer_shared_subscriptions_open"
	checksAsked         = `krm_foyer_access_checks_total{result="allowed",source="api_server"}`
	checksReused        = `krm_foyer_access_checks_total{result="allowed",source="cache"}`
	subjectsResolved    = `krm_foyer_subject_reviews_total{result="resolved"}`
)

// Shared watches (docs/watches.md): notes are shared in the fixture, so every stream
// of one namespace's notes reads from one watch, opened with the shared-watch
// identity. The API server still decides who may read it, by a SubjectAccessReview
// for each user, and the audit log is the witness of who opened what.
var _ = Describe("krm-foyer's shared watches", Label("foyer"), func() {
	It("serves every user's streams of a scope from one watch, opened as the shared identity", func(ctx SpecContext) {
		ns := fx.namespace()
		fx.grant(ns, aliceK8sName, "notes.hello.krm-foyer.example", "get", "list", "watch")
		fx.grant(ns, bobK8sName, "notes.hello.krm-foyer.example", "get", "list", "watch")
		createNotes(ns, "first")
		alice, bob := signIn(ctx, alice), signIn(ctx, bob)
		before := map[string]float64{}
		for _, series := range []string{sharedWatches, userWatches, sharedSubscriptions, checksAsked, checksReused, subjectsResolved} {
			before[series] = fx.metric(series)
		}
		baseAPIWatches := fx.apiServerWatches("notes")

		By("three streams each for alice and bob")
		var streams []*stream
		for _, u := range []user{alice, alice, alice, bob, bob, bob} {
			s := u.open(ctx, noteStream(ns))
			Expect(s.resp.StatusCode).To(Equal(http.StatusOK))
			s.until("synced")
			streams = append(streams, s)
		}

		By("are one watch at the API server, and six subscriptions to it")
		Expect(fx.metric(sharedWatches)).To(Equal(before[sharedWatches] + 1))
		Expect(fx.metric(userWatches)).To(Equal(before[userWatches]))
		Expect(fx.metric(sharedSubscriptions)).To(Equal(before[sharedSubscriptions] + 6))
		Expect(fx.apiServerWatches("notes")).To(BeNumerically("<=", baseAPIWatches+1),
			"the API server serves more than one new watch of notes for six streams of one scope")

		By("each user found by the API server once a stream, and asked about about once, not once a stream")
		Expect(fx.metric(subjectsResolved)).To(Equal(before[subjectsResolved] + 6))
		Expect(fx.metric(checksReused) - before[checksReused]).To(BeNumerically(">=", 4))

		By("one change reaching every stream")
		fx.kubectl("-n", ns, "patch", "notes.hello.krm-foyer.example", "first", "--type=merge",
			"-p", `{"spec":{"text":"seen by alice and bob"}}`)
		for _, s := range streams {
			changed := s.until("modified")
			Expect(changed[len(changed)-1].Object.Spec).To(HaveKeyWithValue("text", "seen by alice and bob"))
		}

		By("the last stream out closing the watch")
		for _, s := range streams {
			_ = s.resp.Body.Close()
			_, _ = s.end(10 * time.Second)
		}
		eventually(ctx, func() float64 { return fx.metric(sharedWatches) }).Should(Equal(before[sharedWatches]))
		eventually(ctx, func() float64 { return fx.metric(sharedSubscriptions) }).Should(Equal(before[sharedSubscriptions]))

		By("opened as the shared identity, according to the API server: not the bait, not a user")
		var events []auditEvent
		eventually(ctx, func() []auditEvent {
			events = fx.audited("/apis/hello.krm-foyer.example/v1/namespaces/" + ns + "/notes?")
			return events
		}).ShouldNot(BeEmpty())
		watches := 0
		for _, e := range events {
			Expect(e.User.Username).To(Equal(fx.sharedAccount), "%s %s", e.Verb, e.RequestURI)
			Expect(e.ImpersonatedUser).To(BeNil())
			if e.Verb == "watch" {
				watches++
			}
		}
		Expect(watches).To(Equal(1), "watches of notes in the namespace, for six streams")
	})

	It("refuses a user RBAC does not allow, with nothing from the shared watch the identity may read", func(ctx SpecContext) {
		ns := fx.namespace()
		fx.grant(ns, aliceK8sName, "notes.hello.krm-foyer.example", "get", "list", "watch") // not bob
		createNotes(ns, "secret-to-bob")
		alice, bob := signIn(ctx, alice), signIn(ctx, bob)
		held := alice.open(ctx, noteStream(ns))
		held.until("synced")

		s := bob.open(ctx, noteStream(ns))
		Expect(s.resp.StatusCode).To(Equal(http.StatusOK), "krm-stream answers a refusal as an event")
		e := s.krmEvent()
		Expect(e.Type).To(Equal("error"))
		Expect(e.Code).To(Equal("FORBIDDEN"))
		Expect(e.Terminal).To(BeTrue())
		Expect(e.Message).To(ContainSubstring(bobK8sName))
		_, err := s.end(10 * time.Second)
		Expect(err).NotTo(HaveOccurred())
		Expect(s.read.String()).NotTo(ContainSubstring("secret-to-bob"))
		Expect(s.read.String()).NotTo(ContainSubstring(fx.sharedAccount))

		By("bob's token went with the review of who he is, and nothing else")
		var events []auditEvent
		eventually(ctx, func() []auditEvent { events = fx.audited(s.Marker); return events }).ShouldNot(BeEmpty())
		for _, ev := range events {
			Expect(ev.User.Username).To(Equal(bobK8sName))
			Expect(ev.RequestURI).To(Equal("/apis/authentication.k8s.io/v1/selfsubjectreviews"))
		}
	})

	It("closes only the streams of a session that logs out, and keeps the watch for the others", func(ctx SpecContext) {
		ns := fx.namespace()
		fx.grant(ns, aliceK8sName, "notes.hello.krm-foyer.example", "get", "list", "watch")
		fx.grant(ns, bobK8sName, "notes.hello.krm-foyer.example", "get", "list", "watch")
		createNotes(ns, "first")
		alice, bob := signIn(ctx, alice), signIn(ctx, bob)
		before := fx.metric(sharedWatches)
		staying := alice.open(ctx, noteStream(ns))
		staying.until("synced")
		leaving := bob.open(ctx, noteStream(ns))
		leaving.until("synced")

		out := bob.b.do(ctx, http.MethodPost, "/auth/logout", nil, bob.proof())
		Expect(out.Code).To(Equal(http.StatusNoContent), "%s", out.Body)
		_, err := leaving.end(30 * time.Second)
		Expect(err).To(HaveOccurred(), "the stream ended cleanly; a stream cut short is aborted")

		fx.kubectl("-n", ns, "patch", "notes.hello.krm-foyer.example", "first", "--type=merge",
			"-p", `{"spec":{"text":"after bob left"}}`)
		changed := staying.until("modified")
		Expect(changed[len(changed)-1].Object.Spec).To(HaveKeyWithValue("text", "after bob left"))
		Expect(fx.metric(sharedWatches)).To(Equal(before + 1))
	})

	It("ends a user's shared stream at the next recheck once RBAC no longer allows it, and only theirs", func(ctx SpecContext) {
		ns := fx.namespace()
		fx.grant(ns, aliceK8sName, "notes.hello.krm-foyer.example", "get", "list", "watch")
		revoke := fx.grant(ns, bobK8sName, "notes.hello.krm-foyer.example", "get", "list", "watch")
		createNotes(ns, "first")
		// The brief instance rechecks every 2 seconds, and reuses a decision for 1.
		alice, bob := signInBrief(ctx, alice), signInBrief(ctx, bob)
		staying := alice.open(ctx, noteStream(ns))
		staying.until("synced")
		revoked := bob.open(ctx, noteStream(ns))
		revoked.until("synced")

		revoke()
		at := time.Now()
		var last krmEvent
		for {
			e := revoked.krmEvent()
			if e.Type == "error" {
				last = e
				break
			}
		}
		Expect(last.Code).To(Equal("FORBIDDEN"))
		Expect(last.Terminal).To(BeTrue())
		Expect(time.Since(at)).To(BeNumerically("<", 10*time.Second), "the recheck interval, the decision's lifetime and the review")
		Expect(strings.Contains(revoked.read.String(), fx.sharedAccount)).To(BeFalse())

		fx.kubectl("-n", ns, "patch", "notes.hello.krm-foyer.example", "first", "--type=merge",
			"-p", `{"spec":{"text":"after bob's grant"}}`)
		changed := staying.until("modified")
		Expect(changed[len(changed)-1].Object.Spec).To(HaveKeyWithValue("text", "after bob's grant"))
	})
})
