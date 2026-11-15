# Chat Completions request fixtures

`sdk-buffered.json` and `sdk-streamed.json` were captured on 2026-10-29 from
[`github.com/openai/openai-go/v3` v3.90.0](https://github.com/openai/openai-go/tree/v3.90.0)
calling a local `httptest` server with this handler and a deterministic fake
model. The SDK's `Models.List`, `Chat.Completions.New`, and
`Chat.Completions.NewStreaming` calls succeeded; the stream parser observed
text, the terminal `stop` choice, and the requested usage chunk.

No provider endpoint was contacted. These fixtures establish local request
shape compatibility, not live provider support. They require no SDK dependency
in the library or its tests. The independently run SDK smoke does not replace
the committed cancellation, admission, history, tool and error tests.
