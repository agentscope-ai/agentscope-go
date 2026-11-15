# MiniMax Messages API

Call a MiniMax model with a local weather tool, retain its full assistant
response, and stream the follow-up answer:

```bash
export MINIMAX_API_KEY='your-api-key'
export MINIMAX_MODEL='your-enabled-model-id'
# Optional complete API prefix:
# export MINIMAX_BASE_URL='https://api.minimax.io/anthropic'
go run ./examples/minimax
```

This makes live, billable model requests. Weather data is a fixed local fixture.
The model decides whether to call the tool (`auto`); if it replies directly, the
example prints that response. Thinking controls are left unset for the model
default. The configured 4096-token output limit includes thinking; raise it if
the model exhausts that limit before answering.

See [MiniMax provider configuration](../../docs/model-providers.md#minimax) for
thinking/effort options, model constraints and scope. App credential/provider
registration is not included in this direct-construction example.
