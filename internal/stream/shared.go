package stream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ConfigButler/krm-stream/gateway"
	"github.com/ConfigButler/krm-stream/gateway/kube"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/ConfigButler/krm-foyer/internal/gate"
	"github.com/ConfigButler/krm-foyer/internal/metrics"
)

// Shared watches (docs/design.md, "Shared watches"): for the resources configured
// for it, every stream of one scope reads from one watch at the API server, opened
// once with an identity of krm-foyer's own. That identity sees more than most
// users, so the API server is asked, with a SubjectAccessReview, whether each user
// may list and watch the scope before the stream is served, and again while it is
// open. RBAC stays the boundary; sharing only changes who holds the watch.

// SharedConfig turns shared watches on.
type SharedConfig struct {
	// TokenFile holds the token of the shared-watch identity: a service account
	// that may list and watch the shared resources and create SubjectAccessReviews,
	// and nothing else. It is read again as it changes, so a projected token rotates.
	TokenFile string
	// Resources are the resources whose streams share watches. Any other resource
	// is streamed with the user's own token, as without sharing.
	Resources []gateway.GroupResource
	// RecheckInterval is how often an open shared stream is authorized again. Zero
	// means DefaultRecheckInterval.
	RecheckInterval time.Duration
	// DecisionTTL is how long one user's access decision for one scope is reused,
	// by the user's other streams of it and by their rechecks. Zero means
	// DefaultDecisionTTL. A revoked grant ends a shared stream within
	// RecheckInterval + DecisionTTL + CheckTimeout, plus the write timeout.
	DecisionTTL time.Duration
	// QPS is how many requests a second the shared-watch identity's client sends:
	// every review and every shared watch, for every user. Bursts are twice as many.
	// Zero means DefaultSharedQPS.
	QPS float32
}

const (
	// DefaultRecheckInterval is the RecheckInterval when none is configured.
	DefaultRecheckInterval = 30 * time.Second
	// DefaultDecisionTTL is the DecisionTTL when none is configured.
	DefaultDecisionTTL = 10 * time.Second
	// CheckTimeout bounds one access decision: the list and the watch review.
	CheckTimeout = 10 * time.Second
	// subjectTimeout bounds the SelfSubjectReview that finds who a user is.
	subjectTimeout = 10 * time.Second
	// DefaultSharedQPS is the QPS when none is configured. The shared-watch
	// identity's client sends every SubjectAccessReview and opens every shared watch,
	// for every user: client-go's default of 5 a second would queue them. The API
	// server's own priority and fairness applies on top.
	DefaultSharedQPS = 100
)

// ParseResources reads resources as kubectl writes them, comma-separated:
// notes.hello.krm-foyer.example for a group's, configmaps for the core group's.
func ParseResources(list string) ([]gateway.GroupResource, error) {
	var out []gateway.GroupResource
	seen := map[gateway.GroupResource]bool{}
	for _, item := range strings.Split(list, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		resource, group, _ := strings.Cut(item, ".")
		if resource == "" || strings.ContainsAny(item, "/ ") || strings.ToLower(item) != item {
			return nil, fmt.Errorf("%q is not a resource as kubectl writes one, such as notes.hello.krm-foyer.example or configmaps", item)
		}
		gr := sharedResource(group, resource)
		if !seen[gr] {
			seen[gr] = true
			out = append(out, gr)
		}
	}
	return out, nil
}

// sharedResource is how a shared resource is admitted: in one namespace, or in every
// namespace for a user RBAC allows that, as for a cluster-scoped resource.
func sharedResource(group, resource string) gateway.GroupResource {
	return gateway.GroupResource{Group: group, Resource: resource, Scope: gateway.ResourceScopeNamespaced, AllowAllNamespaces: true}
}

// shared is what shared streams need, made once for the life of the process.
type shared struct {
	backend   *gateway.SharedBackend
	resources []gateway.GroupResource
	authorize gateway.Authorizer
	interval  time.Duration
}

