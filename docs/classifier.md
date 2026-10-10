# Probabilistic classification

The experimental `pkg/agentscope/classifier` package evaluates typed questions
against shared text or a JSON object. It is independent of `model.ChatModel`.
This guide covers the classifier abstraction merged in [PR #22](https://github.com/agentscope-ai/agentscope-go/pull/22).
The [Jev adapter](jev.md) implements these contracts in a separate subpackage.
Managed execution and routing are later stages of
[issue #16](https://github.com/agentscope-ai/agentscope-go/issues/16); they are not
provided by this package. This guide will grow with those stages.

Run the offline example from a repository checkout:

```bash
go run ./examples/classifier
```

It demonstrates all three answer types, a dispatched failure, unknown usage and
cancellation after simulated dispatch. It uses no network or credentials. The
[same example](../examples/classifier/main.go) also shows a custom local
classifier implementation and zero-request results.

## Requests and answers

```go
type Classifier interface {
    Classify(context.Context, Request) (*Response, error)
}
```

`Request` has a caller-supplied `OperationID`, `State` (`json.RawMessage` containing
one JSON string or object), and `Questions` (`map[string]Question`). Question IDs
only correlate answers. Put the actual question in `Instructions`; IDs are never
implicit prompts. Hosts must authorize and project state and question data before
passing it to a provider adapter. Validation alone is not authorization.

| Question pointer | Answer pointer | Meaning |
|---|---|---|
| `BinaryQuestion` (Noul) | `BinaryAnswer` | Probability that a proposition is true, in [0,1] |
| `ChoiceQuestion` | `ChoiceAnswer` | A highest-probability candidate and a distribution over all requested candidates |
| `ScoreQuestion` | `ScoreAnswer` | Expected zero-based rubric level and a distribution in requested level order |

All questions require nonblank instructions. Choice questions require one or
more named alternatives; their descriptions may be empty. Score questions need
one or more nonblank, distinct level descriptions. Answers must exactly match the
requested IDs and question types. Confidence on Choice and Score is a separate
provider-reported value in [0,1]; it is not substituted for probability or score.
Multiple answers are separate marginal decisions, not a joint distribution.

`ValidateRequest` checks inputs. `ValidateResponse` checks successful answers,
actual model identity and accounting. Values must be finite and in range;
distributions must cover exactly the requested alternatives and sum to one within
`ProbabilityTolerance` (1e-6). Ties for the highest-probability choice are allowed.
Score must agree with the weighted expectation within 1e-6 times the greater of
one and the highest rubric level. Validation does not normalize, repair, choose
default answers or select a fallback model. Validation errors are structured
`errors.AgentError` values matching `ErrInvalidRequest` or `ErrInvalidResponse`
with Go's `errors.Is`.

## Adapter contract and accounting

An implementation should:

1. Check `ctx` and `ValidateRequest` before constructing or dispatching anything.
   Recheck cancellation at dispatch and pass the context to the transport.
2. Make at most one outbound request. Do not enable retries or automatic redirect
   following in a transport that could issue additional requests. Later executor
   policy can repeat primitive calls using the same `OperationID` and a separate
   finite physical-attempt budget; this package does not enforce task limits.
3. Once dispatch begins, append one `PhysicalAttempt`, even if the transport,
   provider, cancellation or result validation later fails. Before dispatch there
   are zero attempts. A `Response` must be returned on errors too. Check the error
   before using answers. `ValidateReport` checks accounting without requiring
   successful answers or known actual model identity.
4. Preserve provider-reported concrete model identity on success. On failure it
   may be unknown: use an empty `Model`, and keep any requested alias separately
   in `Attempt.RequestedModel`. Do not fabricate an actual model identity.
5. Preserve known usage on errors. Usage lives only in `Attempt.Usage`; do not
   also charge a logical operation as a second request. Nil usage or nil token
   fields mean unknown. A pointer to zero means a known zero. Duration measures
   elapsed attempt time, not a provider token estimate. `Attempt.Err` records the
   classification outcome, including response-validation failure. Wrapped context
   errors must remain identifiable with `errors.Is(err, context.Canceled)` or
   `errors.Is(err, context.DeadlineExceeded)`.
6. Use `CategoryModel` with non-retryable code `classifier.canceled` for caller
   cancellation and errors whose chains match either context sentinel. Messages
   distinguish cancellation before and after dispatch. Other attempt failures use
   `classifier.failed`; validation errors retain their validation codes. Derive
   the overall deadline from `ctx`, rather than imposing a shorter independent
   attempt deadline classified as non-retryable cancellation.

A successful purely local classifier may report zero attempts. One `Classify`
call is one primitive evaluation for a logical operation; a later executor can
group retry calls by `OperationID`. Simulations and physical requests are distinct
`Attempt.Kind` values, not interchangeable counts. Validation cannot verify an
adapter's actual network activity; the provider adapter must test its dispatch
boundary. Do not log task text or credentials by default. These exported Go types
are not a persistence schema or a provider JSON wire protocol.

`NewFake(*FakeConfig)` snapshots preset answers and usage. Calls return independent
copies and are safe concurrently; error objects must be immutable. The fake
supports a preset `Error` and `WaitForCancel`. A configured error takes precedence
over answer validation. `WaitForCancel` waits after dispatch and returns the
context error instead of the configured error; without it, cancellation observed
at the post-dispatch context check also overrides the configured error.
`Started()` closes on the first simulated dispatch so tests can cancel
deterministically. Construct the receiver with `NewFake`: a nil receiver panics
on `Started()`, and a zero-value fake returns a nil channel. Invalid preset answers
are checked at call time, preserving simulated attempt records and usage on
failure. All fake attempts are marked `SimulatedAttempt`, regardless of the
fixture's original kind. Fixtures do not establish live-provider behavior.

## Python mapping

The design follows Python AgentScope's
[classifier base](https://github.com/agentscope-ai/agentscope/blob/083cbd1975c7b5ed055c69b0d19c150727e7f606/src/agentscope/classifier/_base.py),
[questions](https://github.com/agentscope-ai/agentscope/blob/083cbd1975c7b5ed055c69b0d19c150727e7f606/src/agentscope/classifier/_question.py)
and [answers/usage](https://github.com/agentscope-ai/agentscope/tree/083cbd1975c7b5ed055c69b0d19c150727e7f606/src/agentscope/classifier).
Like Python, it accepts string/object state, Choice alternatives and zero-based
ordered Score levels. Go uses sealed pointer types and explicit validation,
requires instructions, and keeps score legends in the original request instead
of duplicating them in every answer. It adds explicit operation/attempt reporting
and preserves unknown token counts with pointers. Jev-specific credentials,
parameters and protocol limits are documented in the [Jev guide](jev.md).
