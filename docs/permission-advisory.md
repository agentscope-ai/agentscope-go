# Shadow-mode permission advice

The [permission_advisory example](../examples/permission_advisory/) implements
[W3 issue #127](https://github.com/agentscope-ai/agentscope-go/issues/127), following
[RFC #109](https://github.com/agentscope-ai/agentscope-go/issues/109). It composes
the `OnCheckPermission` middleware hook wired into `UnifiedAgent` with the
existing classifier client.
It is a controlled host example, not a general approval framework. W4 context
retention is outside its scope.

## Run without a model

From the repository root:

```bash
go run ./examples/permission_advisory
go run ./examples/permission_advisory -review=false
```

The default uses an in-process HTTP classifier fixture. The chat model, tool and
user decisions are scripted. There are two independent turns: a classifier
recommends `allow_once` in both; a scripted user denies the first and approves
the second. Each turn still emits one `RequireUserConfirmEvent`. The synthetic
`read_fixture` tool executes zero times after denial and once after approval;
it never reads a file. JSON lines on stdout contain redacted audit records and
summaries with `scripted_user: true`. This demonstrates the permission boundary,
not classifier quality or real human interaction.

`-review=false` installs an inert reviewer: no classifier client, environment
lookup, classification or advisory audit. Both scripted confirmation outcomes
still run. No model downloads or credentials are needed for either command.

## Optional local classifier

Start a compatible local server separately using the [Piccolo guide](piccolo.md).
This example neither starts nor downloads it. Then opt in explicitly:

```bash
SYSTEMONE_LOCAL_ENDPOINT=http://127.0.0.1:18609/v1/systemone \
SYSTEMONE_LOCAL_MODEL=Piccolo-4B-Q8_0 \
go run ./examples/permission_advisory -live
```

Use the exact served model identifier. `SYSTEMONE_LOCAL_API_KEY` is optional;
the existing local configuration supplies a local placeholder if omitted. Only
loopback URLs are accepted; URL validation does not establish process isolation
or prove where a local proxy forwards requests. There is no hosted fallback or
implicit hosted credential. `-live -review=false` is rejected. Only classification
becomes live; chat, tool execution and user confirmation remain scripted.
`evidence: "live-classifier"` identifies the selected transport, not a successful
model response: inspect `status`, attempts and token fields as well.

## Eligible decisions and disclosure

The reviewer first calls `next` and returns the complete original `Decision`
unchanged, including metadata. Engine errors and nil results pass through. It
reviews only Default-mode fallback `Ask` with the typed
`permission.OriginDefaultAsk` origin and no bypass-immune safety flag. Explicit
ask/deny rules, deterministic allow/deny, safety decisions and other modes never
dispatch classification. No decision reason or message is parsed.

`Decision.Origin` is additive provenance, not authorization. The engine sets
`default_ask` only on that final Default branch and clears a tool checker's origin.
Missing, unknown and future values are ineligible. `OriginUnknown` (the empty value) serializes
without an `origin` key; non-empty future values round-trip but remain ineligible.
The eligible fallback now adds `"origin":"default_ask"`
to serialized decisions. Older JSON without this field still decodes. Consumers
that compare whole decisions or enforce an exact JSON schema should account for
this addition. This is an unreleased addition.

The host projector accepts only the exact synthetic user intent, `read_fixture`
and the single argument `target: "fixture.csv"`. It constructs a static operation
summary; extra fields and alternate targets are refused. It sends no transcript,
raw permission metadata, file content or arbitrary user intent. It accepts the
local backend type with an empty backend working directory only; nonlocal
identities and backend working-directory overrides are refused. To adapt this
to a real tool, the host must define an explicit destination and disclosure policy,
not forward the raw hook input or rely on truncation/redaction as authorization.

The hook's integration scope remains fresh sequential non-external tool calls in
interactive `ReplyStream`/Conversation turns with an engine. Buffered `Reply`,
all resume paths, restored/reprompted calls, concurrent-dispatch batches (including
a multi-call batch limited to width one), preflight and loop/direct-orchestrator
checks do not dispatch the classifier. See [middleware scope](middleware.md#oncheckpermission).
The runnable demo uses fresh `ReplyStream` turns; tests also exercise excluded
entry points. Other middleware remains trusted host code and could independently
change a decision; this example installs only its own reviewer.

## Bounds, bindings and ownership

One reviewer admits at most one classification at a time, with no queue or
retry. Each reply admits at most 16 distinct call IDs, once per call. Classifier
requests have a five-second context deadline, 4 KiB request and 64 KiB response
limits, and exactly one guarded physical or simulated attempt. The existing Jev
client honors cancellation. An arbitrary replacement `DispatchClassifier` must
honor its context and synchronous dispatch contract; the example does not launch
abandoned timeout goroutines to contain an uncooperative client.

The host binds advice to the reply scope, tool-call ID, agent/tool names, parsed
arguments, permission context, host policy generation, workspace/backend identity,
and expected classifier endpoint/model. The host policy owner snapshots under a
short lock, then releases it before classifier I/O or user confirmation. All policy
and environment writers must use that owner and advance the generation; this is
not a new engine-wide policy transaction API. The demo submits only one-shot user
confirmations, never persistent rules or working-directory grants.

Bindings are checked before classifier dispatch, before emitting advice and in
the innermost `OnActing` hook immediately before the synthetic tool runs. A model
mismatch, invalid response/report, duplicate/late result, timeout, cancellation,
changed binding or full scope discards advice. Pending advice expires after five
seconds, is consumed once, is invalidated on changed arguments/policy/environment,
and is cleared when a reply ends (including human denial). It cannot authorize a
later call, another reply or resumed execution.

These observations assume the host owns the tool, middleware order and policy
writers. The checks are not an atomic final-dispatch guarantee for arbitrary
middleware or concurrently modified host state. A dispatch-time invalidation
removes advice only: execution already approved by the user follows the ordinary
permission flow. Neither `allow_once` nor `needs_human` rewrites a decision,
adds a rule, bypasses confirmation or changes tool input.

## Audit interpretation

Audit records include original/final deterministic behavior, known origin,
recommendation label, fixed status, attempt count and provider-reported tokens
when available. `final: "ask"` means the unchanged reviewer/engine decision,
including at dispatch observation; it is not the subsequent user confirmation
result. The summary records that separate outcome as `confirmed`. Missing tokens are `null`, not zero. An invalid report has unknown
attempt count (`null`); fixture usage is synthetic, not measured inference cost.
Follow-up lifecycle records have no new classifier attempt report.

Call, binding and expected-classifier references are HMACs with a fresh random
key per reviewer. They correlate only within that instance; they cannot be replayed
as approval tokens. Logs omit arguments, transcripts, rule suggestions, decision
messages/reasons, raw errors, endpoint URLs, credentials and model-response text.
`observed_policy_generation` is the host revision observed while writing the
record; `binding_ref` covers the earlier bound snapshot. `binding_checked` records
a successful observation, not a permission grant. On audit write failure the
reviewer retains no newly emitted advice and the runner returns a sanitized error
after draining the turn. Original permission handling remains unchanged.

The classifier types follow the existing [Python mapping](classifier.md#python-mapping).
This host-owned permission policy and typed Go decision provenance are deliberate
Go additions; no parity with a Python permission-review workflow is claimed.
