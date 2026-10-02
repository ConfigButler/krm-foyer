# Heritage

Where krm-foyer comes from. This file records the past and binds nothing: what krm-foyer
is for is in the [vision](vision.md), what it does is in the [design](design.md), and what
is left to build is in the [roadmap](roadmap.md).

## Voter: the demo that proved the idea

[Voter](https://github.com/sunib/voter) is a conference demo built between March and
September 2026. Attendees scan a QR code, enrol through Room Pass, get their own Kubernetes
identity from Dex, and then vote in a quiz and edit a coffee menu from their phones. Every
action is an API call made with their own token, and every save becomes a Git commit through
[gitops-reverser](https://github.com/ConfigButler/gitops-reverser).

Voter settled several questions that krm-foyer's design now takes as given:

| Voter's finding | Where it lives in krm-foyer |
| --- | --- |
| The first design (March 2026) used an auth service with `Impersonate-User`. It was removed: impersonation lets the application write its own audit provenance | Requests go to Kubernetes with the user's own credential. There is no impersonation and no service-account fallback |
| Tokens stay server-side. JavaScript receives identity and a CSRF token from `/auth/session`, never a bearer token | [Login and sessions](design.md#login-and-sessions) |
| `/auth/whoami` (SelfSubjectReview) is the first thing to check when Kubernetes answers 403 | [/auth/whoami](frontend.md#what-ships-in-the-binary) |
| One shared watch can serve two hundred browsers, if each subscriber is re-checked with a SubjectAccessReview | [Streams and editing](design.md#streams-and-editing) |
| A 30-second SubjectAccessReview recheck covers RBAC for the captured subject. It does not cover identity-provider changes, and must not be described as revocation | [Session lifecycle](design.md#session-lifecycle) |

Voter also showed what a generic service would remove. Its browser never calls
`/apis/...` directly. Each feature has its own `/public/*` handler, and the streams
are limited by a Go variable that lists five resources plus namespace and name pins from
environment variables. Every application built this way writes that code again. krm-foyer
replaces it with the native API paths, and leaves the decisions to RBAC.

The service was designed inside Voter under the working name **k8s-front**.
Voter's adoption review recommended building it as a separate product with its own tests,
and warned that no second consuming application exists yet. That warning is why "two
frontends, two API groups, no application-specific handlers" is the first
[release criterion](design.md#release-criteria).

## What broke on stage

On 2026-09-17, Voter ran in front of about two hundred people. 67 people signed in and 38
ballots were recorded. The [post-mortem](https://github.com/sunib/voter/blob/main/docs/post-demo-2026-09-17.md)
found three defects, and each one shaped a rule here:

1. **The vote guard compared `resourceVersion` when it should have compared `generation`.**
   A controller writing `status` moved `resourceVersion` about once a second, so anyone who
   read the questions carefully was refused. krm-foyer's rule: pass Kubernetes semantics
   through exactly, and never add a guard of our own on top of them.
2. **A 409 conflict reached people as the API server's raw text.** Some read "the object has
   been modified; please apply your changes to the latest version" as "the CRD is not the
   newest version". The proxy is right to pass the `Status` through unchanged. What was
   missing is a frontend helper and documentation that tell a 409 (retry) apart from a 403
   (refusal).
3. **Nothing recorded the refusals.** The failures had to be reconstructed from
   `creationTimestamp`s four days later. krm-foyer is to log every refusal, whether from
   policy or from upstream, with the subject and the reason (see the [roadmap](roadmap.md#seeing-what-happened)).

The post-mortem's closing line is this project's working rule too: *being built on the API
means inheriting its semantics exactly, not approximately.*

## krm-stream: the library next door

[krm-stream](https://github.com/ConfigButler/krm-stream) owns the watch-to-browser protocol,
shared watches, projections, drafts and reconciliation. Voter was its first real consumer, at
0.4.0, and krm-foyer its second: krm-stream 0.5.0 took its
[feedback](investigations/krm-stream-feedback.md). krm-foyer hosts krm-stream; it does not
reimplement any of it. Problems found while building on it are reported to krm-stream, the
way Voter did in its consumer feedback notes.

## gitops-reverser: how we build it

[gitops-reverser](https://github.com/ConfigButler/gitops-reverser) is the mature project, and
krm-foyer follows its engineering conventions rather than inventing new ones: a devcontainer
that holds every tool at pinned versions, Task as the only entry point, CI that runs the same
tasks, e2e against a real k3d cluster, actions pinned by SHA, and releases cut by
release-please.

krm-foyer does one thing differently on purpose: a single `task verify` gate instead of a
validation sequence written down in `AGENTS.md`. A small project can afford a single gate.
