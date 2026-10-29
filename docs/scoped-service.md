# Owner-scoped Agent Service

`service.NewScoped` is an Experimental, opt-in HTTP service for a host that
already authenticates callers. It keeps session creation, lookup, listing,
chat and confirmation in a host-defined owner scope. Existing `service.New`,
`Config` and `AgentFactory` keep their unscoped behavior.

Run the complete offline example:

```sh
go run ./examples/scoped_service
```

The example mounts one shared handler, maps fixed fixture credentials to trusted
context values, and constructs fresh agents with owner-specific read-only file
tools and separate permission contexts. Both tenants use the same user ID and
agent name. Their own tool reads succeed, foreign lookup returns 404, and listing
shows only the caller's session. The model is scripted, files are temporary, and
the server uses loopback. It does not test a real identity provider or model.

## Construction and identity

The additive API is:

```go
type ScopeResolver func(context.Context) (string, error)
type ContextAgentFactory func(context.Context, string, string, model.ChatModel) (*agent.UnifiedAgent, error)
func NewScoped(Config, model.ChatModel, ScopeResolver, ContextAgentFactory) (*Service, error)
```

Both hooks are required. The resolver reads the host's verified identity from
context, and returns an opaque owner key. Choose tenant-only scope for deliberate
tenant-wide sharing, or include both tenant and user for per-user ownership. Use
an unambiguous encoding of compound keys, such as JSON or a host-owned opaque ID.
Empty and whitespace-only keys are rejected; other keys are compared exactly,
without trimming. The service does not parse JWTs or trust JSON tenant fields,
`X-User-ID`, `X-Tenant-ID` or any other raw identity claim.

The factory receives the creation request's context, requested name/prompt and
the configured model. It must return a fully formed, fresh, idle agent exclusively
for that session. It may ignore a nil supplied model and provide its own, just as
the legacy factory can. Configure tools, permissions, workspaces and any persistent
saver namespace from the host identity. Do not capture the construction context
as a session-lifetime context: it ends when that HTTP request ends. Later replies
receive their own authenticated request contexts.

On factory error, nil agent or cancellation before the final registration check,
no session is published. The final context check under the registry mutex is the
construction admission point; later cancellation does not roll it back. The
registry owns admitted agent references, **not external-resource disposal**. The
factory/host owns resource lifetimes and cleanup on failure/canceled construction,
including a successful non-nil result discarded after cancellation. There is no
generic Agent close API or automatic disposer.

## Request order and session access

Every session route resolves identity before body reads, JSON decoding or a
body-limit error. Rejected identities therefore do not pay for reading even a
malformed or oversized body. Authenticated requests receive the default 10 MiB
cap, including when the host mounts `Handler()` instead of `ListenAndServe`.
JSON session/create/chat/confirm bodies must contain one complete bounded
document; trailing whitespace is counted against the cap, and a second document
is rejected. Legacy `New` keeps its single-Decode behavior.

Scope is bound at creation and checked again on each operation:

- `POST /api/session` constructs and registers in the current scope.
- `GET /api/session/{id}`, `DELETE /api/session/{id}`, `POST /api/chat`,
  `GET /api/chat/stream` and `POST /api/confirm` require that scope.
- `GET /api/sessions` returns only the current scope's sessions.

Foreign and unknown session IDs produce the same 404 response. Rejection happens
before Agent invocation or SSE output. Owner metadata is private and is not added
to session responses. Health/model routes and CORS preflight do not run the
session resolver; the host must apply its authentication policy to the whole
handler as appropriate.

One chat or stream owns a scoped session at a time; overlapping replies return
409. The slot stays owned until accepted execution cores have returned, including
after cancellation. A tool that ignores cancellation may delay that cleanup;
the service does not revoke side effects, abandon work to admit an overlapping
reply, or impose a new cleanup timeout. Get/list/delete remain session operations;
deletion does not revoke a previously admitted reply.

Generic resolver rejection is 401, malformed/invalid confirmation input is 400,
oversized input is 413, and canceled construction is 408. Factory error/nil returns
a generic 500 without exposing its error text. Requests admitted before later
identity or policy changes are not automatically revoked.

## Confirmation through the scoped stream

Consume `require_user_confirm` from the session's SSE stream, then submit the
complete set of that event's tool-call IDs and decisions as the owning caller:

```json
{"session_id":"<session>","tool_calls":[{"id":"<pending-call-id>","confirmed":true}]}
```

The scoped adapter retains the original server-emitted tool name, arguments and
metadata. Client-supplied tool name/input fields do not replace them. Empty,
unknown, duplicate, partial or mixed ID sets are rejected without consuming the
pending request. A current pending event accepts one complete response.

The input mailbox belongs to that stream/reply, not the session or raw Agent
channel. A confirmation queued while cancellation wins stays with the retired
reply, so a later reply reusing a call ID cannot consume it. SSE write failure
cancels that reply and clears its pending input. Buffered `POST /api/chat` retains
the Agent's existing behavior without an interactive host; use the stream for
human confirmation. External-result submission is not added by this slice.

The unchanged ID-only HTTP payload does **not** provide request-generation replay
protection when the same call ID asks again within one live reply, such as an
approval followed by a directory grant. Applications needing that distinction
need a host protocol with request-event identifiers; do not treat this owner
check as a general replay-protection protocol.

## Scope and reference designs

Sessions, ownership and pending input are process-local. There are no durable
owner records, recovery protocol, storage-namespace implementation, sharing/RBAC
or permission refresh. `Config.Storage` is not turned into a persistent session
registry. Host authentication, saver namespaces, actual tool guards and resource
lifetimes remain separate responsibilities.

[Python's session router at `e763b351`](https://github.com/agentscope-ai/agentscope/blob/e763b351538c8d7b52f23565f8873a33fc5a5edc/src/agentscope/app/_router/_session.py#L189-L245)
carries user identity into storage queries. Its temporary raw-header identity
dependency is not the authentication mechanism to copy. Go uses the host's
trusted context and an opaque scope instead of Python's app/storage model.
The direction and minimum scope were
[accepted in #83](https://github.com/agentscope-ai/agentscope-go/issues/83#issuecomment-6487314667).
