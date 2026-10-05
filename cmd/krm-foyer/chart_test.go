package main

import (
	"bytes"
	"encoding/pem"
	"io"
	"io/fs"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

const chartDir = "../../charts/krm-foyer"

// chartValues are the values every render starts from: the ones the chart requires.
var chartValues = []string{
	"publicURL=https://foyer.example.test", "oidc.issuer=https://dex.example.test",
	"oidc.clientID=krm-foyer", "oidc.clientSecret.secretName=krm-foyer-oidc",
	// Plain HTTP, the default, needs the ingress named.
	"networkPolicy.from[0].namespaceSelector.matchLabels.role=ingress",
}

// helmTemplate renders the chart with the required values, then the values files and
// --set values given. Helm reads the chart in a subprocess, which go test cannot see,
// so every chart file is read here first: that makes them inputs of the test cache,
// and a chart-only edit runs the test again rather than replaying a cached pass.
func helmTemplate(t *testing.T, valueFiles []string, set ...string) ([]byte, error) {
	t.Helper()
	chart := os.DirFS(chartDir)
	err := fs.WalkDir(chart, ".", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			_, err = fs.ReadFile(chart, path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"template", "krm-foyer", chartDir, "--namespace", "krm-foyer"}
	for _, f := range valueFiles {
		if _, err := os.ReadFile(f); err != nil { //nolint:gosec // the test's own values files
			t.Fatal(err)
		}
		args = append(args, "-f", f)
	}
	for _, s := range append(slices.Clone(chartValues), set...) {
		args = append(args, "--set", s)
	}
	return exec.CommandContext(t.Context(), "helm", args...).CombinedOutput()
}

// rendered is the chart's output, by kind.
type rendered struct {
	deployment      appsv1.Deployment
	serviceAccounts []corev1.ServiceAccount
	secrets         []corev1.Secret
	services        []corev1.Service
	clusterRoles    []rbacv1.ClusterRole
	bindings        []rbacv1.ClusterRoleBinding
	roleBindings    []rbacv1.RoleBinding
	networkPolicies []networkingv1.NetworkPolicy
}

func render(t *testing.T, valueFiles []string, set ...string) rendered {
	t.Helper()
	out, err := helmTemplate(t, valueFiles, set...)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	var r rendered
	deployments := 0
	for _, doc := range bytes.Split(out, []byte("\n---")) {
		var meta struct{ Kind string }
		if err := yaml.Unmarshal(doc, &meta); err != nil {
			t.Fatalf("%v in\n%s", err, doc)
		}
		var target any
		switch meta.Kind {
		case "":
			continue
		case "Deployment":
			deployments++
			target = &r.deployment
		case "ServiceAccount":
			target = appendNew(&r.serviceAccounts)
		case "Secret":
			target = appendNew(&r.secrets)
		case "Service":
			target = appendNew(&r.services)
		case "ClusterRole":
			target = appendNew(&r.clusterRoles)
		case "ClusterRoleBinding":
			target = appendNew(&r.bindings)
		case "RoleBinding":
			target = appendNew(&r.roleBindings)
		case "NetworkPolicy":
			target = appendNew(&r.networkPolicies)
		default:
			t.Fatalf("the chart renders a %s this test does not know: check it, then add it here", meta.Kind)
		}
		if err := yaml.UnmarshalStrict(doc, target); err != nil {
			t.Fatalf("%v in\n%s", err, doc)
		}
	}
	if deployments != 1 {
		t.Fatalf("%d Deployments rendered, want 1", deployments)
	}
	return r
}

func appendNew[T any](s *[]T) *T {
	*s = append(*s, *new(T))
	return &(*s)[len(*s)-1]
}

// container is krm-foyer's container in the rendered pod.
func (r rendered) container(t *testing.T) corev1.Container {
	t.Helper()
	if c := r.deployment.Spec.Template.Spec.Containers; len(c) == 1 {
		return c[0]
	}
	t.Fatal("want one container")
	return corev1.Container{}
}

// parseRendered hands the rendered argv to the binary's own flag parsing, with each file
// the pod mounts in place.
func parseRendered(t *testing.T, r rendered) config {
	t.Helper()
	srv := httptest.NewTLSServer(nil)
	srv.Close()
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	mounted := map[string]string{}
	for _, m := range r.container(t).VolumeMounts {
		for _, name := range []string{"tls.crt", "tls.key", "client-secret", "ca.crt", "token"} {
			mounted[m.MountPath+"/"+name] = ca
		}
	}
	cfg, err := parseConfig(r.container(t).Args, files(mounted), io.Discard)
	if err != nil {
		t.Fatalf("the binary refuses the chart's arguments %q: %v", r.container(t).Args, err)
	}
	return cfg
}

// The chart writes krm-foyer's argv and the binary parses it; nothing else holds the two
// together. A flag the binary does not know, a number Helm prints as a float, or a value
// that keeps its quotes would render, lint and install cleanly, and then crash-loop. So
// each case parses what the chart emits, with the e2e fixture's own values among them.
func TestChartRendersArgsTheBinaryAccepts(t *testing.T) {
	e2e := "../../test/e2e/cluster/"
	t.Run("required values only", func(t *testing.T) {
		cfg := parseRendered(t, render(t, nil))
		if cfg.login == nil || cfg.login.auth.PublicURL != "https://foyer.example.test" || cfg.login.shared != nil {
			t.Fatalf("login %+v", cfg.login)
		}
		if cfg.tlsCert != "" || cfg.listen != ":8080" || cfg.metricsListen != ":9090" {
			t.Fatalf("listen %q, TLS %q, metrics %q", cfg.listen, cfg.tlsCert, cfg.metricsListen)
		}
		if cfg.login.kubernetes.RootCAs == nil || cfg.login.auth.RootCAs != nil {
			t.Fatal("want the API server's CA from kube-root-ca.crt, and the system's for the issuer")
		}
	})
	t.Run("e2e fixture", func(t *testing.T) {
		cfg := parseRendered(t, render(t, []string{e2e + "foyer-values.yaml"}))
		if cfg.listen != ":8443" || cfg.tlsCert == "" || cfg.login.auth.RootCAs == nil {
			t.Fatalf("listen %q, TLS %q", cfg.listen, cfg.tlsCert)
		}
		if s := cfg.login.shared; s == nil || len(s.Resources) != 1 || s.Resources[0].Resource != "notes" ||
			s.Resources[0].Group != "hello.krm-foyer.example" {
			t.Fatalf("shared %+v", cfg.login.shared)
		}
	})
	t.Run("e2e brief fixture", func(t *testing.T) {
		cfg := parseRendered(t, render(t, []string{e2e + "foyer-values.yaml", e2e + "foyer-brief-values.yaml"}))
		g, s := cfg.login.gate, cfg.login.sessions
		if s.IdleTimeout != 45*time.Second || g.SessionCheckInterval != time.Second || g.MaxSessionConcurrentRequests != 2 ||
			g.SessionRequestRate != 1 || g.SessionRequestBurst != 10 || cfg.login.kubernetes.MaxResponseBytes != 131072 {
			t.Fatalf("gate %+v, sessions %+v", g, s)
		}
		if cfg.login.shared.RecheckInterval != 2*time.Second || cfg.login.shared.DecisionTTL != time.Second {
			t.Fatalf("shared %+v", cfg.login.shared)
		}
	})
	t.Run("every value set", func(t *testing.T) {
		values := filepath.Join(t.TempDir(), "values.yaml")
		// Large numbers, which Helm reads from a file as floats, and a fraction.
		err := os.WriteFile(values, []byte(`
oidc: {scopes: [openid, email], caConfigMap: {name: issuer-ca}}
tls: {secretName: krm-foyer-tls}
metrics: {enabled: false}
bounds:
  sessionIdleTimeout: 2h
  sessionAbsoluteTimeout: 10h
  sessionCheckInterval: 3s
  maxResponseDuration: 1h
  streamWriteTimeout: 15s
  maxSessionConcurrentRequests: 7
  maxConcurrentRequests: 3000000
  maxSessionStreams: 9
  maxStreams: 4000
  sessionRequestBurst: 50
  sessionRequestRate: 0.5
  maxResponseBytes: 67108864
sharedWatches:
  resources: [notes.hello.krm-foyer.example, configmaps]
  tuning: {recheckInterval: 1m, decisionTTL: 20s, qps: 2500000}
`), 0o600)
		if err != nil {
			t.Fatal(err)
		}
		cfg := parseRendered(t, render(t, []string{values}))
		l := cfg.login
		if cfg.metricsListen != "" || !slices.Equal(l.auth.Scopes, []string{"openid", "email"}) {
			t.Fatalf("metrics %q, scopes %q", cfg.metricsListen, l.auth.Scopes)
		}
		if l.sessions.IdleTimeout != 2*time.Hour || l.sessions.AbsoluteTimeout != 10*time.Hour ||
			l.gate.SessionCheckInterval != 3*time.Second || l.gate.MaxResponseDuration != time.Hour ||
			l.streamWrites != 15*time.Second || l.gate.MaxSessionConcurrentRequests != 7 ||
			l.gate.MaxConcurrentRequests != 3000000 || l.gate.MaxSessionStreams != 9 || l.gate.MaxStreams != 4000 ||
			l.gate.SessionRequestBurst != 50 || l.gate.SessionRequestRate != 0.5 ||
			l.kubernetes.MaxResponseBytes != 67108864 {
			t.Fatalf("gate %+v, sessions %+v, bytes %d", l.gate, l.sessions, l.kubernetes.MaxResponseBytes)
		}
		if s := l.shared; len(s.Resources) != 2 || s.RecheckInterval != time.Minute || s.DecisionTTL != 20*time.Second ||
			s.QPS != 2500000 {
			t.Fatalf("shared %+v", s)
		}
	})
}

// The schema is the chart's contract: a misspelt key, a missing value or a malformed
// one fails the install, naming the value, instead of rendering something krm-foyer
// refuses at start or, worse, something it accepts and does differently.
func TestChartRefusesValues(t *testing.T) {
	tests := map[string]struct {
		set  []string
		want string
	}{
		"no public URL":                 {[]string{"publicURL="}, "/publicURL"},
		"a public URL with a path":      {[]string{"publicURL=https://foyer.example.test/app"}, "/publicURL"},
		"an issuer over plain HTTP":     {[]string{"oidc.issuer=http://dex.example.test"}, "/oidc/issuer"},
		"no client secret":              {[]string{"oidc.clientSecret.secretName="}, "/oidc/clientSecret/secretName"},
		"a misspelt key":                {[]string{"publicUrl=https://foyer.example.test"}, "publicUrl"},
		"a misspelt bound":              {[]string{"bounds.maxStream=10"}, "maxStream"},
		"a duration without a unit":     {[]string{"bounds.sessionIdleTimeout=45"}, "/bounds/sessionIdleTimeout"},
		"a bound of zero":               {[]string{"bounds.maxStreams=0"}, "/bounds/maxStreams"},
		"a resource with a wildcard":    {[]string{"sharedWatches.resources={*}"}, "/sharedWatches/resources/0"},
		"a resource with a subresource": {[]string{"sharedWatches.resources={pods/log}"}, "/sharedWatches/resources/0"},
		"two replicas":                  {[]string{"replicaCount=2"}, "replicaCount"},
		"plain HTTP open to anyone":     {[]string{"networkPolicy.from=null"}, "networkPolicy.from must name the ingress"},
		"an account named by no one":    {[]string{"serviceAccount.create=false"}, "/serviceAccount"},
		"the pod's account as the shared identity": {
			[]string{"sharedWatches.resources={configmaps}", "sharedWatches.serviceAccount.name=krm-foyer"},
			"must not be the pod's own service account",
		},
		"the pod's own named account as the shared identity": {
			[]string{"sharedWatches.resources={configmaps}", "serviceAccount.create=false", "serviceAccount.name=app",
				"sharedWatches.serviceAccount.create=false", "sharedWatches.serviceAccount.name=app"},
			"must not be the pod's own service account",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			out, err := helmTemplate(t, nil, tc.set...)
			if err == nil {
				t.Fatalf("rendered:\n%s", out)
			}
			if !strings.Contains(string(out), tc.want) {
				t.Fatalf("want an error naming %q, got:\n%s", tc.want, out)
			}
		})
	}
}

// krm-foyer never sends a request as its pod (docs/design.md): every request carries
// the user's token, or the shared-watch identity's. The chart is where that could quietly
// stop being true, by mounting the pod's token or granting its account something. This
// renders the chart with every grant it can make and looks for a way to the pod's
// account: a binding, a mounted token, or a shared-watch token that is the pod's own.
func TestChartGivesThePodsAccountNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		set []string
		// sharedToken is the Secret the shared-watch token must come from; empty
		// when no token may be mounted at all.
		sharedToken string
	}{
		"defaults":       {nil, ""},
		"shared watches": {[]string{"sharedWatches.resources={notes.hello.krm-foyer.example,configmaps}"}, "krm-foyer-shared-token"},
		"an account it brings": {[]string{"serviceAccount.create=false", "serviceAccount.name=app",
			"sharedWatches.resources={configmaps}"}, "krm-foyer-shared-token"},
		"a shared account it brings": {[]string{"sharedWatches.resources={configmaps}",
			"sharedWatches.serviceAccount.create=false", "sharedWatches.serviceAccount.name=watcher"}, "watcher-token"},
	} {
		t.Run(name, func(t *testing.T) {
			r := render(t, nil, tc.set...)
			pod := r.deployment.Spec.Template.Spec
			account := pod.ServiceAccountName
			if account == "" || account == "default" {
				t.Fatalf("the pod runs as %q", account)
			}
			if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
				t.Fatal("the pod mounts its account's token")
			}
			for _, sa := range r.serviceAccounts {
				if sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
					t.Fatalf("service account %s mounts its token by default", sa.Name)
				}
			}
			// The only token mounted is the shared-watch identity's, from its own Secret.
			var tokens []string
			for _, v := range pod.Volumes {
				if v.Projected != nil {
					t.Fatalf("volume %s is projected, which can only carry the pod's own token", v.Name)
				}
				if v.Secret != nil && slices.ContainsFunc(v.Secret.Items, func(k corev1.KeyToPath) bool { return k.Key == "token" }) {
					tokens = append(tokens, v.Secret.SecretName)
				}
			}
			if want := []string{tc.sharedToken}; tc.sharedToken == "" && len(tokens) > 0 ||
				tc.sharedToken != "" && !slices.Equal(tokens, want) {
				t.Fatalf("tokens mounted from %q, want %q", tokens, tc.sharedToken)
			}
			for _, s := range r.secrets {
				if s.Annotations[corev1.ServiceAccountNameKey] == account {
					t.Fatalf("Secret %s holds the pod's own token", s.Name)
				}
			}
			if len(r.roleBindings) > 0 {
				t.Fatal("the chart binds a Role; check it does not grant the pod's account anything, then allow it here")
			}
			for _, b := range r.bindings {
				for _, s := range b.Subjects {
					if s.Name == account {
						t.Fatalf("%s binds the pod's account to %s", b.Name, b.RoleRef.Name)
					}
				}
			}
		})
	}
}

