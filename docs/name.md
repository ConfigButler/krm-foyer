# Product naming

Decision (2026-09-30): the product is called **krm-foyer**. Use lowercase and the
hyphenated spelling throughout. It was designed under the working name **k8s-front**,
which already has prior uses (see below).

**krm-foyer — the browser's way in to Kubernetes: login, API access and live krm-stream resources.**

The technical description remains **a Kubernetes BFF for browser applications**.
That description, not the name, tells readers what the service is.

## Why krm-foyer

Three requirements decided it:

1. **A fresh name.** Every descriptive candidate checked (`kube-front`, `k8s-front`,
   `krm-front`) already appears on GitHub, two of them for Kubernetes frontends. The
   words that describe the service best are the ones others have already used.
2. **Belonging to krm-stream.** The service and the library have complementary
   responsibilities: the service owns authentication and API access, and the library
   owns live resource views and reconciliation. A shared `krm-` prefix shows that
   relationship without a sentence of explanation. `krm-front` was dropped only because
   of [chenjunfit/krm-front](https://github.com/chenjunfit/krm-front), not because the
   pairing was wrong.
3. **Not a UI library.** "front" invites frontend developers to read it as a component
   library or a dashboard. "foyer" does not suggest a UI toolkit.

The earlier argument for `k8s` over `krm` was clarity: KRM (Kubernetes Resource Model)
needs explaining, and a frontend developer who works with Kubernetes may not know the
term. That argument only holds while the rest of the name describes the service. Once
the name contains a metaphor, the tagline has to explain it anyway, so the name is no
longer self-explanatory with either prefix. The tagline can explain KRM at the same
time, and readers then learn it once for the whole `krm-*` family.

The honest cost is that a newcomer meets two unfamiliar things at once: an acronym and
a metaphor. Mitigate that by always introducing the service with its tagline, and by
linking "KRM" to its definition on first use in the README.

A foyer is an entrance hall: a metaphor for browser entry, login and access to
Kubernetes resources. It says where the service sits, not how it works. The tagline
must make clear that this is a deployed backend service that proxies API requests.

## Prior-use checks

The first round of checks ran on **2026-09-11** and the foyer names were checked again on
**2026-09-30**. Both rounds used web search, GitHub repository search and the npm
registry, and included hyphenated and joined spellings. GitHub search also returns
partial matches, so result counts are not counts of exact-name projects.

| Candidate | Evidence | Naming implication |
| --- | --- | --- |
| `krm-foyer` / `krmfoyer` | 2026-09-30: GitHub repository-name search returned zero results for both spellings. Both exact npm registry lookups returned 404. A web search returned only unrelated uses of the letters KRM (an architecture firm, a client's initials, radio call signs), with no software project. | No collision found. The chosen name. |
| `kubefoyer` / `kube-foyer` | No software project surfaced in the web searches. GitHub repository-name search returned zero results for `kubefoyer` on 2026-09-11 and for both spellings on 2026-09-30. Both exact npm registry lookups returned 404. | No collision found. A viable fallback if the krm family is abandoned. |
| `kube-front` / `kubefront` | [rctl/kubefront](https://github.com/rctl/kubefront) is a Kubernetes dashboard with a repository created in March 2018. [dhyanio/kubefront](https://github.com/dhyanio/kubefront) describes a cloud compliance and autotagging tool. [kubefront.net](https://kubefront.net/devops/kubelogin-openid-connect-kubernetes/) also uses the name for Kubernetes-related content. | Existing uses in the same ecosystem. Adding a hyphen would not meaningfully distinguish the name. |
| `k8s-front` | Exact repository names include [fifthl/k8s-front](https://github.com/fifthl/k8s-front), created in April 2024, and [Soyoung-Kim/k8s-front](https://github.com/Soyoung-Kim/k8s-front). | Understandable, but already used. These findings do not establish a prominent product, yet they fail the preference for a fresh name. |
| `krm-front` | [chenjunfit/krm-front](https://github.com/chenjunfit/krm-front), created in March 2025, describes a Kubernetes multi-cluster management frontend. | Close subject matter makes confusion particularly plausible. |
| `k8s-bff` | No exact-name repository appeared in the returned GitHub results. Related names include [PRO-Robotech/openapi-ui-k8s-bff](https://github.com/PRO-Robotech/openapi-ui-k8s-bff). | Useful architectural description, but a generic phrase with existing nearby uses. |

Exact npm lookups also returned 404 for `kube-front`, `kubefront`, `k8s-front`,
`krm-front` and `k8s-bff`. An absent npm package does not outweigh an existing project
elsewhere, and a 404 does not guarantee that a registry will permit registration.
These results mean **no matching project found where reported**, not that a name has
never been used. Domain and trademark availability were not checked; do that before
the first release is announced.

## Spelling

Prefer **krm-foyer** over `krmfoyer`: the word boundary is easier to read and matches the
lowercase, hyphenated style of `krm-stream`. Treat both spellings as the same name when
checking for collisions. A hyphen would not solve a collision with an existing product,
just as it does not solve the existing `kubefront` uses. Keep one canonical spelling in
documentation and distribution names.

## Product name and URL paths

The product name and API paths should be coherent, but need not be identical:

| Name or path | Meaning |
| --- | --- |
| `krm-foyer` | The service, its repository and its container image |
| `/k8s/...` | Native Kubernetes API access through the proxy |
| `/stream` | krm-stream access |
| `/auth/...` | Login, callbacks and sessions |

Keep paths based on their purpose rather than inserting the product name into every
URL. `/k8s/` describes what is behind it, the Kubernetes API, whatever the product is
called. This lets API consumers understand the routes independently of branding, and
a later rename would need no URL changes.

## Relationship to krm-stream

Describe krm-stream as the underlying streaming library. Using it does not require
krm-foyer, and ordinary Kubernetes API requests do not pass through the stream
protocol. krm-foyer hosts both access paths and is independent of any particular
frontend framework. Sharing the `krm-` prefix signals that they are
designed together, not that one depends on the other.

The name does not imply upstream endorsement, shared ownership or affiliation with
Kubernetes. The checks above assess discoverable prior use; they do not establish
exclusive rights to a name.