func newShared(cfg SharedConfig, s *Streams) (*shared, error) {
	if cfg.TokenFile == "" || len(cfg.Resources) == 0 {
		return nil, errors.New("shared watches need a token file and at least one resource")
	}
	interval, ttl := cfg.RecheckInterval, cfg.DecisionTTL
	if interval == 0 {
		interval = DefaultRecheckInterval
	}
	if ttl == 0 {
		ttl = DefaultDecisionTTL
	}
	qps := cfg.QPS
	if qps == 0 {
		qps = DefaultSharedQPS
	}
	if interval < 0 || ttl < 0 || qps < 0 {
		return nil, fmt.Errorf("the recheck interval and the decision lifetime must be positive, got %v and %v", interval, ttl)
	}
	// Checked now, so a missing or empty token stops krm-foyer at startup rather than
	// refusing every shared stream later. client-go reads the file again as it changes.
	if token, err := os.ReadFile(cfg.TokenFile); err != nil {
		return nil, fmt.Errorf("the shared-watch token: %w", err)
	} else if strings.TrimSpace(string(token)) == "" {
		return nil, fmt.Errorf("the shared-watch token file %s is empty", cfg.TokenFile)
	}
	// Built by hand like a user's, with nothing from the environment: the token is
	// read from the file it was given, never the pod's own service account, and goes
	// to the configured API server alone, refusing redirects.
	rc := &rest.Config{
		Host:            s.server.String(),
		BearerTokenFile: cfg.TokenFile,
		Transport:       s.transport,
		UserAgent:       "krm-foyer shared watches",
		QPS:             qps,
		Burst:           int(2 * qps),
	}
	httpClient, err := kube.HTTPClientFor(rc)
	if err != nil {
		return nil, err
	}
	data, err := dynamic.NewForConfigAndClient(rc, httpClient)
	if err != nil {
		return nil, err
	}
	clientset, err := kubernetes.NewForConfigAndClient(rc, httpClient)
	if err != nil {
		return nil, err
	}
	m := s.gate.Metrics()
	upstream := &sharedUpstream{kube: kube.NewBackend(data), metrics: m}
	return &shared{
		backend: gateway.NewSharedBackendWithOptions(upstream, gateway.SharedOptions{
			Observer: observer{metrics: m},
		}),
		resources: slices.Clone(cfg.Resources),
		authorize: &decisions{
			check: kube.SubjectAccessReviewAuthorizer(clientset, subjectOf),
			ttl:   ttl, now: time.Now, metrics: m, entries: map[string]*decision{},
		},
		interval: interval,
	}, nil
}

// serves reports whether streams of scope share a watch.
func (sh *shared) serves(scope gateway.Scope) bool {
	if sh == nil {
		return false
	}
	return slices.ContainsFunc(sh.resources, func(gr gateway.GroupResource) bool {
		return gr.Group == scope.Group && gr.Resource == scope.Resource
	})
}

// subscriber is a shared stream's caller: the session's credential, which the
// stream needs for nothing but finding who the user is, and that answer.
type subscriber struct {
	subject kube.Subject
}

func subjectOf(p gateway.Principal) (kube.Subject, error) {
	sub, ok := p.(subscriber)
	if !ok {
		return kube.Subject{}, errors.New("not a shared stream's caller")
	}
	return sub.subject, nil
}

// principal finds who the user behind cred is to Kubernetes, with a
// SelfSubjectReview sent with the user's own token: the username, groups, UID and
// extras the API server makes of it, which the SubjectAccessReviews then ask about.
// Nothing the browser says, and no claim krm-foyer reads from the token, decides it.
func (u *upstream) principal(ctx context.Context, cred gate.Credential) (gateway.Principal, error) {
	m := u.streams.gate.Metrics()
	if cred.Token == "" {
		return nil, gateway.Unauthenticated("not signed in")
	}
	cfg := u.userConfig(cred)
	httpClient, err := kube.HTTPClientFor(cfg)
	if err != nil {
		return nil, err
	}
	client, err := kubernetes.NewForConfigAndClient(cfg, httpClient)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, subjectTimeout)
	defer cancel()
	review, err := client.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	switch {
	case err == nil && review.Status.UserInfo.Username != "":
	case err == nil:
		m.SubjectReview(metrics.SubjectError)
		u.streams.logger.Warn("the API server named no user for a shared stream's caller")
		return nil, errors.New("no username")
	case apierrors.IsUnauthorized(err):
		m.SubjectReview(metrics.SubjectRefused)
		return nil, gateway.Unauthenticated("the API server does not accept this session's token")
	default:
		m.SubjectReview(metrics.SubjectError)
		u.streams.logger.Warn("could not find who a shared stream's caller is", "cause", class(err))
		if retryable(err) {
			delay, _ := apierrors.SuggestsClientDelay(err)
			return nil, gateway.UpstreamUnavailable("the API server could not say who you are; retry later",
				time.Duration(delay)*time.Second)
		}
		return nil, err // UNAUTHENTICATED, without its text
	}
	m.SubjectReview(metrics.SubjectResolved)
	info := review.Status.UserInfo
	sub := subscriber{subject: kube.Subject{User: info.Username, UID: info.UID, Groups: slices.Clone(info.Groups)}}
	if len(info.Extra) > 0 {
		sub.subject.Extra = make(map[string]authorizationv1.ExtraValue, len(info.Extra))
		for k, v := range info.Extra {
			sub.subject.Extra[k] = slices.Clone(authorizationv1.ExtraValue(v))
		}
	}
	return sub, nil
}

