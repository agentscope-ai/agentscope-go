# Jev classification

The experimental `classifier/jev` package implements the `classifier.Classifier`
interface using TypeSafe's System One HTTP API. See the
[classifier guide](classifier.md) for the provider-independent contracts. This is the adapter stage of
[RFC #16](https://github.com/agentscope-ai/agentscope-go/issues/16). It does not
select a generation model or implement managed admission, authorization or
routing. Hosts must authorize the destination and project both state and question
content **before** calling `Classify`.

## Use

```go
import (
    "context"
    "encoding/json"
    "os"
    "time"

    "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
    "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier/jev"
)

func classify(ctx context.Context) (*classifier.Response, error) {
    client, err := jev.New(jev.Config{
        APIKey: os.Getenv("TYPESAFE_API_KEY"),
        Model: "jev-1.13.0", // explicitly choose a version supported by your service
    })
    if err != nil {
        return nil, err
    }
    ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
    defer cancel()
    return client.Classify(ctx, classifier.Request{
        OperationID: "ticket-1",
        State: json.RawMessage(`"The export button fails."`),
        Questions: map[string]classifier.Question{
            "bug": &classifier.BinaryQuestion{Instructions: "Does this report a software bug?"},
        },
    })
}
```

Always check the error before reading answers. The adapter returns a non-nil
report even on failure; constructor failure is separate. The existing
`go run ./examples/classifier` demonstrates typed questions, fake results,
cancellation and accounting without credentials or network access.

`Config.Model` and `APIKey` are required. The client does not select an alias or
read environment variables implicitly. `Endpoint` is the complete POST URL;
its default is `https://api.typesafe.ai/v1/systemone`. Custom HTTP(S) endpoints
must not contain userinfo, query parameters or fragments. Use HTTPS for remote
services; HTTP is supported for local fixtures. Endpoint choice is a host trust
decision, not an allowlist enforced by the library.

`MaxRequestBytes` and `MaxResponseBytes` default to 1 MiB and 4 MiB. The request
limit applies to serialized JSON before dispatch; serialization itself allocates
the payload. The response limit bounds bytes read after HTTP decompression.
Oversized bodies fail without truncating answers into a successful result.

A supplied `HTTPClient` is copied; its transport remains shared. Its `Timeout`
must be zero because the caller's context owns the overall deadline. Redirects
and cookie jars are disabled on the copy. A custom transport must honor context,
be concurrency-safe, and perform no retries or hidden requests. The adapter
cannot enforce these properties inside caller-supplied transport code.

## Protocol and validation

The wire mapping follows the [official API reference](https://docs.typesafe.ai/api):
Binary uses `noul`; Choice sends an option map; Score sends ordered levels.
Score responses use stringified indices, which are checked against the returned
legend and converted to probabilities in the original level order. The adapter
requires 2–10 Score levels and at most 255 Choice options before dispatch. These
are stricter than the provider-independent classifier contract.

All three answer types pass `classifier.ValidateResponse`, including exact
question IDs, concrete returned model identity, distributions and expected score.
Missing or null required numeric fields fail rather than becoming zero. Unknown
additional JSON fields are ignored for forward compatibility. The Go interface
accepts string/object state and string instructions/criteria; it does not expose
all structured question formats supported by TypeSafe.

## Failures and accounting

- Invalid configuration, input, request limits or cancellation before dispatch
  produce zero attempts. A dispatch starts at entry to `http.Client.Do`;
  connection failure does not prove server receipt, but still counts as an attempt.
- Each dispatched call retains one `PhysicalAttempt`, including HTTP errors,
  malformed responses, cancellation and read failures. There are no automatic
  retries or redirects. A caller may use the error's retry hint for its own policy.
- `Attempt.RequestedModel` is the configured name. `Response.Model` and
  `Attempt.Model` come from the response, never from a requested alias.
- Usage is recorded only on the attempt. Missing counts are nil; a reported zero
  is a pointer to zero. Valid counts survive malformed answers and HTTP errors.
  Invalid counts remain unknown, while valid sibling counts survive. Unparseable,
  truncated or oversized JSON cannot provide trustworthy usage.
- Errors use `errors.AgentError`: local validation is `classifier.invalid_request`,
  response validation is `classifier.invalid_response`, and HTTP/transport failures
  are `classifier.failed`. After a complete response body is read within the
  limit, non-2xx responses wrap `*jev.HTTPError` with `StatusCode`. 429 and 5xx
  (including 529) then carry `Retryable=true`; a valid `Retry-After` supplies a
  delay hint. Authentication and parameter failures are non-retryable. Read
  failures or oversized bodies take precedence over HTTP status and carry no
  retry hint.
- Cancellation and errors matching either context sentinel use the non-retryable
  `classifier.canceled` code and remain matchable with `errors.Is`. Cancellation
  observed before return takes precedence over a late success or other failure.
  Other transport failures omit raw causes and have no retry hint.

The adapter emits no logs. Returned error chains omit raw provider bodies,
transport error strings, state and credentials. Host transports and host logging
must follow the same data-handling rules. Successful answer IDs, choices, model
identity and counts are still provider data, not authorization decisions.

## Verification

```bash
go test -race -count=1 ./pkg/agentscope/classifier/jev
```

Offline tests use local HTTP fixtures or custom transports. They establish wire
conversion and failure behavior, not real service quality, cost or availability.
The live contract test is explicitly opt-in, sends only synthetic test data to
the default TypeSafe endpoint, and performs one potentially billable request:

```bash
JEV_LIVE_TEST=1 JEV_LIVE_MODEL=jev-1.13.0 \
  go test -v -count=1 -run '^TestLiveContract$' ./pkg/agentscope/classifier/jev
```

Set `TYPESAFE_API_KEY` securely in the environment first. Without opt-in or a key,
the test reports **SKIP**, not live verification. It requires a fixed three-part
model version, verifies the returned version, and logs requested/returned model
identities. It checks the protocol, not semantic answer quality or savings.

## Python mapping

The [Python Jev adapter](https://github.com/agentscope-ai/agentscope/blob/083cbd1975c7b5ed055c69b0d19c150727e7f606/src/agentscope/classifier/_jev/_model.py)
uses the TypeSafe SDK. Go implements the HTTP protocol with the standard library,
adds bounded bodies and attempt accounting, and deliberately omits SDK retries
and independent request timeouts to preserve the Go classifier contract. The
shared `internal/httpx.DoJSONRequest` helper retries and reads unbounded response
bodies, so it is not used on this single-attempt path. No new dependency is needed.
