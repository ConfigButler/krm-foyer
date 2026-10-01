# Choosing a universal or domain-specific BFF

Use this guide to decide whether a browser application should use native Kubernetes APIs,
a domain-specific backend, or both. It accompanies the [krm-foyer service design](design.md).
The examples illustrate design choices; krm-foyer does not implement their domain rules.

## Choose the domain API and the browser access layer

krm-foyer supports domains deliberately designed around the Kubernetes Resource Model
(KRM). Choosing its generic transport is separate from deciding whether KRM is the right
domain API. Establish both before committing to the architecture.

| Domain API choice | What it means | What must be established |
| --- | --- | --- |
| Expose an existing Kubernetes model | Keep current resources and let clients use their native API | Existing persistence and permissions must already be safe without narrow handlers |
| Design the domain in KRM | Make CRDs, request/intent semantics, status and domain operators the supported public contract | Explicit lifecycles, trusted identities, enforcement, concurrency and operational ownership |
| Design a domain HTTP API | Express commands and read views through a service, using Kubernetes or another store internally | Service authorization, validation, stable outcomes and failure handling |

These choices are distinct from universal versus domain-specific transport. A universal
BFF can serve a purpose-built KRM domain with substantial domain logic behind it. A
domain-specific BFF can also preserve Kubernetes resource shapes.

Decide per capability, not once for the entire application. A Kubernetes object editor
and an anonymous quiz result screen have different contracts even if they share storage.

| Option | Contract and ownership | Best reason to choose it | Main cost |
| --- | --- | --- | --- |
| Universal BFF | krm-foyer owns login and native API/stream transport; Kubernetes extensions enforce domain invariants | Several applications need the same resource access and users can safely exercise all their Kubernetes permissions from the browser | Frontends absorb Kubernetes semantics; admission/controllers may become the domain backend |
| Domain-specific BFF | Application handlers express actions, validate inputs and return suitable views; responses can still be Kubernetes-shaped | Workflows, privacy and error handling differ materially from native CRUD | Per-application handlers and integration tests; shared infrastructure can be duplicated |
| Hybrid | Generic access for approved resources; domain endpoints for commands and restricted views | Resource editing and business workflows coexist | Two access paths need an explicit policy boundary, identity contract and operational ownership |

Choose universal transport when the exposed resource API already expresses the intended
permissions and operations. Choose a domain BFF when a service expresses the product's
commands and read views more directly. Combine them when different features need different
contracts. Neither choice removes the need for domain logic.

## Design the resource lifecycle

A useful resource contract expresses domain concepts, not just database rows with Kubernetes
metadata. For a quiz, one possible design is:

| Resource or field | Intended meaning | Design obligation |
| --- | --- | --- |
| QuizSession intent and status | The desired round lifecycle and the operator's observed readiness/closure | Specify when a round actually stops accepting work |
| QuizSubmission spec | An immutable request containing answers and a round reference | Establish trusted submitter identity and any rules required before storage |
| QuizSubmission status | Pending processing, accepted or rejected, with stable reasons | Define terminal decisions, retries and failure separately; users cannot forge outcomes |
| QuizResults | A read model of accepted submissions suitable for the audience | Specify read grants, update lag and counting each accepted submission once |

In this example, a successful create acknowledges persistence. The UI must distinguish
pending processing from a successful domain outcome. Missing status is not success; a controller outage is not a
business rejection. A useful status contract identifies which intent/revision was processed
and which conditions or decisions are authoritative, including terminality and correction.

