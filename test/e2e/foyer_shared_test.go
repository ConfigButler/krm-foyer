//go:build e2e

package e2e

import (
	"context"
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

	// RoleBindings are shared in one namespace alone (foyer-values.yaml), with a Role
	// there: a ClusterRole for them would let the identity read every grant in the
	// cluster. What must hold is that nothing of another namespace comes out: not
	// through the shared watch, which may not read it, and not by the shared identity
	// asking on anyone's behalf. Elsewhere a stream is the user's own, and the API
	// server answers it as the user.
	It("shares a namespaced resource in its namespace alone, and shows nothing of any other", func(ctx SpecContext) {
		shared, other := fx.sharedNamespace, fx.namespace()
		id := randomID()
		visible, secret := "e2e-visible-"+id, "e2e-secret-"+id
		fx.kubectl("-n", shared, "create", "rolebinding", visible, "--clusterrole=view", "--user=nobody-"+id)
		DeferCleanup(func() { fx.kubectl("-n", shared, "delete", "rolebinding", visible, "--ignore-not-found") })
		fx.kubectl("-n", other, "create", "rolebinding", secret, "--clusterrole=view", "--user=nobody-"+id)
		DeferCleanup(fx.grant(shared, aliceK8sName, "rolebindings.rbac.authorization.k8s.io", "get", "list", "watch"))
		fx.grant(other, bobK8sName, "rolebindings.rbac.authorization.k8s.io", "get", "list", "watch") // not alice
		alice, bob := signIn(ctx, alice), signIn(ctx, bob)
		beforeShared, beforeUser := fx.metric(sharedWatches), fx.metric(userWatches)

		By("the shared namespace: served from the shared watch, with that namespace's grants alone")
		inShared := alice.open(ctx, roleBindingStream(shared))
		Expect(inShared.resp.StatusCode).To(Equal(http.StatusOK))
		names := map[string]bool{}
		for _, e := range inShared.until("synced") {
			names[e.Object.Metadata.Name] = true
		}
		Expect(names).To(HaveKey(visible))
		Expect(names).NotTo(HaveKey(secret))
		Expect(fx.metric(sharedWatches)).To(Equal(beforeShared + 1))
		Expect(fx.metric(userWatches)).To(Equal(beforeUser))

		By("another namespace, which alice may not read: refused as alice, with nothing of it")
		elsewhere := alice.open(ctx, roleBindingStream(other))
		e := elsewhere.krmEvent()
		Expect(e.Type).To(Equal("error"))
		Expect(e.Code).To(Equal("FORBIDDEN"))
		Expect(e.Terminal).To(BeTrue())
		_, err := elsewhere.end(10 * time.Second)
		Expect(err).NotTo(HaveOccurred())
		Expect(elsewhere.read.String()).NotTo(ContainSubstring(secret))
		Expect(elsewhere.read.String()).NotTo(ContainSubstring(fx.sharedAccount))
		expectAskedOnlyAs(ctx, elsewhere, aliceK8sName, "/apis/rbac.authorization.k8s.io/v1/namespaces/"+other+"/rolebindings")

		By("every namespace at once: refused as alice, with nothing of any namespace")
		everywhere := alice.open(ctx, roleBindingStream(""))
		e = everywhere.krmEvent()
		Expect(e.Type).To(Equal("error"))
		Expect(e.Code).To(Equal("FORBIDDEN"))
		Expect(e.Terminal).To(BeTrue())
		_, err = everywhere.end(10 * time.Second)
		Expect(err).NotTo(HaveOccurred())
		Expect(everywhere.read.String()).NotTo(ContainSubstring(secret))
		Expect(everywhere.read.String()).NotTo(ContainSubstring(visible))
		expectAskedOnlyAs(ctx, everywhere, aliceK8sName, "/apis/rbac.authorization.k8s.io/v1/rolebindings")

		By("another namespace, which bob may read: his own watch, with that namespace's grants alone")
		bobs := bob.open(ctx, roleBindingStream(other))
		names = map[string]bool{}
		for _, e := range bobs.until("synced") {
			names[e.Object.Metadata.Name] = true
		}
		Expect(names).To(HaveKey(secret))
		Expect(names).NotTo(HaveKey(visible))
		Expect(fx.metric(sharedWatches)).To(Equal(beforeShared+1), "bob's stream must not open a shared watch")
		Expect(fx.metric(userWatches)).To(Equal(beforeUser + 1))
		_ = bobs.resp.Body.Close()
		_, _ = bobs.end(10 * time.Second)
		expectAskedOnlyAs(ctx, bobs, bobK8sName, "/apis/rbac.authorization.k8s.io/v1/namespaces/"+other+"/rolebindings")

		By("the API server: the shared identity may not read RoleBindings outside its namespace")
		for _, ns := range []string{other, ""} {
			for _, verb := range []string{"list", "watch"} {
				Expect(fx.accessReview(fx.sharedAccount, verb, "rbac.authorization.k8s.io", "rolebindings", ns)).
					To(BeFalse(), "%s may %s rolebindings in %q", fx.sharedAccount, verb, ns)
			}
		}
		Expect(fx.accessReview(fx.sharedAccount, "watch", "rbac.authorization.k8s.io", "rolebindings", shared)).To(BeTrue())

		By("the audit log: the shared identity read RoleBindings in the shared namespace, and nowhere else")
		_ = inShared.resp.Body.Close()
		_, _ = inShared.end(10 * time.Second)
		eventually(ctx, func() float64 { return fx.metric(sharedWatches) }).Should(Equal(beforeShared))
		var reads []auditEvent
		eventually(ctx, func() []auditEvent {
			reads = nil
			for _, ev := range fx.audited(fx.sharedAccount) {
				if ev.User.Username == fx.sharedAccount && strings.Contains(ev.RequestURI, "/rolebindings") {
					reads = append(reads, ev)
				}
			}
			return reads
		}).ShouldNot(BeEmpty())
		for _, ev := range reads {
			Expect(ev.RequestURI).To(HavePrefix("/apis/rbac.authorization.k8s.io/v1/namespaces/"+shared+"/rolebindings"),
				"the shared identity sent %s %s", ev.Verb, ev.RequestURI)
		}
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
		// Bob's stream has a deadline, so a stream that outlives the revocation fails
		// the spec rather than hanging it.
		bobCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		revoked := bob.open(bobCtx, noteStream(ns))
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

// roleBindingStream is the /stream/v1 target for the RoleBindings of namespace ns, or
// of every namespace when ns is empty. They are shared in fx.sharedNamespace alone.
func roleBindingStream(ns string) string {
	q := url.Values{"group": {"rbac.authorization.k8s.io"}, "version": {"v1"}, "resource": {"rolebindings"}}
	if ns != "" {
		q.Set("namespace", ns)
	}
	return fx.foyerURL + "/stream/v1?" + q.Encode()
}

// expectAskedOnlyAs checks, in the audit log, that every request s led to was sent as
// username, impersonating nobody, and that one of them was for a path under prefix:
// the stream was the user's own, and the shared identity was never asked.
func expectAskedOnlyAs(ctx SpecContext, s *stream, username, prefix string) {
	GinkgoHelper()
	var events []auditEvent
	eventually(ctx, func() []auditEvent { events = fx.audited(s.Marker); return events }).ShouldNot(BeEmpty())
	asked := false
	for _, ev := range events {
		Expect(ev.User.Username).To(Equal(username), "%s %s", ev.Verb, ev.RequestURI)
		Expect(ev.ImpersonatedUser).To(BeNil())
		asked = asked || strings.HasPrefix(ev.RequestURI, prefix)
	}
	Expect(asked).To(BeTrue(), "no request under %s in %+v", prefix, events)
}

// accessReview asks the API server, as the fixture's admin, whether a service account
// may verb resource in ns, or in every namespace when ns is empty. A review names its
// groups itself, so the account's are given as the API server would resolve them.
func (f *fixture) accessReview(account, verb, group, resource, ns string) bool {
	GinkgoHelper()
	parts := strings.Split(account, ":") // system:serviceaccount:<namespace>:<name>
	Expect(parts).To(HaveLen(4))
	review := fmt.Sprintf(`{"apiVersion":"authorization.k8s.io/v1","kind":"SubjectAccessReview","spec":{`+
		`"user":%q,"groups":["system:serviceaccounts","system:serviceaccounts:%s","system:authenticated"],`+
		`"resourceAttributes":{"verb":%q,"group":%q,"resource":%q,"namespace":%q}}}`,
		account, parts[2], verb, group, resource, ns)
	file := filepath.Join(GinkgoT().TempDir(), "review.json")
	Expect(os.WriteFile(file, []byte(review), 0o600)).To(Succeed())
	return f.kubectl("create", "-f", file, "-o", "jsonpath={.status.allowed}") == "true"
}
