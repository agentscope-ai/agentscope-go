# Upstream sync for v2.0.11

## Baseline and scope

This design compares Go revision `7506686` with Python AgentScope revision
[`5ff52f87`](https://github.com/agentscope-ai/agentscope/commit/5ff52f877de12d66a30d55af279dd4f42b1590f3),
including upstream changes from September 8–18, 2026. The release also includes
Go changes already on `main` since `v2.0.10`, particularly the module move to
`github.com/agentscope-ai/agentscope-go/v2`.

The selected work improves human interaction and message handling without adding
provider SDKs or changing the required `Tool` and `ChatModel` interfaces.

| Upstream change | Go decision |
|---|---|
| [AskUser #2573](https://github.com/agentscope-ai/agentscope/pull/2573), [question validation #2586](https://github.com/agentscope-ai/agentscope/pull/2586), [successful-result validation #2623](https://github.com/agentscope-ai/agentscope/pull/2623) | Add an opt-in external question tool, validated input and answers, and preserve external result metadata through events and recorded tool results. |
| [Console cancellation #2658](https://github.com/agentscope-ai/agentscope/pull/2658) | Return caller cancellation from the prompt and reply paths; retain reply-only interruption for SIGINT. |
| [Empty Gemini text #2641](https://github.com/agentscope-ai/agentscope/pull/2641), [empty hints #2656](https://github.com/agentscope-ai/agentscope/pull/2656) | Remove empty text/hint payloads in Anthropic and Gemini formatters and empty text in Gemini request construction. Preserve supported media within hints and message attribution after filtering. |
| [Model context #2597](https://github.com/agentscope-ai/agentscope/pull/2597), [Ollama cards #1753](https://github.com/agentscope-ai/agentscope/pull/1753) | Follow up with an explicit metadata/configuration design. Most Go adapters do not expose `ModelNamer`; a model card's maximum is not an Ollama server's configured `num_ctx`. Existing `ContextConfig.ContextSize` remains the explicit override. |
| [Ark #2532](https://github.com/agentscope-ai/agentscope/pull/2532) | Separate provider contribution with credentials, request contracts, streaming fixtures and model cards. Do not equate a custom OpenAI base URL with verified Ark support. |
| Realtime [#2547](https://github.com/agentscope-ai/agentscope/pull/2547), [#2552](https://github.com/agentscope-ai/agentscope/pull/2552), [#2553](https://github.com/agentscope-ai/agentscope/pull/2553), [#2556](https://github.com/agentscope-ai/agentscope/pull/2556); TUI [#2507](https://github.com/agentscope-ai/agentscope/pull/2507), [#2594](https://github.com/agentscope-ai/agentscope/pull/2594) | Separate work: audio transport, turn lifecycle, reconnect semantics and frontend support need a shared protocol before adding provider implementations. |
| [SOP #2393](https://github.com/agentscope-ai/agentscope/pull/2393) | Separate workflow design for step state, persistence, verification and approval; AskUser provides one useful prerequisite. |
| [Glob output #2572](https://github.com/agentscope-ai/agentscope/pull/2572) | Go already caps output at 1000 matches. Pagination and recursive truncation reporting deserve a focused follow-up; no claim of identical Python behavior. |
| [In-memory lock TTL #2654](https://github.com/agentscope-ai/agentscope/pull/2654) | No direct port: Go's `MessageBus` has no `try_lock` operation. Do not extend every backend merely to copy this fix. |
| [Anthropic stream blocks #2495](https://github.com/agentscope-ai/agentscope/pull/2495), [tool media ordering #2613](https://github.com/agentscope-ai/agentscope/pull/2613), [history marker #2643](https://github.com/agentscope-ai/agentscope/pull/2643) | Follow up with provider-specific ordered-block fixtures. This release does not claim full streaming or formatter parity. |

Python-only frontend, document-parser, CI and packaging updates are outside this
batch. Go PR #9 remains a separate review; this release does not merge or claim
its `max_tokens` fix.

## AskUser contract

`tool.AskUserTool()` returns a `Tool` named `AskUser`. It is read-only,
concurrency-safe and external. It is opt-in: neither the enhanced toolkit nor
application factories register it automatically. The host must configure
`WithPermissionContext` (which initializes the external/confirmation channels)
and consume
`UnifiedAgent.ReplyStream` events and answer `RequireExternalExecutionEvent`
through `SubmitExternalResult`. Ordinary `Reply`, the loop runner and the stock
console do not provide a question UI.

Export typed `AskUserParams`, `AskUserQuestion`, `AskUserOption`, `AskUserAnswer`
and `AskUserMetadata` for applications constructing requests and answers. Match
Python's JSON field names: `questions`, `question`, `header`, `context`,
`options`, `label`, `description`, `preview`, `multi_select`, `answers`,
`selected` and `other`.

Input validation enforces 1–4 questions, 2–4 options per question, a header of at
most 12 Unicode code points, nonblank required strings, unique question texts
and unique option labels per question. Optional previews are for single-select
questions. Validation happens before an external request is emitted, including
on checkpoint resume. Restored submitted calls must resolve to an active external
tool, validate input again and honor current permission rules before being
re-emitted. A missing, inactive or no-longer-external tool produces an error; it
must not fall through to local execution. Invalid calls produce a normal error tool result so the
model can correct its request. Direct execution fails with an explanation that
an external host is required; it never invents a user's answer.

Two optional interfaces keep existing `Tool` implementations source-compatible:

- `InputValidator.ValidateInput(map[string]any) error` adds semantic validation
  after the existing schema/type checks in toolkit execution and before an
  external handoff. Existing toolkit coercion remains in place.
- `ExternalResultValidator.ValidateExternalResult(map[string]any,
  *message.ToolResultBlock) error` validates successful external results against
  the original request. Error and denied results do not need success metadata.

Successful AskUser metadata must contain one answer per requested question,
using the exact question text. Selected labels must belong to that question and
must not repeat. Single-select questions accept at most one selected label.
An answer needs a selection or nonblank free text; free text does not have to
match an offered option. This deliberately checks more than Python's metadata
shape validation so applications can safely correlate answers with questions.
It does not establish that a human actually supplied an answer or authorize any
subsequent tool action; the host remains the trust boundary.

A malformed successful result becomes an error tool result without retaining
unvalidated success metadata. Unlike Python's synchronous rejection/retry path,
Go's existing `SubmitExternalResult` returns no error, so this release reports
validation failure through the agent event stream and lets the model re-ask.
No new acknowledgement or re-submission protocol is introduced.

Preserve validated external metadata on `ToolResultEndEvent`, the recorded
`ToolResultBlock` and messages reconstructed through `Msg.AppendEvent`. Add a
`ToolResultEndEvent.GetMetadata` accessor for the existing metadata field and
consume it in the reconstruction path. Apply the same contract to resumed
external calls. Preserve complete multi-block external output in recorded state
and emit supported text/data blocks in order, subject to existing event data
limits. Normal single-text results keep their string representation; resumed
results keep the submitted representation. External submission transfers ownership of the supplied result
objects to the agent; callers must not mutate them after submission. Existing
session snapshot/deep-copy limitations remain as documented in `STABILITY.md`.
Tool execution permissions still apply: asking the user must not bypass an
explicit denial rule or grant permission to a later operation. `ModeDontAsk`
denies AskUser because no interactive user is available; other modes allow the
question tool itself with a tool-level Allow decision, subject to explicit engine
rules. An explicit AskRule can still request permission confirmation; hosts must
handle that event too. Test that ModeDefault without such a rule does not ask
for confirmation before asking the question. This is an intentional
difference from Python's unconditional tool-level allow decision.

## Empty-content handling

Skip genuinely empty text (`""`), without trimming whitespace from nonempty
text. Empty hints must not introduce an empty provider block. For structured
hints, preserve nonempty text and supported data blocks in order; do not discard
an image-only hint. Skip contentless messages instead of synthesizing empty
provider text.

Format each original message with its own sender attribution before discarding
empty/system messages in `FormatMultiAgent`. Indexing filtered output against
unfiltered input mislabels subsequent messages and becomes more likely when
empty messages are removed.

Gemini's model adapter builds requests independently from its public formatter.
Test and fix empty ordinary/system text in that actual request path too, in both
`Chat` and `ChatStream`. The current adapter does not gain general multimodal
hint support in this batch; document that distinction. Anthropic already routes
its requests through its formatter.

## Cancellation

A canceled parent context returns `ctx.Err()` from `console.Launch`, whether it
is waiting for input, waiting for a confirmation or consuming a reply. A
reply-only SIGINT still cancels the active reply and returns to the prompt;
ordinary EOF and quit remain successful exits.

Make event consumption context-aware so a producer that does not promptly close
its channel cannot keep the console blocked. Do not close channels owned by the
agent or input streams supplied by callers. The existing blocking-reader
limitation remains: owners of pipes must close them to release a blocked reader.

## Validation and delivery

Write regression/acceptance tests first and record the relevant pre-fix
failures. Cover malformed and duplicate questions, Unicode headers, single- and
multi-select answers, free text, mismatched/duplicate answers, error results,
permission denial (including after resume), cancellation, external metadata
events/state/stream reconstruction and checkpoint resume. Use deterministic event fixtures, not a claimed live human/provider
integration. Add an executable offline AskUser example with a scripted model
and host-provided answers, clearly labeled as such.

Formatter tests cover empty and mixed hints, image-only hints, tool blocks and
sender attribution after filtering. Captured HTTP requests cover actual
Anthropic/Gemini behavior. Console tests use explicit synchronization and check
both `context.Canceled` and `context.DeadlineExceeded`; retain SIGINT tests.

Measure touched-package coverage with the same Go toolchain and flags before
and after. Run build (including examples), vet, full race tests, lint and the
library coverage gate. Check Go 1.25 compatibility and CI's OS/architecture
matrix. No safety-parser changes are planned; add the relevant fuzz smoke if
that changes. No new library dependency is planned.

Independent evaluators review this design before implementation, code before
commit, documentation against final source, and the final release notes.
Correct every blocking finding and request re-review. Review transcripts and
validation logs stay outside the repository.

Before delivery, fetch `main`, reconcile any intervening changes and repeat
affected checks/review. Push without force, wait for CI on the pushed revision,
then tag that revision as `v2.0.11`. Publish the approved notes with GitHub CLI
and release name `v2.0.11`.

The changelog needs a historical repair: the previous `v2.0.10` tag already
contains the long September 7 sync/product sections still under `[Unreleased]`.
Move those existing entries under `v2.0.10`, using the tag as evidence; only
changes after that tag belong in `v2.0.11`. Update installation, stability and
security-support documentation for the first community-path release. Explain
the import-prefix migration explicitly and keep the existing old-path deleted-
version caveat; do not present historical features as new in this release.
