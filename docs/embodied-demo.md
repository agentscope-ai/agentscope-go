# Single-device embodied simulation

`examples/embodied_demo` demonstrates the #67 milestone-2B loop in a
deterministic simulator: observation → device Agent planning → edge/local or
edge+cloud replanning → device validation → structured action → execution
feedback. It uses the real buffered `UnifiedAgent.Reply` and
`tierrouter.ChatRouter` path for semantic planning and `embodied.Gateway` for
action admission.

It is not a live-provider, robot, real-time or hardware-safety demonstration.

## Run

```bash
go run ./examples/embodied_demo
go test -race -count=1 ./examples/embodied_demo
```

The offline comparison runs the same unavailable-packing-station scenario in
edge-only and edge+cloud modes. Reports include every planned task,
completion, interventions, rejected stale proposals, duplicate effects,
model/cloud requests, action effects and wall-clock latency. Fixture behavior
establishes no performance benefit.

## Boundary

- The device Agent owns the root task and action authority. The host schedules
  pick/deliver steps and stops on unresolved outcomes.
- The edge coordinator validates observation/site versions and proposed
  alternatives. The optional read-only cloud planner proposes a destination
  but cannot execute or mutate systems.
- The observation has an opaque media reference, but only its approved text
  summary enters edge/cloud requests. Tests assert that no media reference is
  disclosed.
- Structured actions use registered skills and destinations through
  `embodied.Gateway`. Controller records provide independent effect evidence.
- No-cloud mode has a declared local/edge path. Without an approved local
  alternative and with cloud unavailable, the task fails explicitly instead
  of silently escalating.
- Routine execution feedback does not increment the site-state version and
  therefore does not invalidate the second scheduled action.

## Fault evidence

Tests cover WAN-loss degradation, stale observations, site-state revision
while cloud planning is in flight, forbidden destinations and lost ACK
recovery. Restart reopens the same metadata, Store and controller logs,
restores trusted world state and calls `Recover` before further admissions.
The recovered pick action is not repeated; only the remaining deliver action
continues.

Real providers, media loading, automatic plan progression, hardware, fleet
coordination, distributed quotas and generalized operation governance remain
outside this example.
