# Evaluate managed task routing

Stage 5 of [RFC #16](https://github.com/agentscope-ai/agentscope-go/issues/16)
adds `evalkit.Runner.RunManagedLoad`, an integrated offline example and an
explicit live experiment mode. Follow the [fixed executor](managed-task.md),
[routing](managed-routing.md), [classifier](classifier.md) and [Jev](jev.md)
guides for the underlying contracts. These Experimental APIs are available in
this source checkout; this guide does not establish published-release availability.

## Run the workflow without credentials

From the repository root:

```bash
go run ./examples/managed_routing_eval -split tuning -revision "$(git rev-parse HEAD)" > /tmp/routing-tuning.json
go run ./examples/managed_routing_eval -split acceptance -revision "$(git rev-parse HEAD)" > /tmp/routing-acceptance.json
go test -race -count=1 ./examples/managed_routing_eval ./pkg/agentscope/replay/evalkit
```

Record local diffs alongside the source revision when evaluating an uncommitted
checkout. Reports include the complete experiment plan, task inputs, gold rubric,
arrival schedule, terminal observations and managed attempt snapshots. Store them
locally with appropriate access controls; do not publish sensitive task reports.

Three groups execute the same inputs and arrival schedule:

| Group | Managed entry point | Policy |
|---|---|---|
| Fixed | `Executor.Execute` | Baseline deployment; host supplies category `general` |
| Rules only | `Router.Execute` | Prefix rules, then explicitly applicable general default |
| Rules plus classifier | `Router.Execute` | Same rules; classify misses, then qualified host pools/default |

The default is explicitly applicable to every task in this evaluation corpus.
The example authorizes only the selected corpus's input texts, Evaluation mode,
and exact configured destinations/versions. Projection sends only that input;
gold answers, labels and rubric stay in the host. Classification questions are
fixed, public task descriptions and are part of the approved disclosure. Every
dispatch rechecks authorization. The static allowlist demonstrates the callback
boundary; a production host must also check current policy and revocation.

Offline generation uses actual loopback HTTP. Classification delegates to
`classifier.Fake` and is recorded as simulated, excluded from physical requests
and costs. Both use explicit lookup tables. Model identities, prices in
`fixture-credits`, usage, confidence and input bounds are synthetic. Candidate
extraction returns 529 without usage; the configured baseline fallback completes
it. Candidate classification completes directly; rewriting selects the baseline.
This intentionally demonstrates that failed attempts and fallback raise request
counts and leave incomplete cost accounting. It measures neither language
understanding nor economic benefit. Tests separately exercise incorrect answers,
rejections, cancellation, deadlines and unfinished callbacks.

No qualification records are created. All candidates remain disabled for
production: the example's host authorization rejects Production and there is no
`Deployment.Qualifications` evidence. The existing
[offline routing example](../examples/managed_routing/main.go) also demonstrates
production rejection. Evaluation bypasses only qualification; authorization,
projection, capabilities, sizing, admission, deadlines and attempt caps still apply.

## Dataset and proposed acceptance experiment

The versioned [seed corpus](../examples/managed_routing_eval/dataset.json) is
self-authored public synthetic text, with no personal/customer data. It contains
12 tuning and 12 acceptance tasks; each split has two Chinese and two English
tasks per category. IDs and exact inputs are disjoint. They share templates and
are a pilot corpus, not an independent representative benchmark. Tune question
wording, prefix rules, confidence and pools on tuning only; freeze them before
acceptance. Do not tune on the acceptance report.

| Category | Independent rubric `managed-text-rubric-v1` |
|---|---|
| Information extraction | Entire response must be a JSON object with exactly the expected string fields and values |
| Closed-set classification | Trimmed output must equal the expected `positive` or `negative` label; no explanation |
| Constrained rewriting | All required phrases, no forbidden phrase, nonempty output and explicit Unicode character limit |

Scoring is deterministic host code implementing the existing `evalkit.Scorer`
contract. Jev never scores output. The rewrite rubric checks stated constraints,
not all semantic quality; larger acceptance work needs blinded human checks for
meaning preservation and harmful changes. Extraction rejects extra fields or
non-JSON framing; ordinary JSON duplicate-key semantics still apply.

A concrete **proposed, unqualified** live comparison uses baseline
`gpt-4.1-2025-04-14`, candidate `gpt-4.1-mini-2025-04-14`, and classifier
`jev-1.13.0`. The generation snapshots are documented in the official
[GPT-4.1](https://developers.openai.com/api/docs/models/gpt-4.1) and
[GPT-4.1 mini](https://developers.openai.com/api/docs/models/gpt-4.1-mini)
model references. Their presence in a plan does not establish access, endpoint
compatibility, context bounds, Jev calibration or task qualification. The live
plan must pin the actual host destinations, returned versions, server settings
(including sampling/cache behavior), quotas and trustworthy input bounds. Prices
are host supplied per-token rates, not hard-coded provider quotes.

Proposed pilot margins are at most **0.02 absolute loss** in mean quality per
planned task against fixed, per category, and **5 seconds P95** end-to-end
latency. The example emits observed per-category `Comparisons`; incomplete
execution/scoring cannot meet its pilot margins. Neither passing these checks nor
a successful process exit grants production qualification. Four samples per
category cannot substantiate a 2% margin. For a qualification study, expand to at
least 200 held-out tasks per category and language, pre-register repeats/load,
measure confidence intervals for paired quality differences, and require the
interval to satisfy the chosen margin. This sample size is a proposed starting
point, not a statistical guarantee. Require no critical constraint violations,
complete accounting for economic claims, and a host review of quality evidence.
A rerun on the same seed corpus is not additional independent evidence.

`Qualification` must bind real evidence to deployment ID, full endpoint, model,
category, dataset/rubric versions, actual sample size and quality/latency margins.
The framework checks those fields' completeness, not their scientific validity.
Missing experiment metadata, fixtures alone or a pilot margin PASS must never
be converted into such a record. Revocation and final approval belong to the host.

## Opt into live Jev and generation

Copy [live-plan.example.json](../examples/managed_routing_eval/live-plan.example.json)
to a local file. It is deliberately disabled: isolation is false, context/bound
values are zero, and evidence strings are empty. Supply the actual approved
HTTPS destinations, pinned versions, source revision and server evidence. Keep
keys in the named environment variables, outside the plan:

- `TYPESAFE_API_KEY` for Jev;
- `ROUTING_BASELINE_API_KEY` and `ROUTING_CANDIDATE_API_KEY` for the proposed candidates.

`BaseURL` is the generation base without `/v1`; the adapter appends
`/v1/chat/completions`. Jev `Endpoint` is a full URL. The plan's three category
bindings, fixed target and broadly applicable general default must refer to
registered IDs. `VersionEvidence` records how the version and server settings
are fixed; the example cannot certify that an arbitrary model name is immutable.
The 0.8 Jev threshold is an experimental starting value for this exact question,
candidate set and pinned version; calibrate it on tuning before acceptance.

For each deployment, `InputTokenUpperBound` must be a host-certified bound over
**every final JSON request up to 4096 bytes plus server framing/hidden overhead**.
Record its justification in `BoundEvidence`, and supply the actual configured
`ContextWindow`. The runner reserves 128 output tokens; their sum must fit.
`CountTokens`, observed averages and `len(wire)+32` are not acceptable live
bounds. The latter exists only for offline fixtures. If no trustworthy bound is
available, keep live mode disabled. An evidence string is an operator assertion,
not a proof validated by the example.

Optional `Prices` use the existing `managedtask.Prices` fields `Currency`,
`InputPerToken`, `CachedInputPerToken` and `OutputPerToken`; omit unavailable
rates. Jev does not support cached pricing. All configured prices must use one
currency. Missing prices or usage remain unknown, including on failed calls.
With a complete plan and credentials:

```bash
go run ./examples/managed_routing_eval -live -config /absolute/path/live-plan.json > /tmp/routing-live.json
```

To use a local System One classifier instead of hosted Jev, opt in with the
same [local overrides](jev.md#local-system-one-configuration-w0):

```bash
SYSTEMONE_LOCAL_ENDPOINT=http://127.0.0.1:8080/v1/systemone \
  SYSTEMONE_LOCAL_MODEL=YOUR_EXACT_SERVED_MODEL_NAME \
  go run ./examples/managed_routing_eval -live -config /absolute/path/live-plan.json > /tmp/routing-live.json
```

These variables override only `Jev.Endpoint` and `Jev.Model`; the effective values
are recorded in the report's experiment plan. The optional
`SYSTEMONE_LOCAL_API_KEY` defaults to `local-systemone`, and hosted credentials
are never reused for the local classifier. Keys are not written to the plan.
The same overrides apply to `TestLiveExperiment` below. Offline mode ignores
them and sends no requests to that destination. The original hosted validation
remains the default with all `SYSTEMONE_LOCAL_*` variables unset.

Generation candidates still require HTTPS destinations, credentials, pinned
version evidence, certified context bounds and explicit evaluation isolation;
the local override does not enable local generation or qualify a deployment.
The router's Choice question has four options, satisfying Piccolo's minimum of
two. The exact served model name and non-empty key requirements are documented
in the linked guide. This prepares W1; local fixtures do not establish real
Piccolo compatibility or quality.

This makes billable classification and generation requests. The example permits
three attempts per task (classification plus generation/fallback), one classifier
attempt, ten-second task/arrival deadlines, 4096-byte requests, 128 output tokens,
four in-flight arrivals and arrivals every 25 milliseconds. Groups run in fixed
order, so warmed caches and provider changes can confound comparisons. For a
larger study, counterbalance group order and record cache/rate-limit conditions.
The reusable API accepts a caller's own schedule and preconfigured executors.

Optional live quality test, with a local report destination required up front:

```bash
ROUTING_LIVE_TEST=1 ROUTING_LIVE_CONFIG=/absolute/path/live-plan.json \
  ROUTING_LIVE_REPORT=/tmp/routing-live-test.json \
  go test -run '^TestLiveExperiment$' -count=1 -v ./examples/managed_routing_eval
```

Default tests skip this test. Missing explicit live settings fail before sending.
For standalone live adapter compatibility use the
[Jev contract test](jev.md#verification); compatibility and quality are separate.
A successful live test means it saved the report, not that quality or savings
passed. No live compatibility or quality results are bundled with this change.

## Reuse the managed evaluator

Bind `ManagedLoadConfig.Execute` to a shared `router.Execute`. For a fixed
executor, a wrapper supplies its configured trusted category. `RunManagedLoad`
passes only Input, unique run/arrival ID, empty Category and Evaluation mode;
gold `Categories[taskID]` are solely for reports. Host callbacks remain responsible
for isolation and disclosure. Ordinary ChatModel and Agent paths are not evaluated.

The API reuses `bench.RunOpenLoop`, `WorkloadManifest`, `TaskArrival`, bounded
`scoreLoad` and the existing scorer interface. Specify all generator and scoring
limits explicitly. Agent settings on `Runner` do not apply. Task System, Turns,
Tools, Fixture, Budget, Sampling and TaskSpec Repeat greater than one are rejected;
use explicit arrival repeat numbers. Default scorers must grade text, not tool
trajectories or agent budgets. Custom scorers must honor context and be safe for
the configured scorer concurrency.

Scoring starts after execution observations freeze and uses an independent
context, never the canceled callback context. It scores only accepted successful
managed completions. Execution and scoring statuses remain separate. Late work
cannot overwrite returned reports; callbacks/scorers that ignore cancellation
may outlive observation. Stop them before repeated runs. Managed instances retain
request/task capacity through actual local cleanup. Cancellation does not prove
remote computation stopped. Error objects returned by callbacks must be immutable;
JSON snapshots render their messages as strings, not implementation objects.

## Read the report

Both overall and gold-category summaries preserve all planned arrivals:

- `Planned`, `Completed`, `Scored`, `Qualified`: planned count, on-time successful
  managed completions, scored successes, and quality-passing on-time completions.
  `Qualified` describes output quality only, not deployment qualification.
- `CompletionRate` divides successful completions by all planned arrivals.
  `QualityPerPlanned` treats failures/unscored tasks as zero.
  `MeanScoredQuality` uses only scored successes and is null with no scores.
- `Statuses` and `ArrivalStatuses` distinguish managed terminal results from
  generator rejection, timeout, cancellation, unfinished and not-offered work.
  Every arrival remains in `Load.Results`; nil managed result means unobserved.
- `CompletionLatencies` gives min/max/mean/P50/P95/P99 for all accepted callback
  completions, including errors, measured from scheduled arrival; `LatencySamples`
  states their count. Unfinished or
  unstarted work has no latency sample. Inspect completion rate alongside it.
- `PhysicalRequests`, `ClassificationRequests`, `GenerationRequests`,
  `SimulatedAttempts`, `AdditionalGenerationRequests` expose classification and
  extra generation attempts. Additional generation includes retries/fallback,
  not necessarily fallback alone. Per-attempt target, duration, error and usage
  remain available. `ClassificationAttemptTime` sums physical attempt durations,
  not classifier queue time or complete wall-clock routing overhead.
- `KnownCostByCurrency` totals recognized amounts from **all observed tasks**, not
  just successful tasks. It includes classification, failed generation and
  fallback. `UnknownCostAttempts`, `UnobservedExecutions`, `UnfinishedAttempts`
  preserve gaps; request counts are lower bounds when execution is unobserved.
- `KnownCostPerQualified` divides that entire known subtotal by quality-qualified
  completions, separately by currency. It remains a lower-bound subtotal when
  costs are missing. `CostPerQualified` is null for zero denominator, unknown
  costs, unfinished attempts, unobserved execution or multiple/no currencies.
  A zero known subtotal with unknown items does not mean free work.

The ledger supplies task totals, not independently priced classification/fallback
components. No strict monetary budget or billing reconciliation is promised.
No report, even a complete one, automatically creates production evidence.

## Evidence levels and Python reference

| Evidence | What it establishes | What remains unverified |
|---|---|---|
| Offline fixtures/default tests | Flow, HTTP fixture contract, rubric and reporting arithmetic | Real service compatibility, quality and savings |
| Explicit live protocol check | One configured adapter/endpoint/version contract | Routing benefit and task quality |
| Explicit live quality pilot | Observed outcomes for its saved plan/data/environment | Generalization, statistical qualification and production approval |

Python's [model router](https://github.com/agentscope-ai/agentscope/blob/083cbd1975c7b5ed055c69b0d19c150727e7f606/src/agentscope/middleware/_model_router.py)
selects models by middleware and temporarily replaces `agent.model`. This Go
experiment deliberately exercises task-owned Executor/Router execution, keeping
policy and accounting within the boundary built in stages 3–4. It neither mutates
Agent nor imports the still separate [RFC #11](design/managed-inference.md)
proposal. See the [stage checklist](https://github.com/agentscope-ai/agentscope-go/issues/16#issuecomment-6092790238).
