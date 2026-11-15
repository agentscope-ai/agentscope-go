# Three-tier ChatModel routing

The experimental `tierrouter.ChatRouter` connects the routing policy from
issue #67 to the ordinary `model.ChatModel` interface. A `UnifiedAgent` can
use device, edge and cloud deployments without moving the Agent, creating a
remote-Agent subtask or changing the Stable `ChatModel` contract.

This source-checkout slice is limited to deterministic, single-attempt fake
ChatModels, plain-text buffered calls and in-process replay. It does not
establish live-provider compatibility, routing quality, hardware safety or
performance benefits.

## Run the offline comparison

```bash
go run ./examples/tier_routing
go test -race -count=1 ./pkg/agentscope/tierrouter ./examples/tier_routing
```

The example runs the same four synthetic tasks through fixed device, fixed
edge, fixed cloud and routed groups. A scripted clean cloud failure produces
one cloud-to-edge fallback in the routed group. Reports include every planned
task, terminal status, end-to-end latency, attempts, the actual tier path,
per-tier request counts and attempts without usage. The `status` control is
pinned to one device attempt with no edge/cloud request. Delays and responses
are fixtures, not benchmarks of real deployments.

## Construct a routed model

```go
router, err := tierrouter.NewChatRouter(tierrouter.ChatRouterConfig{
    Policy: tierrouter.StaticPolicy{
        Rules: map[string][]tierrouter.Tier{
            "local": {tierrouter.TierDevice},
            "scene": {tierrouter.TierEdge, tierrouter.TierDevice},
            "plan":  {tierrouter.TierCloud, tierrouter.TierEdge, tierrouter.TierDevice},
        },
        Default: []tierrouter.Tier{tierrouter.TierDevice},
    },
    Epoch: func() int64 { return currentEpoch },
    Deployments: []tierrouter.ChatDeployment{
        {Tier: tierrouter.TierDevice, Model: deviceFake},
        {Tier: tierrouter.TierEdge, Model: edgeFake},
        {Tier: tierrouter.TierCloud, Model: cloudFake},
    },
    ContextFallback: 4096,
})
if err != nil {
    // Handle invalid registration.
}

replyAgent := agent.NewUnifiedAgent("device-agent", "Answer briefly.", router,
    agent.WithModelStreaming(false),
    agent.WithModelConfig(agent.ModelConfig{MaxRetries: 1}))
ctx := tierrouter.WithChatOperation(context.Background(), &tierrouter.Operation{
    ID: "task-42/model-call-1", TaskEpoch: currentEpoch, Kind: "scene",
    TTL: 2 * time.Second,
})
reply, err := replyAgent.Reply(ctx, "Describe the approved scene.")
```

Each deployment must be non-nil and claim a distinct tier. The constructor
rejects nested `ChatRouter`, `FallbackChatModel` and `ConnectivityAwareModel`
deployments. Arbitrary custom wrappers can still hide retries or fallback;
they are outside this slice's single-owner contract. `CallOptions.MaxRetries`
must remain zero and tools, voice and non-`none` tool choices are rejected.

For the exercised Agent path, use `WithModelStreaming(false)`,
`ModelConfig{MaxRetries: 1}` and no Agent `FallbackModel`. Otherwise the Agent
can add attempts outside the router. `ChatStream` returns
`model.ErrStreamNotSupported`; the default Agent streaming path can fall back
to buffered `Chat`, so direct stream rejection alone does not enforce the
claimed configuration.

`managedtask` remains an independent fixed-task boundary and is not used by
this integration. `taskenvelope` models optional remote-Agent delegation;
selecting a model tier does not create an envelope or transfer device action
authority.

## Operation identity and deadlines

The trusted host supplies an `Operation` through `WithChatOperation`. Model
text cannot set its ID, kind, content, local-only flag, epoch or TTL. Missing
identity fails closed. A local-only operation is filtered to the device tier
after policy evaluation, even when the policy prefers another tier.

