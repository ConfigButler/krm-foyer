# Vision

What krm-foyer is for, who it helps, and what it will not become. The
[service design](design.md) is the contract; this document is the reason for it.

## The argument

**A browser application can use the Kubernetes API as its backend.** Custom resources are
the schema, RBAC does the authorization, admission enforces the rules, the audit log is
the history, and a refusal the user sees is a real answer from the API server.

What stands in the way is plumbing. Before a browser can make its first API call, someone
has to write an OIDC client, a session store, a proxy that keeps tokens off the browser,
and a gateway for live resource streams. Every application built this way writes those
four things again, and they are where the security bugs live.

krm-foyer is those four things, built once, kept small, and tested hard enough that
other people can put it in front of a cluster.

## Who it helps

- **A frontend developer** reads resources, submits changes and follows their progress
  live, using Kubernetes objects and [krm-stream](https://github.com/ConfigButler/krm-stream),
  with no backend code of their own.
- **A domain team** puts its rules in CRDs, admission and controllers, and gets one
  contract that browsers, `kubectl` and automation all use.
- **A platform team** exposes approved APIs through one service it can audit, instead of
  one hand-written auth layer per application.

Adding a resource whose permissions and invariants are already enforced should take
configuration and frontend work, not another backend handler. The benefit is reuse across
applications. A separate service also costs deployment, session storage and support; for
a single application it does not guarantee less code in total.

## Explorable in a browser

Most traffic will come from a library or a few lines of JavaScript, but krm-foyer should
also make sense to a person with nothing but a browser tab. Open a `/k8s/...` URL and you
see what Kubernetes answered for you. Where krm-foyer itself steps in, because you are not
signed in, or the upstream answered with a redirect or a page it will not render, it tells you, in a page instead of a bare error code. For a redirect,
you see where it leads and choose whether to go.

Two planned pages will answer the questions everyone asks first. `/auth/whoami` will show who Kubernetes
thinks you are. `/_foyer/access` will show what you may do, from Kubernetes' own reviews,
with a "can I?" form. Code asks Kubernetes the same questions natively, to hide buttons
that would only produce a 403.

Not at any price: code always gets the same JSON and status code, krm-foyer never
replaces an answer from Kubernetes, and no page lets anyone run upstream content as the
signed-in user. The [design](design.md#interruptions) has the rules.

## What it takes from the domain

krm-foyer carries requests; it does not make a domain safe to expose. That work belongs to
the domain, in Kubernetes:

1. The browser creates a resource, say a `WorkspaceRequest`, with its CSRF proof.
   Admission validates it and binds ownership to the authenticated user before anything
   is stored.
2. Kubernetes answers 201. The UI shows **Request received**, not **Workspace ready**, and
   follows the resource's stream.
3. A controller decides, and publishes acceptance or rejection in `status`, which users
   cannot write. A rejection has a stable reason the UI can show.
4. The controller does the work under a key tied to the request's UID, so a restart
   resumes it instead of starting it twice. The UI shows **Provisioning**.
5. When it is ready, `status` says where to go. The destination checks access itself;
   knowing its URL is not a grant.

Every step there is the domain's code. If the browser lost the create response, the
outcome is unknown, and the UI looks the request up before offering to retry. A
disconnected stream means progress is unknown, not that the request was rejected.

This fits durable intent, observable processing and several clients sharing one API. It
fits less well for immediate transactions, private ad-hoc queries and high-volume event
storage. The [decision guide](bff-choice.md) works through where the line is, with a
quiz, reservations, reporting and payments as examples.

## Staying small

The project stays useful by staying small, so these stay out:

- **Application-specific endpoints, DTOs or aggregation.** Domain logic belongs in
  operators and admission.
- **A single-page application or resource browser.** See the [frontend decision](frontend.md).
- **Access rules of its own,** for now. Kubernetes decides; limiting an application
  further is a [deferred design](application-scope.md).
- **Impersonation**, and any fallback to krm-foyer's own service account for a user's
  request.
- **Exec, attach and port-forward**, until they have their own design and tests.
- **A reimplementation of anything krm-stream provides.** Problems found while building
  on it are reported to krm-stream.

A feature that needs any of these belongs in the application or in krm-stream.

## How we will know it works

Two frontends with different API groups run on krm-foyer with no application-specific
code in it. Until a second one exists, krm-foyer is product development, not proven
reuse. The [roadmap](roadmap.md) tracks the way there; [heritage](heritage.md) records
the demo that started it.