Enable and authorize the CRD status subresource so participants cannot write controller-owned
outcomes. Status is a separately writable part of the resource, not automatically a private
read view; use separate resources/grants where disclosure differs. Kubernetes documents
[CRD status behavior](https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definitions/#status-subresource).

This model lets browsers and automation share a domain API and observe durable work through
restarts. It also makes eventual processing visible to the user and makes the domain team
responsible for reconciliation and API evolution. Those are deliberate product choices.

### Assign ownership

Here, a **domain operator** means software that manages the domain through Kubernetes
controllers, potentially packaged with admission. A **platform operator** is the person
or team running it. The [Kubernetes operator pattern](https://kubernetes.io/docs/concepts/extend-kubernetes/operator/)
provides a place for domain expertise; it does not supply that expertise automatically.

| Responsibility | Owner |
| --- | --- |
| Define identity, uniqueness, acceptance, privacy and lifecycle rules | Domain/API designers |
| Implement admission, canonical keys, arbitration and retry-safe reconciliation | Domain operator developers |
| Install policies/controllers, maintain availability, audit grants and manage upgrades | Platform team, with domain-team support |
| Explain pending, rejected, failed and completed outcomes | Frontend team using the domain contract |
| Login and native API/stream transport | krm-foyer maintainers |

The domain operator implements the guarantees assigned to it. They must be
implemented by the right mechanism, not handed to an asynchronous reconciliation loop
and assumed solved. Domain and Kubernetes expertise must be available to design, review
and operate the complete contract; a generic proxy cannot compensate for their absence.

## Evaluate reuse and exposure

A generic service can consolidate login, credentials and resource transport across
applications. It does not necessarily remove DTOs: a domain BFF can already return
Kubernetes-shaped objects. Count all maintained components when evaluating savings,
including session storage, deployment, policies, controllers and tests.

Include the cost of [session storage](design.md#login-and-sessions) and complete the
[grant review](design.md#access) before deploying. Those service
requirements apply regardless of which domain model is chosen.

## The constraints that decide most cases

**Authorization granularity.** A create grant does not express “one reservation per
person” or “accept only while booking is open.” If arbitrary authorized API calls can
violate those rules, implement admission or a deliberate request/acceptance protocol
before exposing the resource. An asynchronous rejection cannot prevent initial storage.

**Read privacy.** A readable resource includes its metadata and status. An application
that shows anonymized totals cannot safely expose the underlying records just because
its users already have list permission. Use separate read grants and aggregate resources,
or keep a domain query endpoint. UI hiding and stream projection cannot protect fields
available through a raw GET.

**Client contract.** With native access, resource versions, schemas and lifecycle states
are a public domain API. The team must maintain compatibility and translate outcomes into
usable screens. Choose this when the resource model is useful to several clients. A domain
HTTP API may better isolate browsers from storage changes or present stable commands.

Other constraints still need explicit design:

| Constraint | Required design work |
| --- | --- |
| Concurrent edits | UID/version guards, patch semantics and understandable conflict handling |
| Multi-object operations | Recovery and partial-success behavior; neither a proxy nor a BFF creates a transaction automatically |
| Reporting | Aggregation, indexing or a separate read model; list/watch is not a general query engine |
| Capacity | Bounded lists/watches and tested write volume; consider another store for high-volume records |
| Operations | Owners for CRDs, admission, controllers, sessions and upgrades |

See [Kubernetes API semantics](https://kubernetes.io/docs/reference/using-api/api-concepts/)
for the native contract. Deliberate KRM modeling can address these constraints, but the
implementation and operational costs remain part of the choice.

## Compare application fits

These are starting recommendations under the stated assumptions, not built-in features.

| Application | Starting choice | What would change the decision? |
| --- | --- | --- |
| Workspace provisioning with observable progress | KRM domain plus universal BFF | An immediate all-or-nothing allocation requirement needs a stronger allocation protocol |
| Shared configuration editor where users may read and edit the resource | Universal BFF with conditional writes | Field confidentiality or narrow write permissions may require separate resources, admission or a domain endpoint |
| Equipment reservations with limited inventory | KRM request/status if pending decisions are acceptable | Guaranteed immediate booking may be simpler in a transactional domain service |
| Customer reporting with private records and flexible queries | Domain query API, possibly beside generic resource access | A small, predefined aggregate resource could serve the required views safely |
| Payments involving external providers | Domain service or operator with provider-supported idempotency | KRM can expose durable intent/status, but cannot itself guarantee a single external charge |

The [workspace walkthrough](vision.md#what-it-takes-from-the-domain) follows a suitable
KRM case end to end. The quiz below examines a harder case: identity-bound uniqueness and
private results. Its mechanisms also apply to applications, registrations and reservations.

## Example: a quiz with one submission per participant

| Requirement | Domain BFF implementation | Native API design must supply |
| --- | --- | --- |
| Correct round and questions | Reads the round, checks live state and submitted UID/version, validates answers | Trusted round lookup or controlled policy parameters, schema/answer validation and an explicit close-race contract |
| One submission per identity per round | Server derives a canonical name from round UID and trusted identity; atomic create rejects duplicates | An enforced deterministic key, not a name suggested by frontend code |
| Trusted attribution | Session supplies identity; backend chooses namespace, labels and timestamp | Admission/controller verification or generation of trusted fields; browser metadata is untrusted |
| No participant changes | No update/patch/delete route; other credential paths must also be restricted | Audit all additive grants and define admission rules and administrator exceptions as needed; prevent delete/recreate bypass |
| Private result presentation | Aggregates submissions, omitting object metadata | Narrow raw reads, provide authorized aggregates or isolate each participant's records |
| Unknown create outcome | Stable server-derived name prevents a second stored vote | Stable enforced identity key plus a safe way to inspect/reconcile outcome; no automatic POST replay |

Omitting metadata is not a promise of complete anonymity: free-text answers can identify
people and Kubernetes administrators retain broader visibility. Specify the intended
privacy boundary before selecting either implementation.

### Three different “only once” guarantees

| Guarantee | Required mechanism | What is insufficient |
| --- | --- | --- |
| At most one stored submission per enrolled identity and round | Trusted identity-to-key mapping, enforced canonical namespace/name, atomic create and retention/deletion rules | A controller that later marks additional stored objects rejected |
| At most one accepted submission, allowing several request objects | A durable decision record per identity/round, atomic winner selection and recovery that preserves that decision | Two workers independently checking for an accepted request and then setting their own status |
| At most one external effect, such as a payment or redemption | An idempotent destination or transactional/deduplication protocol spanning retries and crash recovery | A successful status update, a single controller replica or ordinary leader election alone |

At-most-once is also not a promise of eventual success. Define how pending work recovers
and how a caller discovers an outcome after losing a response. “One person” here means
one trusted enrolled identity; multiple enrollments by the same human require a separate
identity/eligibility policy. Neither Kubernetes nor a deterministic object name proves
physical-person uniqueness.

A domain operator could accept multiple immutable requests, use an atomic canonical
acceptance record to select one, and reconcile each request's status from that durable
record. It must never reselect a winner after a crash or count pending/rejected requests.
This is a viable alternative if the requirement is one counted answer. It does not meet
a requirement that a second request object must never exist. Changing that requirement
needs a deliberate product decision and corresponding API/UX changes.

The stronger storage guarantee still belongs at the creation boundary: admission/policy
and API-server atomic create, possibly using namespaces provisioned by an operator.
A later cleanup loop cannot retroactively prevent storage or disclosure. The
webhook and namespace options below retain this distinction.

### Option A: admission webhook with a shared namespace

A webhook can derive a canonical name using a hash, check identity and round content,
and reject alternate names. A mutating webhook could supply trusted fields; validating
admission must enforce the resulting contract. Kubernetes supplies authenticated request
identity. If a naming scheme uses an OIDC subject while admission sees Kubernetes user
information, define a stable mapping; do not assume the strings are interchangeable.

Keep the uniqueness guarantee in the atomic creation of one canonical object. A webhook
that merely lists existing submissions and rejects a duplicate can admit two concurrent
requests before either is persisted. Avoid side-effecting reservations during admission.
A read of the round in a webhook likewise does not atomically lock it until submission
persistence. Specify whether voting must be open at validation time or whether a stronger
close boundary needs a coordinated command-processing design. A domain BFF that reads
the round and then creates a submission has the same cross-object race.

This is domain code moved to the Kubernetes write boundary, where it can protect all
clients. It does not eliminate domain code. Budget for webhook certificates, permissions,
latency, availability, timeout behavior and fail-closed enforcement of critical rules.
Scope it narrowly so its outage does not block unrelated resource writes. Webhooks do not
filter ordinary GET/list/watch responses, so read privacy needs a separate solution.
See [dynamic admission control](https://kubernetes.io/docs/reference/access-authn-authz/extensible-admission-controllers/).

Choose this when all writers should obey these rules and the team already operates admission.
For a single quiz UI it may be more infrastructure than a small handler warrants.

### Option B: declarative admission with redesigned identity keys

CRD schema/CEL can enforce object-local constraints. ValidatingAdmissionPolicy can also
use request identity and configured parameter resources; it is not an arbitrary API
lookup facility. Round-dependent checks need a trusted, explicitly bound source and a
plan for its updates and absence. See [ValidatingAdmissionPolicy](https://kubernetes.io/docs/reference/access-authn-authz/validating-admission-policy/).

The documented Kubernetes [CEL libraries](https://kubernetes.io/docs/reference/using-api/cel/)
do not provide a SHA-256 primitive for a hash-derived naming scheme. Such a scheme
therefore needs a different implementation or key design. A key could combine a trusted, stable,
name-safe participant identifier and round UID, then admission validates the relationship.
Simply concatenating an arbitrary username is insufficient: length, disallowed characters,
collisions after normalization and identity changes must be addressed. Kubernetes has
[resource naming restrictions](https://kubernetes.io/docs/concepts/overview/working-with-objects/names/).

A provisioned identifier mapping may be justified when it is already part of enrollment.
Creating it solely to avoid a small hashing webhook could make the design more elaborate.
These are design options, not tested CEL expressions or a claim of compatibility with
every cluster version.

### Option C: one namespace per person, with an enforced round name

Provision a namespace and narrow RoleBinding for each stable enrolled identity. Keep
shared rounds elsewhere. Store each person's submission under a canonical name derived
from the round UID, such as `round-<uid>`. With an enforced name, atomic create gives
one submission per person per round without hashing the person into the object name.
A literal name `submission` permits only one retained submission per personal namespace;
for multiple rounds it requires namespace-per-person-per-round or another storage design.
Deleting the previous submission sacrifices history and can reopen duplicate voting.

A namespace alone does not force that name. RBAC cannot restrict top-level create by
`resourceNames`; admission must reject other names and verify the round reference.
RBAC grants are additive, so retaining shared or cluster-wide read grants defeats the
intended isolation. Selectors and owner labels are not authorization boundaries.
These restrictions follow from [Kubernetes RBAC](https://kubernetes.io/docs/reference/access-authn-authz/rbac/).

This option offers private reads of one's own submission and simplifies identity scoping.
It still needs answer/live-round validation and trusted enrollment-to-namespace mapping.
Participants must not create extra personal namespaces, change ownership mappings or
acquire broad bindings. Provisioning and cleanup become product operations: roughly 200
participants imply roughly 200 namespaces plus bindings, with more objects for per-round
isolation. Measure that lifecycle; it is not automatically too large or automatically free.
A trusted aggregator still needs cross-namespace access and an audience-readable result
resource. Consider quotas and retention alongside privacy.

Choose this when personal workspaces already make sense for the product. Creating a
namespace, binding, identity mapping and aggregate controller solely to express one quiz
submission may cost more to build and operate than a small domain handler.

### Protect results and individual submissions

For a shared namespace, remove audience raw submission reads, including watch, and expose
a separately authorized aggregate. A controller may read submissions under an explicit
service identity; participants must not forge aggregate contents or status. Define lag,
round UID scoping, rebuild/deduplication and text disclosure rules.

If an existing result handler reads using the participant token, narrowing grants also
breaks that handler. Ship the replacement read model and client route before, or atomically
with, that RBAC change. In a personal-namespace design, allow only each person's own raw reads
and aggregate reads; audit other grants too. Admission cannot substitute for this cutover.

## Compare complexity and product fit

Moving rules into Kubernetes is natural when they protect several clients, represent
durable domain intent or observable lifecycles, and fit existing operational ownership.
That includes business resources; they do not have to be infrastructure resources. It becomes forced
when most resources, namespaces and controllers exist to reconstruct one UI command and
its result, with little independent value.

Evaluate each candidate against the same requirements:

1. State the user operation and every invariant, including privacy and concurrency.
2. Identify where each invariant holds if the browser sends arbitrary allowed API calls.
   An unspecified or browser-only enforcement point blocks raw exposure.
3. Count the full implementation: frontend glue, handlers, policies, webhook/controller
   code, manifests, session storage, test fixtures and on-call dependencies.
4. Describe failure handling: expired login, lost create response, round closure, failed
   webhook, lagging aggregate and partially completed workflow. Compare the user experience.
5. Confirm who has both the domain knowledge and Kubernetes expertise to maintain
   the API, admission, reconciliation, version compatibility and failure recovery.
6. Name the second real consumer of the reusable layer. If none exists yet, justify the
   work as product development rather than immediate application simplification.

Prefer a universal BFF when resource operations already match intended permissions and
native semantics, and reuse outweighs the operational overhead. Prefer a domain BFF
when immediate commands, restricted queries or transaction-like workflows are more clearly
expressed by a service. An asynchronous command with a useful durable lifecycle can still
be a good KRM resource; adding status alone is not evidence that it is. Prefer a hybrid
when these answers differ by feature. Do not decide by endpoint count alone.

## Record the adoption decision

Complete this table for the application being designed. Reject an option if its required
guarantees lack an implementation and owner, even if its transport is simpler.

| Decision | What to record | Evidence |
| --- | --- | --- |
| Domain contract | Existing resource API, deliberately designed KRM domain, domain HTTP API or a combination | Resource/command definitions, lifecycle and compatibility policy |
| Access scope | Exact raw API, stream and domain routes | Effective grants for the application's users, and bypass tests |
| Uniqueness | Stored-object, accepted-outcome or external-effect guarantee; identity scope and retention | Concurrent requests, forged identities, deletion/recreation and crash-recovery tests |
| Read privacy | Who can read raw records, status and aggregates | Read grants, disclosure tests and migration dependencies |
| Processing | Admission rules, controller decisions, retry behavior and partial-success handling | Failure tests and user-visible outcomes |
| Expertise | Domain, controller/admission, frontend and platform owners | Review coverage and operational support plan |
| Reuse | Applications and clients benefiting from the common layer | A second consumer without application-specific gateway code |
| Operations | Session storage, controllers/webhooks, upgrades and capacity targets | Load/failure tests and full-system maintenance estimate |

A good KRM design gives several clients a durable, observable domain API. A good domain
BFF gives clients a clear command and query contract. Select the design that meets the
product's guarantees with a system the team can reliably build and operate.