The versioned chat request hash covers kind, content, local-only, epoch, TTL,
the complete normalized messages and frozen call options. Message IDs and
timestamps therefore matter: an idempotent replay must submit the same
logical request, not recreate a message with new generated fields. Changed
messages, options or hashed operation fields with the same ID return
`ErrIDConflict` without dispatch.

All original `CallOption` functions are applied exactly once per `Chat`
invocation. Their resulting `CallOptions` snapshot is cloned for hashing and
every tier attempt, so a stateful option is not re-executed during fallback.
Nested retries are rejected. A `RetryDelay` supplied without `MaxRetries` is
inert: it is preserved in the frozen options but does not create a retry by
itself. Message and response metadata must contain supported JSON-compatible
values and valid UTF-8. Invalid UTF-8, cyclic containers and unsupported
values fail closed rather than being dropped, coerced or recursively expanded.
Typed-nil containers remain nil.

A zero `SubmittedAt` is stamped at first admission. The original operation
and deadline are stored with the record. Later submissions cannot extend or
shorten that deadline by supplying another `SubmittedAt`.

## Dispatch, response admission and replay

Epoch and TTL are checked at admission and before every dispatch, including
an unattached-tier record. A clean fake-provider failure can advance to the
next candidate. Cancellation, deadline expiry and unknown outcomes stop the
operation without fallback. Retry after uncertainty requires a new operation
ID. `policy.Prefer` and the epoch callback run under the router mutex; they
must be fast, concurrency-safe and must not call back into the same router.

Replay also checks the original operation's current guards before returning a
recorded outcome. Consequently, replaying a `no-route` rejection after an
epoch advance reports `ErrStaleEpoch`, while the snapshot retains `no-route`
as the original stop reason. A first-call invalid response returns
`ErrChatInvalidResponse`; replaying that uncertain record returns
`ErrUncertain`. Both paths fail closed and neither redispatches.

A response returned without a Go error is usable only when it is non-nil, has
no in-band `ChatResponse.Error`, is final, contains at least one text block,
contains no unsupported blocks, and is not truncated, filtered or a tool-call
response. Invalid or partial responses are terminal uncertainties, not clean
failures, and never trigger fallback.

Before a successful response is returned to the Agent, the router rechecks
the original epoch and TTL. A late response can remain visible as a completed
attempt in the evidence snapshot, but the operation becomes `superseded` and
the response is not accepted. It cannot enter the current Agent reply or
assistant history.

Completed state and the process-local response cache are published under the
same lock. An identical in-process replay returns a deep copy without another
deployment call, but still requires the original epoch and deadline to be
current. A defensive completed-without-cache condition returns
`ErrResponseUnavailable`; it never causes redispatch. A new `ChatRouter` has
no operations or responses. Durable response recovery and cross-process
replay are not provided.

`ChatSnapshot` returns a deep-copied evidence record containing the request
hash, operation state, stop reason, attempts and available usage. It does not
contain message or response bodies. Agent history retains its usual text and
input/output usage fields; earlier-tier and failed-attempt usage remains in
router snapshots rather than being added to the accepted Agent response.

## Context windows and token estimates

`ContextSize` returns the minimum resolved window across every registered
deployment. An unknown or non-positive window uses the required positive
`ContextFallback`. `CountTokens` returns the maximum estimate across the
deployments. The router does not expose one deployment's `ModelName` as the
identity of the whole chain.

## Evidence and exclusions

Tests exercise the actual buffered `UnifiedAgent.Reply` path, including
fallback, no additional Agent retry, stale-response rejection and recovery
under a new operation. Fake request records and router snapshots are the
grading evidence; provider self-reports alone are not.

Real providers need typed outcome evidence before ambiguous transport or
decoding failures may fall back. Streaming, multimodal content, tools,
compression/repair/verification helpers, durable response recovery,
arrival-rate limiting, distributed allowances, cloud Agents, physical
execution and hardware benchmarks remain separate work.
