# OpenAI Decisions classification

The experimental `classifier/openai` package implements `classifier.Classifier`
and `classifier.DispatchClassifier` using OpenAI's Decisions HTTP endpoint.
It evaluates text independently of generation models; it does not itself select
or call a generation model. See the [classifier contract](classifier.md) and
[feature request #25](https://github.com/agentscope-ai/agentscope-go/issues/25).
Hosts must authorize the destination and project both state and question content
before calling the adapter. Ordinary `Classify` performs no host authorization.

## Use

Model-name reference checked on 2026-10-17: the
[official Decisions guide](https://developers.openai.com/api/docs/guides/decisions)
and [API reference](https://developers.openai.com/api/reference/resources/decisions/methods/create)
list `gpt-6-luna` as supported by this endpoint. The examples below use that
name explicitly. This is documentation verification, not a successful live
request or confirmation of access for a particular account. Recheck current
service documentation and account availability before running the live test.

```go
package main

import (
    "context"
    "encoding/json"
    "fmt"
    "os"
    "time"

    "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
    "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier/openai"
    "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/credential"
)

func main() {
    client, err := openai.New(openai.Config{
        Credential: &credential.OpenAICredential{Key: os.Getenv("OPENAI_API_KEY")},
        Model: "gpt-6-luna", // Explicitly choose a model supported by Decisions.
    })
    if err != nil {
        panic(err)
    }
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()
    out, err := client.Classify(ctx, classifier.Request{
        OperationID: "ticket-1",
        State: json.RawMessage(`"Export fails in one browser."`),
        Questions: map[string]classifier.Question{
            "bug": &classifier.BinaryQuestion{Instructions: "Does this report a software bug?"},
        },
    })
    if err != nil {
        // out remains non-nil and retains any attempt and known usage.
        fmt.Fprintln(os.Stderr, err)
        return
    }
    fmt.Println(out.Answers["bug"].(*classifier.BinaryAnswer).Probability)
}
```

This example sends one potentially billable request. The constructor never reads
environment variables or chooses a model implicitly. A credential and model are
required. An empty credential URL uses `https://api.openai.com/v1`; a custom URL
is an API base, including its version/prefix, to which `/decisions` is appended.
Trailing slashes are removed. Userinfo, queries and fragments are rejected.
Use HTTPS for remote services; HTTP is available for local fixtures. Selecting
an endpoint is a host trust decision, not a library-enforced allowlist.

`New` snapshots credential values and copies a supplied `HTTPClient`, sharing
its transport. Client timeout must be zero: the caller's context owns the overall
deadline. Redirects and cookie jars are disabled on the copy. A custom transport
must honor context, be concurrency-safe, and perform no retries or hidden
requests. The adapter cannot enforce these properties inside custom transports.

## Mapping and validation

The mapping follows the [official API reference](https://developers.openai.com/api/reference/resources/decisions/methods/create)
and [Decisions guide](https://developers.openai.com/api/docs/guides/decisions).
Both Choice and Score return probability distributions; the guide illustrates
all supplied candidates and all supplied levels. The response schema does not
itself enforce completeness by array cardinality, so the adapter checks it.

| Go contract | Decisions request | Decisions response |
|---|---|---|
| `BinaryQuestion` / `BinaryAnswer` | `predicate`; optional True/False descriptions appended to instructions | `probability` copied unchanged |
| `ChoiceQuestion` / `ChoiceAnswer` | String candidate IDs as `choices[].value`, with optional descriptions | `choice`, `confidence`, and `probabilities[]` mapped by candidate value |
| `ScoreQuestion` / `ScoreAnswer` | Ordered `Levels` as `levels[].label` | `score`, `confidence`, and per-level probabilities mapped by zero-based `value` into the original rubric order |

Question IDs become `name`. Questions and Choice candidates are sorted by ID for
stable wire ordering. Returned answer and distribution arrays may be reordered;
they are matched by name/value, not array position. Score labels must match the
requested rubric. Choice requires 2–255 candidates, stricter than the shared
contract. The adapter applies the shared nonempty Score rubric validation; it
does not impose Jev's 2–10 level limit.

JSON string state is decoded to a text string. Object state is compacted and sent
as JSON **text**, not as structured API message content. Images, chat histories,
boolean candidate values and separate score label/description fields are not exposed.
Invalid UTF-8 descriptions are rejected before dispatch.

Missing, duplicate, unknown or mismatched answers/candidates/levels fail. Missing
or null required numeric fields cannot become zero. `ValidateResponse` checks
probability ranges and totals, that Choice selects a maximum, and that Score
matches its probability-weighted expected index. Provider values are never
filled in, normalized or recomputed. Unknown additional JSON fields are ignored.
A refusal makes the whole call fail with non-retryable `classifier.failed`;
there is no public refusal answer type and no successful partial result.
All failures clear `Answers` and retain the attempt and known usage.

## Limits, errors and accounting

- `MaxRequestBytes` and `MaxResponseBytes` default to 1 MiB and 4 MiB. The request
  cap applies to complete serialized JSON before dispatch; serialization itself
  allocates the payload. The response cap bounds bytes read after HTTP
  decompression. Oversized bodies fail without truncating into success.
- Validation, request limits, pre-dispatch cancellation and guard failures record
  zero attempts. Entry to `http.Client.Do` records one physical attempt, including
  connection failures that do not establish server receipt. There are no internal
  retries or redirects. `RequestedModel` is configured; actual model identity
  comes only from the response.
- Usage is held only on the attempt. Nil counts mean unknown; a pointer to zero
  means a reported zero. Valid counts survive refusal, malformed answers, HTTP
  errors and late cancellation. Invalid counts remain unknown while valid
  siblings survive. Unparseable, oversized or incomplete bodies supply no usage.
- Local failures use `classifier.invalid_request`; response validation uses
  `classifier.invalid_response`; HTTP/transport/refusal failures use
  `classifier.failed`. Non-2xx responses wrap `*openai.HTTPError` with only the
  status code. 429 and 5xx carry retry hints, including a valid `Retry-After`,
  for the caller's policy. Body read errors and response limits take precedence
  over HTTP status and have no retry hint.
- Cancellation uses non-retryable `classifier.canceled`, matchable with
  `errors.Is`. Cancellation observed before return overrides late success or
  other failures. Transport errors preserve only standard context sentinels;
  raw provider bodies, transport error strings and credentials are omitted from
  error chains. The adapter emits no logs. Successful IDs and model names are
  still untrusted provider data, not authorization decisions.

For the optional [managed router](managed-routing.md), `Target` binds the actual
endpoint/model. `ClassifyWithDispatch` narrows byte caps and invokes the host's
guard once after local preparation, immediately before dispatch. A guard error
preserves its identity and prevents the attempt; ordinary calls use no guard.

## Verification

```bash
go test -race -count=1 ./pkg/agentscope/classifier/openai
```

Offline tests use local HTTP fixtures and custom transports. They establish
conversion, validation, cancellation and accounting behavior, not real service
compatibility or quality. The live contract test sends synthetic text to the
default OpenAI endpoint and makes one potentially billable request. Set
`OPENAI_API_KEY` securely in the environment, then explicitly opt in:

```bash
OPENAI_DECISIONS_LIVE_TEST=1 OPENAI_DECISIONS_LIVE_MODEL=gpt-6-luna \
  go test -v -count=1 -run '^TestLiveContract$' ./pkg/agentscope/classifier/openai
```

Without opt-in or a key it reports **SKIP**, not live verification. The model
must be explicitly supplied and supported by the service; requested and returned
identities are logged because aliases can resolve differently. The test checks
complete distributions through the shared validator, not semantic answer quality,
calibration, cost savings or downstream model qualification.

## Python reference

The [Python adapter merged in PR #3174](https://github.com/agentscope-ai/agentscope/blob/98dff833628a93bf44b2f33eb222d01ff6db6266/src/agentscope/classifier/_openai/_model.py)
uses the OpenAI SDK and supports Base64 images and per-question `RefusalAnswer`.
Go deliberately keeps text-only input and treats refusals as errors without
extending the shared answer API. It uses standard-library HTTP with no new
dependency, no SDK retries or independent timeout, and retains failed-attempt
accounting. The existing `internal/httpx.DoJSONRequest` retries and reads
unbounded bodies, so it is not used on this single-attempt path.
