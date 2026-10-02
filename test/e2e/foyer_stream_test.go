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
})
