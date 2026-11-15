# Bailian decision-model classification

The experimental `classifier/bailian` package calls Alibaba Cloud Model Studio's
System One endpoint through `classifier.Classifier` and
`classifier.DispatchClassifier`. It returns typed probabilistic decisions; it
does not generate chat text or select a generation model. This is the focused
provider integration from [issue #69](https://github.com/agentscope-ai/agentscope-go/issues/69),
building on the classifier contracts in [#16](https://github.com/agentscope-ai/agentscope-go/issues/16).

## Configuration and use

`APIKey`, `Model` and `Endpoint` are required. The library reads no environment
variables and chooses no default model or region. The following example makes
one live request when run with valid configuration; its compilation does not
verify the service:

```go
package main

import (
    "context"
    "encoding/json"
    "fmt"
    "os"
    "time"

    "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
    "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier/bailian"
)

func main() {
    client, err := bailian.New(bailian.Config{
        APIKey: os.Getenv("DASHSCOPE_API_KEY"),
        Model: "decision-model-preview", // Explicit preview model selection.
        Endpoint: os.Getenv("BAILIAN_ENDPOINT"),
    })
    if err != nil { panic(err) }
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()
    out, err := client.Classify(ctx, classifier.Request{
        OperationID: "ticket-1",
        State: json.RawMessage(`"My payment failed."`),
        Questions: map[string]classifier.Question{
            "refund": &classifier.BinaryQuestion{Instructions: "Does this request a refund?"},
        },
    })
    if err != nil { panic(err) }
    fmt.Println(out.Answers["refund"].(*classifier.BinaryAnswer).Probability)
}
```

Supply the complete workspace/region POST URL, including
`/compatible-mode/v1/systemone`, rather than a base URL. The
[official API reference](https://help.aliyun.com/zh/model-studio/decision-model-api)
documents these endpoints (replace `{WorkspaceId}` with your business workspace ID):

- Beijing: `https://{WorkspaceId}.cn-beijing.maas.aliyuncs.com/compatible-mode/v1/systemone`
- Singapore: `https://{WorkspaceId}.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1/systemone`

Use HTTPS for remote services; HTTP is accepted for local fixtures. URLs cannot
contain credentials, queries or fragments. The host owns endpoint authorization
and must project both state and question content before calling the classifier.
Accepting a URL does not establish an allowed destination.

The [executable package examples](../pkg/agentscope/classifier/bailian/example_test.go)
include a synthetic HTTP transport. Run them without credentials or network calls:

```bash
go test -v -count=1 -run '^Example' ./pkg/agentscope/classifier/bailian
```

## Supported questions and limits

The adapter shares an internal System One codec with Jev while keeping its own
provider identity, configuration, limits and HTTP error type. No Jev API changes
are required, and Jev retains its existing 2–10 score-level limit.

| Input | Bailian adapter behavior |
|---|---|
| State | JSON string or object, following the Go classifier contract |
| Questions | 1–16 per request; nonblank instructions required |
| Binary | Encoded as `noul`; returns P(true) in [0,1] |
| Choice | 1–255 named alternatives; returns a choice, confidence and complete distribution |
| Score | 2–255 distinct ordered levels; returns the zero-based expected level, confidence and distribution |

The provider also documents array state and optional instructions. This adapter
deliberately retains the shared Go string/object and required-instruction
contract. Response IDs, types, distributions, finite ranges, score expectations
and score legends are checked; see [classifier validation](classifier.md).
It does not repair invalid answers or treat confidence as a candidate probability.

The service documents a **65,536-token state/context limit**. The adapter has no
authoritative Bailian tokenizer and does not enforce that limit locally. Hosts
must size their input for the deployed service and handle provider rejection;
the byte caps below are resource bounds, not token guarantees. No truncation is
performed. The explicitly configured preview model may change; protocol support
does not establish task quality or production qualification.

## Execution, errors and accounting

- Calls make at most one HTTP attempt. There are no internal retries, redirects
  or cookie-jar use. Caller context owns deadlines and cancellation.
- A custom `HTTPClient` is copied, must have `Timeout == 0`, and retains its
  shared transport. That transport must honor context, be safe concurrently and
  make no hidden retries; the adapter cannot enforce those properties inside it.
- `MaxRequestBytes` and `MaxResponseBytes` default to 1 MiB and 4 MiB. The request
  cap is checked after JSON encoding; the response cap bounds bytes read after
  decompression. Oversized responses fail without successful partial results.
- Pre-dispatch validation/cancellation/guard failures have zero attempts. Once
  `http.Client.Do` is entered, the report retains one `PhysicalAttempt`, even on
  connection failure. This does not prove server receipt or remote cancellation.
- Missing usage remains unknown, including an omitted `output_tokens` field;
  reported zero remains zero. Valid usage survives malformed answers and HTTP
  errors when the complete, bounded JSON body can be decoded. Requested and
  returned model identities remain separate.
- Local validation uses `classifier.invalid_request`, invalid replies use
  `classifier.invalid_response`, and HTTP/transport failures use `classifier.failed`.
  A fully read non-2xx response wraps `*bailian.HTTPError` with its status, even
  if its JSON is malformed. 429/5xx carry retry hints, never automatic retries.
  Read/size failures take precedence over HTTP status. Context errors use
  `classifier.canceled`, preserve `errors.Is`, and override late completion.

Always check the error before using answers. Methods return a non-nil report on
failure. Errors omit credentials, raw transport strings and provider bodies;
the adapter emits no logs. Host transports/logging must follow the same rules.

`Target` binds the actual configured endpoint and requested model.
`ClassifyWithDispatch` narrows byte caps through positive `DispatchLimits` values
and runs `begin` immediately before the single dispatch, after local validation
and encoding. Zero retains configured caps; negative limits are invalid. Guard
rejection preserves its error identity and records no attempt. Ordinary `Classify`
does not run a managed authorization guard. See [managed routing](managed-routing.md).

## Verification

Offline fixtures cover wire conversion, provider limits, failures, cancellation,
usage and dispatch accounting. They do not establish live-provider compatibility,
classification quality, latency or savings:

```bash
go test -race -count=1 ./pkg/agentscope/classifier/bailian
```

The opt-in live contract test sends one synthetic, potentially billable request.
Set `DASHSCOPE_API_KEY` securely first, then select a full official HTTPS workspace
endpoint and a model explicitly:

```bash
BAILIAN_LIVE_TEST=1 \
BAILIAN_LIVE_ENDPOINT='https://{WorkspaceId}.cn-beijing.maas.aliyuncs.com/compatible-mode/v1/systemone' \
BAILIAN_LIVE_MODEL=decision-model-preview \
go test -v -count=1 -run '^TestLiveContract$' ./pkg/agentscope/classifier/bailian
```

Without opt-in or an API key this test reports **SKIP**, not live verification.
With both set, missing/invalid endpoint or model configuration fails locally.
The live check accepts the documented Beijing/Singapore HTTPS workspace domains,
validates all three answer types and logs requested/returned model identities.
Report its date, selected model and outcome separately from fixture checks;
preview aliases do not identify an immutable model version.

## Python reference

The [Python Jev adapter](https://github.com/agentscope-ai/agentscope/blob/083cbd1975c7b5ed055c69b0d19c150727e7f606/src/agentscope/classifier/_jev/_model.py)
uses the TypeSafe SDK; the related [Python discussion](https://github.com/agentscope-ai/agentscope/discussions/3259)
proposes explicit Bailian support. Go uses the standard library and shared internal
wire conversion, with explicit full endpoints, bounded bodies and no SDK retries
or independent request timeout. No dependency is added.