// retryable reports whether err may pass: the API server busy, failing or out of
// reach.
func retryable(err error) bool {
	if apierrors.IsTooManyRequests(err) || apierrors.IsServiceUnavailable(err) || apierrors.IsInternalError(err) ||
		apierrors.IsServerTimeout(err) || apierrors.IsTimeout(err) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var api apierrors.APIStatus
	if errors.As(err, &api) {
		return api.Status().Code >= http.StatusInternalServerError
	}
	return !errors.Is(err, kube.ErrRedirectRefused) // no answer at all: the connection failed
}

// decisions reuses access decisions. A user's streams of one scope (a page opened
// in several tabs, nine streams of the rehearsal's) and their rechecks would each
// send two SubjectAccessReviews; with this, one pair answers all of them for ttl.
// Two checks of the same question at once wait for one answer, whatever it is.
//
// A decision is kept for exactly the question asked: the whole subject (user, UID,
// groups and extras, as the API server resolved them) and the whole scope. Only an
// answer is kept, allowed or denied; an error is not, and the next check asks again.
type decisions struct {
	check   gateway.Authorizer
	ttl     time.Duration
	now     func() time.Time
	metrics *metrics.Metrics

	mu      sync.Mutex
	entries map[string]*decision
	swept   time.Time
}

type decision struct {
	done chan struct{} // closed once err is set
	err  error
	at   time.Time
}

func (d *decisions) Authorize(ctx context.Context, p gateway.Principal, scope gateway.Scope) error {
	subject, err := subjectOf(p)
	if err != nil {
		return gateway.Unauthenticated("not authenticated")
	}
	// The question is what the reviews ask: the subject, and the group, version,
	// resource, namespace and name of the scope. A label selector is not part of it
	// (RBAC cannot grant by label, and the reviews do not send one), so streams that
	// differ only in their selector share a decision. TestASharedStreamAsksAboutEachUser
	// fails if the reviews start sending one.
	key, err := json.Marshal(struct {
		Subject                                   kube.Subject
		Group, Version, Resource, Namespace, Name string
	}{subject, scope.Group, scope.Version, scope.Resource, scope.Namespace, scope.Name})
	if err != nil {
		return err
	}

	d.mu.Lock()
	now := d.now()
	if now.Sub(d.swept) > d.ttl {
		for k, e := range d.entries {
			if isDone(e) && now.Sub(e.at) >= d.ttl {
				delete(d.entries, k)
			}
		}
		d.swept = now
	}
	e, ok := d.entries[string(key)]
	if ok && isDone(e) && now.Sub(e.at) >= d.ttl {
		ok = false
	}
	if ok {
		d.mu.Unlock()
		select {
		case <-e.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		// The answer the other check got. An error is passed on too, but not kept:
		// the next check asks again.
		if e.err == nil || isDenial(e.err) {
			d.metrics.AccessCheck(metrics.SourceCache, result(e.err), 0)
		}
		return e.err
	}
	e = &decision{done: make(chan struct{})}
	d.entries[string(key)] = e
	d.mu.Unlock()

	// Not the caller's context: others may be waiting for this answer, and the
	// caller leaving must not fail them. CheckTimeout bounds it instead.
	checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), CheckTimeout)
	defer cancel()
	start := time.Now()
	err = d.check.Authorize(checkCtx, p, scope)
	d.metrics.AccessCheck(metrics.SourceAPIServer, result(err), time.Since(start).Seconds())

	d.mu.Lock()
	e.err, e.at = err, d.now()
	if err != nil && !isDenial(err) && d.entries[string(key)] == e {
		delete(d.entries, string(key))
	}
	close(e.done)
	d.mu.Unlock()
	return err
}