// The shared-watch identity sees what every user of a shared resource might, so the
// chart grants it the least that works (docs/watches.md): list and watch on exactly the
// resources named, and asking the API server about users.
func TestChartGrantsTheSharedIdentityOnlyWhatItNeeds(t *testing.T) {
	r := render(t, nil, "sharedWatches.resources={notes.hello.krm-foyer.example,configmaps,deployments.apps}")
	if len(r.clusterRoles) != 2 {
		t.Fatalf("%d ClusterRoles, want 2", len(r.clusterRoles))
	}
	want := []rbacv1.PolicyRule{
		{APIGroups: []string{"hello.krm-foyer.example"}, Resources: []string{"notes"}, Verbs: []string{"list", "watch"}},
		{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"list", "watch"}},
		{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: []string{"list", "watch"}},
	}
	got := r.clusterRoles[0].Rules
	reviews := r.clusterRoles[1].Rules
	if len(reviews) != 1 || !slices.Equal(reviews[0].APIGroups, []string{"authorization.k8s.io"}) ||
		!slices.Equal(reviews[0].Resources, []string{"subjectaccessreviews"}) || !slices.Equal(reviews[0].Verbs, []string{"create"}) ||
		len(reviews[0].ResourceNames) > 0 || len(reviews[0].NonResourceURLs) > 0 {
		t.Fatalf("the reviews role is %+v", reviews)
	}
	if len(got) != len(want) {
		t.Fatalf("rules %+v", got)
	}
	for i := range want {
		if !slices.Equal(got[i].APIGroups, want[i].APIGroups) || !slices.Equal(got[i].Resources, want[i].Resources) ||
			!slices.Equal(got[i].Verbs, want[i].Verbs) || len(got[i].ResourceNames) > 0 || len(got[i].NonResourceURLs) > 0 {
			t.Fatalf("rule %d is %+v, want %+v", i, got[i], want[i])
		}
	}
	roles := map[string]bool{}
	for _, b := range r.bindings {
		if len(b.Subjects) != 1 || b.Subjects[0].Name != "krm-foyer-shared" || b.Subjects[0].Namespace != "krm-foyer" {
			t.Fatalf("%s binds %+v", b.Name, b.Subjects)
		}
		roles[b.RoleRef.Name] = true
	}
	if len(roles) != 2 || !roles[r.clusterRoles[0].Name] || !roles[r.clusterRoles[1].Name] {
		t.Fatalf("the shared identity is bound to %v", roles)
	}
}

