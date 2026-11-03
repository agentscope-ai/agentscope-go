# Volcengine Ark chat

Stream a text reply through `model.NewVolcengineChatModel`:

```bash
export ARK_API_KEY='your-api-key'
export ARK_MODEL='your-enabled-model-id'
# Optional standard data-plane API prefix:
# export ARK_BASE_URL='https://ark.cn-beijing.volces.com/api/v3'
go run ./examples/volcengine
```

This makes a live, billable model request. The example leaves thinking controls
unset so the provider default applies. See [provider configuration](../../docs/model-providers.md#volcengine-ark)
for explicit thinking/effort options and supported scope. This example uses
direct Go construction; app credential registration is not included.