func isDone(e *decision) bool {
	select {
	case <-e.done:
		return true
	default:
		return false
	}
}

// isDenial reports whether err is the API server saying no, rather than failing to
// answer.
func isDenial(err error) bool {
	var se *gateway.StreamError
	return errors.As(err, &se) && se.Code == gateway.CodeForbidden
}

func result(err error) string {
	switch {
	case err == nil:
		return metrics.ResultAllowed
	case isDenial(err):
		return metrics.ResultDenied
	default:
		return metrics.ResultError
	}
}

// sharedUpstream is the shared-watch identity's backend. Each user was authorized
// before the shared watch is read for them, so the API server refusing the shared
// identity itself is krm-foyer's configuration falling short, not the user's
// permissions: it reaches the browser as a terminal INTERNAL, without the API
// server's message, which would name that identity, and is logged by its kind. It
// also counts the watches it holds open.
type sharedUpstream struct {
	kube    *kube.Backend
	metrics *metrics.Metrics
}

// Watch returns at once, and the watch is opened on the first Next. krm-stream calls
// Watch holding a lock every shared scope takes, and with a context nobody cancels
// but the scope's own: opened here, one API server slow to answer would hold up the
// streams of every other scope, and a stream that gave up waiting could not end the
// request. Opened in Next, it is the scope's alone, and its last stream leaving
// cancels it (docs/investigations/krm-stream-shared-watches.md, ask 9).
func (b *sharedUpstream) Watch(ctx context.Context, scope gateway.Scope) (gateway.Watcher, error) {
	return &sharedWatcher{upstream: b, ctx: ctx, scope: scope}, nil
}

type sharedWatcher struct {
	upstream *sharedUpstream
	ctx      context.Context // the scope's: cancelled when its last stream leaves
	scope    gateway.Scope

	mu      sync.Mutex
	open    gateway.Watcher // nil until opened
	done    func()
	stopped bool
}

func (w *sharedWatcher) Next(ctx context.Context) (gateway.WatchEvent, error) {
	w.mu.Lock()
	open := w.open
	w.mu.Unlock()
	if open == nil {
		opened, err := w.upstream.kube.Watch(w.ctx, w.scope)
		if err != nil {
			return gateway.WatchEvent{}, ownRefusal(err)
		}
		w.mu.Lock()
		if w.stopped {
			w.mu.Unlock()
			opened.Stop()
			return gateway.WatchEvent{}, context.Canceled
		}
		w.open, w.done = opened, w.upstream.metrics.UpstreamWatch(metrics.IdentityShared)
		open = opened
		w.mu.Unlock()
	}
	ev, err := open.Next(ctx)
	if ev.Type == gateway.WatchError {
		ev.Err = ownRefusal(ev.Err)
	}
	return ev, ownRefusal(err)
}

func (w *sharedWatcher) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return
	}
	w.stopped = true
	if w.open != nil {
		w.open.Stop()
		w.done()
	}
}

// errSharedRefused is the cause of an INTERNAL sent for the shared identity being
// refused.
var errSharedRefused = errors.New("the API server refused the shared-watch identity")

func ownRefusal(err error) error {
	var se *gateway.StreamError
	if !errors.As(err, &se) || (se.Code != gateway.CodeForbidden && se.Code != gateway.CodeUnauthenticated) {
		return err
	}
	return &gateway.StreamError{Code: gateway.CodeInternal, Terminal: true,
		Message: "krm-foyer may not watch this for you; its operator can tell why from its log",
		Cause:   fmt.Errorf("%w: %w", errSharedRefused, err)}
}

// observer counts the shared watch's subscriptions and overflows.
type observer struct {
	metrics *metrics.Metrics
}

func (o observer) Observe(obs gateway.Observation) {
	switch obs.Kind {
	case gateway.ObservationSharedSubscriptionOpened:
		o.metrics.SharedSubscriptionOpened()
	case gateway.ObservationSharedSubscriptionClosed:
		o.metrics.SharedSubscriptionClosed()
	case gateway.ObservationSharedOverflow:
		o.metrics.SharedOverflow()
	}
}
