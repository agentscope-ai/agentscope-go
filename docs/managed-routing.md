# Managed task routing

The experimental `managedtask.Router` implements stage 4 of
[RFC #16](https://github.com/agentscope-ai/agentscope-go/issues/16), using the
[fixed managed executor](managed-task.md), [classifier contract](classifier.md)
and optional [Jev adapter](jev.md). This is source-checkout behavior; it does not
establish availability in a published release. The stage documentation checklist
is tracked in the RFC discussion.

```bash
go run ./examples/managed_routing
```

The offline example demonstrates rules, simulated classification, an explicit
default, generation fallback and unqualified production rejection. Generation
uses actual loopback HTTP attempts; classification is `SimulatedAttempt` and
excluded from costs. Identities, usage, thresholds, sizing and completions are
synthetic. No credentials, live service, production qualification or savings
claim is involved.

## Registration and precedence

Construct `managedtask.NewRouter(managedtask.RouterConfig{...})` and share the
returned instance. `Router.Execute(ctx, task)` returns the same frozen `Result`
and statuses as `Executor.Execute`. A nil router/context or invalid task is
rejected before callbacks/dispatch. Empty mode means production. Unlike the
fixed executor, the router permits an empty `Task.Category` to request routing.
Task identity, mode and any nonempty category belong to trusted host code;
untrusted task text must not fill them or grant permissions.

1. A nonempty trusted `Task.Category` selects `Categories[category]` immediately.
2. Otherwise, local `Rules` run in configured order. Empty category means a miss;
   the first nonempty category wins. Rules are trusted local callbacks, must honor
   context, and must not make hidden external calls.
3. If every rule misses, the optional classifier evaluates one logical operation.
4. Validated categories select host-configured pools, which select deployments.
5. Each candidate enters the same managed execution checks as a fixed deployment.

An unknown metadata/rule category or rule error goes directly to the explicit
default policy; it does not ask Jev to override trusted information. Classifier
failure (including host authorization denial and capacity exhaustion during
classification), malformed answers/reports, mismatched version, low confidence,
insufficient information and conflicting decisions also require that default.
Without `Default`, these cases return `Rejected` with `ErrRoute`. Cancellation
and deadline errors always terminate; they never trigger a default call.

`DefaultRoute{Category, PoolID}` explicitly permits all otherwise undecidable
tasks to use that category/pool, subject to the current deployment authorization,
qualification, sizing and capacity checks. Its category must map to the same pool
in `Categories`. Configure it only if that general applicability is intended;
use host authorization to deny individual tasks or destinations.

`RouterConfig.Deployments` contains ordinary PR3 `Config` registrations, each
binding a stable `Deployment.ID` to the client's exact endpoint/model,
capabilities, context window, qualification evidence, disclosure callbacks and
optional prices. Duplicate IDs and invalid registrations are rejected. Registry
configuration and nested maps/slices/prices are snapshotted. Clients and host
callbacks remain shared trusted, concurrency-safe code.

`Qualification` remains a host assertion bound to deployment ID, endpoint,
version and category, with dataset/rubric versions, sample size and quality/latency
margins. The router does not produce or certify this evidence. Production members
without matching qualification are excluded from weighted selection; each
fallback checks qualification again. Evaluation bypasses only qualification and
requires host-enforced isolation. Revocation belongs to current host policy.

## Pools, weights and fallback

`Pool` has a unique ID, positive integer-weighted `Members`, explicit ordered
`Fallback` deployment IDs and `AttemptsPerDeployment` (zero selects **1**).
References, duplicate members/fallbacks, zero weights and weight-sum overflow are
validated. SHA-256 of task ID plus pool ID chooses a weighted member among the
qualified members. This choice is stable for the same task ID, pool and registry;
it is neither a random load balancer nor a persistent route across configuration
changes. Use unique task IDs. Independent tasks never mutate a shared Agent.

Only the chosen member and explicitly listed fallbacks can run. The selected ID
is skipped if it also appears in `Fallback`. If no weighted member is qualified,
explicit fallback candidates may still be examined and must pass qualification.
Every fallback rebuilds projected text/final JSON and rechecks authorization,
capabilities, qualification, context bound plus output reservation, admission,
remaining attempts and deadline. A failure grants no new permission or quality
qualification. Exhaustion of the shared attempt cap rejects with `ErrCapacity`.

Within a candidate, only typed retryable failures retry, with the PR3 backoff and
`Retry-After` handling. After that candidate's local limit, the next configured
deployment can be tried immediately, within the task's shared limits. Failures
and pre-dispatch rejection can advance to fallback. `Succeeded`, cancellation,
timeout, `Unsupported`, `Filtered` and `Truncated` stop the task. Tool responses
are unsupported; partial/filtered/truncated text is never a successful completion.
There is no automatic quality-based escalation or whole-Agent rerun.

## Optional classification and disclosure

`ClassificationConfig` requires a `classifier.DispatchClassifier`, explicit
`ClassificationTarget{ID, Endpoint, Model}`, `MaxAttempts`, `Authorize`, `Project`
and `Decisions`. The target must exactly match `Client.Target()`; classifier IDs
must not collide with generation IDs. The existing Jev client and offline Fake
implement this opt-in interface. Ordinary `Classifier.Classify` stays compatible;
a classifier implementing only that method cannot enter managed routing.

`Authorize(ctx, task, target)` runs **before** `Project` and provider request
construction. `Project(ctx, task, target)` must return only permitted state and
question data. It must cover instructions, candidate descriptions and question
IDs as well as task text: questions are also disclosed data. It returns a
`classifier.Request`; the router assigns `OperationID = task.ID +
"/classification"`, validates and owns the projected state/questions across retries.
Do not put secrets in task IDs or configured identifiers.

The adapter prepares its request, then invokes the managed dispatch guard
immediately before dispatch. The guard rechecks current authorization, context,
terminal state and shared attempt capacity, and commits a placeholder attempt.
If policy changes to require a different projection, authorization must reject
the already prepared request. If Jev is denied, projection/request construction
never occurs and there are zero Jev requests. Denial at the dispatch guard also
records zero attempts. An allowed generation default remains subject to its own
disclosure policy.

The opt-in `ClassifyWithDispatch` receives `classifier.DispatchLimits` to narrow
its complete request/response byte caps to the router's limits. Jev enforces
these caps on its actual encoded JSON and decoded body; its own stricter adapter
caps still apply. Fake bounds its synthetic representation. The guard is a
trusted-adapter contract, not sandboxing: custom implementations and transport
hooks must not bypass it or dispatch hidden retries. A successful report without
one guarded, matching attempt is rejected. Ordinary unguarded adapter calls do
not receive managed protection.

This first router consumes **Choice** questions only; the independent classifier
still supports Binary and Score for other callers. Each `ChoiceDecision` contains
an exact `QuestionID`, `Question` (instructions, candidates and descriptions),
`MinConfidence` and a complete choice-to-category `Categories` map. An empty
mapped category means insufficient information. Projected questions must exactly
match these configured questions. Every decision must meet its own threshold and
agree on a nonempty category; marginal answers are not joint confidence.

Thresholds are bound to this exact question/candidate set and configured classifier
model version; the returned version must match. No universal threshold is supplied.
The example's 0.8 is synthetic and does not establish Jev calibration or Chinese/
English routing quality. Hosts need independent evaluation before production use.
Jev returns category choices, never permissions, pool weights or arbitrary URLs.

For a real Jev client, use `jev.New(jev.Config{APIKey: key, Model: pinnedVersion,
Endpoint: approvedEndpoint})` and bind `client.Target()` in `ClassificationTarget`.
Keep keys in host credential handling. `Project` must return the exact approved
Choice questions configured in `Decisions`. Live protocol tests and credentials
are documented in the [Jev guide](jev.md#verification); the
[evaluation workflow](managed-routing-evaluation.md) provides an explicit live
Jev/generation pilot and offline three-group comparison. No live provider is invoked by the offline example or default tests.

## Shared limits, accounting and result ownership

`RouterConfig.Limits` selects the same finite defaults as PR3. Deployment-level
`Config.Limits` must be nil: independent limits would incorrectly reset the task
budget. All classification and generation targets in one router share:

- `MaxTasks`, retained through host callbacks, backoff and local cleanup;
- FIFO `MaxConcurrent`/`MaxQueued` admission and per-wait `QueueTimeout`;
- one overall `TaskTimeout`, shortened by the caller deadline;
- total `MaxAttempts`, request/response byte limits and generation output reservation.

Classifier `MaxAttempts` additionally bounds primitive classification calls;
`Pool.AttemptsPerDeployment` bounds calls within each generation candidate. Both
must fit the total cap. There is at most one logical classification operation;
retries reuse its ID. No layer retries internally. Simulated dispatches
conservatively consume the total cap, but remain distinguishable from physical
requests and are excluded from cost arithmetic. A zero-attempt error is never
charged as a dispatch. Queue wait/backoff holds no active request permit.

Each guarded dispatch occupies exactly one `Result.Attempts` record, numbered
across classification and generation. Classification records have nonempty
`OperationID`; generation records leave it empty. Returned classifier reports
complete that existing placeholder; they are not appended again. Recognized usage
from failed classification/generation survives. Invalid accounting is retained
as an unknown guarded attempt, never treated as a validated answer.

Optional generation and classifier `Prices` must use the same currency.
Classifier prices support input/output rates, with no cached-price field.
`Cost.KnownSubtotal` includes recognized priced components from physical
classification, failed generation and fallback attempts. Missing usage/prices,
version mismatches and unfinished calls remain `UnknownAttempts`. A zero subtotal
with unknown attempts does not mean free work. `StrictBudget` is rejected on both
router and registrations; no strict monetary bound is promised.

Cancellation can return before local callbacks/transports finish. The worker
retains its task slot and any active request permit until cleanup actually ends.
Late work cannot change the returned route, text, attempts or cost; unfinished
attempts have `Completed=false` and unknown usage. This cannot prove remote
computation stopped. The router emits no logs and omits raw host/provider errors.
`Result.Route` records configured source/category/pool/last examined deployment
only; it contains no task text or classifier answer. Provider model metadata and
successful text remain untrusted data.

## Validation and Python mapping

```bash
go test -race -count=1 ./pkg/agentscope/managedtask ./pkg/agentscope/classifier/...
go run ./examples/managed_routing
```

Local fixtures test precedence, forbidden classification, typed decisions,
shared admission/limits, fallback checks, concurrency, failed-attempt costs,
cancellation and frozen results. They do not establish live compatibility,
qualification, routing quality or savings. The [stage 5 evaluation harness](managed-routing-evaluation.md) runs
fixed/rules/rules-plus-classifier comparisons directly through this boundary.
Live benefit and production qualification still require independent evidence. Admission remains
single-process and instance-local; ordinary Agent, streaming and helper paths
are outside this boundary. The governance contract aligns with
[RFC #11](https://github.com/agentscope-ai/agentscope-go/issues/11), without a
dependency on the unmerged shared-inference proposal.

The inspected [Python router](https://github.com/agentscope-ai/agentscope/blob/083cbd1975c7b5ed055c69b0d19c150727e7f606/src/agentscope/middleware/_model_router.py)
selects a candidate and temporarily replaces `agent.model`, retaining the original
on failure. Go deliberately uses a task-owned route, category-qualified host pools,
explicit defaults/fallbacks and the managed execution boundary, with no shared
Agent mutation or structured-output ChatModel fallback.