// With plain HTTP, the hop from the ingress carries session cookies, so only the
// ingress may reach the origin port (docs/ingress.md); the chart refuses to render
// without it being named. With TLS the origin is open, and metrics are open unless
// limited. No other port is admitted.
func TestChartAdmitsOnlyTheIngress(t *testing.T) {
	type rule struct {
		port int32
		from int
	}
	for name, tc := range map[string]struct {
		set  []string
		want []rule
	}{
		"plain HTTP":           {nil, []rule{{8080, 1}, {9090, 0}}},
		"TLS":                  {[]string{"tls.secretName=tls", "networkPolicy.from=null"}, []rule{{8443, 0}, {9090, 0}}},
		"metrics limited":      {[]string{"networkPolicy.metricsFrom[0].podSelector.matchLabels.app=prometheus"}, []rule{{8080, 1}, {9090, 1}}},
		"metrics off":          {[]string{"metrics.enabled=false"}, []rule{{8080, 1}}},
		"turned off with TLS":  {[]string{"tls.secretName=tls", "networkPolicy.enabled=false"}, nil},
		"turned off, explicit": {[]string{"networkPolicy.from=null", "networkPolicy.enabled=false"}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			r := render(t, nil, tc.set...)
			if tc.want == nil {
				if len(r.networkPolicies) != 0 {
					t.Fatalf("%d NetworkPolicies, want none", len(r.networkPolicies))
				}
				return
			}
			if len(r.networkPolicies) != 1 {
				t.Fatalf("%d NetworkPolicies, want 1", len(r.networkPolicies))
			}
			p := r.networkPolicies[0].Spec
			if !slices.Equal(p.PolicyTypes, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}) ||
				p.PodSelector.MatchLabels["app.kubernetes.io/name"] != "krm-foyer" {
				t.Fatalf("policy %+v", p)
			}
			var got []rule
			for _, in := range p.Ingress {
				if len(in.Ports) != 1 {
					t.Fatalf("a rule with ports %+v", in.Ports)
				}
				got = append(got, rule{in.Ports[0].Port.IntVal, len(in.From)})
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("admits %+v, want %+v", got, tc.want)
			}
		})
	}
}
