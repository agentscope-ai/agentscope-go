# Local Piccolo text decisions

[Piccolo](https://github.com/agentscope-ai/Piccolo) returns candidate decisions
rather than generated chat text. For the text `/v1/systemone` path, the existing
experimental `classifier/jev` adapter can target QwenPaw's compatible llama.cpp
runtime; a separate adapter or `ChatModel` is not needed for the requests tested
here. See [RFC #109](https://github.com/agentscope-ai/agentscope-go/issues/109)
and the [Jev contract](jev.md) for the underlying API and accounting.

## Start with the offline example

From this repository's root:

```bash
go run ./examples/piccolo
```

This starts a loopback HTTP fixture, makes one actual HTTP attempt through Jev,
and closes the fixture. It needs no model download or external service. Its
output includes `"evidence":"fixture"` and synthetic probabilities and token
usage. It ignores all `SYSTEMONE_LOCAL_*` settings unless `-live` is supplied.

The example asks three questions about a fixed synthetic CSV-reading request:
Binary read-only probability, a two-option Choice (`read`/`ask`), and a two-level
Score (`low`/`high`). It displays judgments only. It does not read a CSV, execute
a tool, approve a permission, route a generation request or compact context.
The risk score is a probability-weighted rubric value, not a calibrated safety
threshold. Returned answers are validated before display; the actual returned
model must match the configured served name. Unknown token counts remain JSON
`null`; they are not estimated.

## Reproduce the CPU setup on Apple Silicon

The concrete baseline below is the official macOS ARM64 runtime in CPU mode,
with Piccolo-4B Q8_0. It was exercised on an M4 with 24 GiB unified memory and
macOS 15.3.1. Model files require approximately 4.48 GB of disk space; this file
size is not a runtime memory requirement. Other machines still need their own
capacity and compatibility check. For other platforms, see the upstream
[runtime guide](https://github.com/agentscope-ai/QwenPaw/blob/main/docs/local-piccolo.md).

The W0 environment configuration landed in [PR #111](https://github.com/agentscope-ai/agentscope-go/pull/111).
Use a source checkout containing that change and this example; the commands do
not establish availability in a particular published release.

### Pinned artifacts

| Artifact | Identity |
| --- | --- |
| QwenPaw runtime | `piccolo-v1`, `macos-arm64`; llama.cpp `f4d8af608a6b39946406f9439edfb6e83ca6a923` |
| Runtime patch SHA-256 | `e20000f7b4bcfd6fc497db93cfc9f5d3a8a88f961b4d30098152ebe21ba5ebca` |
| Runtime archive | `llama-piccolo-v1-bin-macos-arm64.tar.gz`, 17,909,806 bytes |
| Runtime archive SHA-256 | `5781dbacda99562ebade227219623dc93ecc5af7435aea5979b8d6c5890836ef` |
| Model | `agentscope-ai/Piccolo-4B-Q8_0`, revision `2e8d453a1efaea55ccfd1fb75e25ae83e0848305` |
| GGUF | `Piccolo-4B-Q8_0.gguf`, 4,482,403,872 bytes |
| GGUF SHA-256 | `1372d7521f1b570a0d9284115f7aca989b0ae15cc6dd62d7d125716fd860b69c` |

Compare the [runtime release identity](https://github.com/agentscope-ai/QwenPaw/blob/main/src/qwenpaw/local_models/runtime-release.json),
[published archive manifest](https://download.qwenpaw.agentscope.io/files/models/llama_cpp/piccolo-v1/runtime-release.json)
and [model manifest](https://github.com/agentscope-ai/Piccolo/blob/main/models/manifest.json).
A stock llama.cpp or Ollama chat endpoint does not implement this Piccolo
protocol. These commands use the compatible official bundle directly and do
not require installing the QwenPaw application or replacing an existing runtime.

### Download and verify

Use an otherwise empty directory. The commands below require `curl` and Python
3.12 or newer, and download about 4.5 GB. The model URL pins its revision; hashes
and bundle identity are checked before execution:

```bash
mkdir -p /tmp/piccolo-local
cd /tmp/piccolo-local
curl -fL --retry 3 -o llama-piccolo-v1-bin-macos-arm64.tar.gz \
  https://download.qwenpaw.agentscope.io/files/models/llama_cpp/piccolo-v1/llama-piccolo-v1-bin-macos-arm64.tar.gz
curl -fL --retry 3 -o Piccolo-4B-Q8_0.gguf \
  https://huggingface.co/agentscope-ai/Piccolo-4B-Q8_0/resolve/2e8d453a1efaea55ccfd1fb75e25ae83e0848305/Piccolo-4B-Q8_0.gguf
python3 - <<'PY'
import hashlib
import json
from pathlib import Path
import tarfile

expected = {
    "llama-piccolo-v1-bin-macos-arm64.tar.gz": (17909806, "5781dbacda99562ebade227219623dc93ecc5af7435aea5979b8d6c5890836ef"),
    "Piccolo-4B-Q8_0.gguf": (4482403872, "1372d7521f1b570a0d9284115f7aca989b0ae15cc6dd62d7d125716fd860b69c"),
}
for filename, (size, digest) in expected.items():
    path = Path(filename)
    with path.open("rb") as stream:
        actual = hashlib.file_digest(stream, "sha256").hexdigest()
    if path.stat().st_size != size or actual != digest:
        raise SystemExit(f"Artifact mismatch: {filename}")
with tarfile.open("llama-piccolo-v1-bin-macos-arm64.tar.gz") as archive:
    archive.extractall("runtime", filter="data")
identity = json.loads(Path("runtime/qwenpaw-runtime.json").read_text())
if not (
    identity.get("release") == "piccolo-v1"
    and identity.get("target") == "macos-arm64"
    and identity.get("upstream_commit") == "f4d8af608a6b39946406f9439edfb6e83ca6a923"
    and identity.get("patch_sha256") == "e20000f7b4bcfd6fc497db93cfc9f5d3a8a88f961b4d30098152ebe21ba5ebca"
    and {"chat", "piccolo"}.issubset(identity.get("capabilities", []))
):
    raise SystemExit("Unexpected runtime identity")
print("Artifacts and runtime identity verified")
PY
```

### Run the server

In that directory, keep the server running in its own terminal:

```bash
./runtime/llama-server \
  --host 127.0.0.1 --port 18609 \
  --model ./Piccolo-4B-Q8_0.gguf --alias Piccolo-4B-Q8_0 \
  --ctx-size 8192 --batch-size 8192 --ubatch-size 8192 \
  --parallel 1 --device none --gpu-layers 0 \
  --no-cache-prompt --cache-ram 0 --no-context-shift
```

The equal context/batch/microbatch limits, single slot and disabled cache and
context shifting preserve the upstream decision protocol. Each question's
complete compiled prompt must fit; this example does not measure large-input
capacity. Wait for the server to report that the model is loaded, then check:

```bash
curl -f http://127.0.0.1:18609/health
curl -f http://127.0.0.1:18609/v1/models
```

Expect model ID `Piccolo-4B-Q8_0` and `output_modalities: ["decisions"]`. This
sample server is loopback-only and has no configured authentication. The
example's URL check is not an OS sandbox or network isolation mechanism.

### Run the Go example and contract test

In another terminal, return to the agentscope-go repository root:

```bash
export SYSTEMONE_LOCAL_ENDPOINT=http://127.0.0.1:18609/v1/systemone
export SYSTEMONE_LOCAL_MODEL=Piccolo-4B-Q8_0
go run ./examples/piccolo -live
JEV_LIVE_TEST=1 go test -v -count=1 -run '^TestLiveContract$' ./pkg/agentscope/classifier/jev
```

Both commands use the existing [local configuration](jev.md#local-system-one-configuration-w0).
Endpoint and served-model overrides are required together; only loopback HTTP(S)
URLs without userinfo, query or fragment are accepted. `SYSTEMONE_LOCAL_API_KEY`
is optional and defaults to the non-empty placeholder `local-systemone`, because
`jev.New` requires a key even for unauthenticated servers. If the server requires
a key, set it securely in that variable; no `TYPESAFE_API_KEY` is reused.
`jev.New` itself never reads environment variables.

Live output is marked `"evidence":"live"`, indicating a real server response,
not a quality guarantee. The example uses a 30-second deadline and one primitive
request, with no retries. HTTP errors, malformed responses, cancellation and
returned-model mismatch cause failure before decision output. A writer failure
can still leave partially written output, so callers must check the exit status.
Stop the server with Ctrl-C when finished; it is not stopped by the Go client.

## Observed runtime differences and Metal caveat

The [W1 report](https://github.com/agentscope-ai/agentscope-go/issues/109#issuecomment-6678109407)
records a real CPU run against the pinned artifacts above at agentscope-go
`831e02aebaabef75b0e6ac53c05398ccc6f20360`. The unchanged live contract passed
for Binary, two-option Choice and two-level Score questions. Object and string
states also succeeded in direct requests. The W1 object-state request reported
369 input tokens and 0 output tokens; these counts belong to that request, not
the new example's longer questions. Offline HTTP fixtures verify wiring only.

| Input or field | Patched llama.cpp, observed in W1 | SGLang behavior described in RFC #109 |
| --- | --- | --- |
| Choice with one candidate | HTTP 400, criteria must have at least two options | HTTP 422 |
| Incorrect request model name | HTTP 200; response names the actually loaded model | HTTP 400 |

The raw successful W1 responses omitted `evaluation_trace` and
`usage.cached_tokens`; the Go codec does not consume those extra fields.

Use at least two Choice candidates and the exact served name on either runtime.
The example and contract test check the returned model, which is important when
a runtime ignores the request's model field. SGLang was not exercised in W1;
its column reports the RFC's source-based notes, not an equivalent live test.

On the tested M4/macOS 15.3.1 machine, `--gpu-layers auto` failed during Metal
shader compilation: `ggml_metal_library_compile_all` could not build `unary`,
with `constexpr function never produces a constant expression` in a `bfloat4`
cast. This is a platform-specific runtime observation, not a proven root cause.
CPU initialization still logged the Metal error during device scanning, but the
explicit CPU configuration above loaded the model and completed inference.
The result does not establish that GPU acceleration works.

This is limited text-protocol evidence. Semantic quality, safety calibration,
production readiness, long-context behavior, cancellation against a real model,
authentication enforcement, vision, other model variants, and SGLang or Metal
inference remain unverified. This guide adds no permission or compaction policy.
The [Python Jev adapter](https://github.com/agentscope-ai/agentscope/blob/083cbd1975c7b5ed055c69b0d19c150727e7f606/src/agentscope/classifier/_jev/_model.py)
also exposes an endpoint through credentials; this Go example reuses the existing
single-attempt HTTP adapter rather than adding Python's SDK dependency or retries.
