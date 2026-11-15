## Contributing to agentscope-go

Thank you for considering contributing to `agentscope-go`!

This project aims to be a **production-grade, Go-idiomatic multi-agent LLM framework**,
mirroring the core concepts of the Python `AgentScope` project while embracing Go best
practices (`context.Context`, explicit `error` handling, small interfaces, etc.).

Before opening a pull request, please read this guide to keep the codebase clean and
consistent.

### Development workflow

1. **Fork & branch**
   - Fork the repository on GitHub.
   - Create a feature branch from `main` (for example: `feat/rag-qdrant`, `fix/react-agent-loop`).

2. **Go version**
   - The module requires **Go 1.25+** as declared in `go.mod`.
   - Please keep code compatible with Go 1.25 (the minimum version in `go.mod`).

3. **Dependencies**
   - Use the standard Go module tooling:
     - Add dependencies with `go get example.com/module@version`.
     - Run `go mod tidy` before committing when you change dependencies.
   - Prefer **small, well-maintained libraries** over large frameworks.
   - New dependencies should have a clear reason (performance, robustness, or missing
     functionality in the standard library).

4. **Build & test locally**

   From the repository root. This is what CI runs, on a 3-OS matrix
   (ubuntu / macos / windows):

   ```bash
   go build ./... && go build ./examples/...
   go vet ./...
   go test -race -count=1 ./...   # -race is mandatory: CI gates on it, and
                                  # several concurrency fixes are only visible
                                  # to the race detector
   golangci-lint run ./...        # v2; CI gates on 0 issues
   # install: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
   ```

   `go test ./...` without `-race` is not sufficient: it passes on races that CI
   then fails. After touching `pkg/agentscope/tool/bash_parser.go` or `safety.go`,
   also run the fuzz smoke:

   ```bash
   go test -run=x -fuzz='FuzzBashSafety$' -fuzztime=1000000x ./pkg/agentscope/tool/
   ```

   The `Nx` budget counts executions instead of elapsed time, avoiding a
   [Go 1.25 fuzz deadline race](https://github.com/golang/go/issues/75804).
   CI bounds the combined fuzz step with a separate 10-minute timeout. See
   [AGENTS.md](AGENTS.md#validation) for the content-block parser fuzz command
   and the full validation requirements.

   Coverage is a commit gate as well: `make cover-check` fails when statement
   coverage of `./pkg/...` drops below `COVERAGE_MIN` (value and rationale in
   `.github/workflows/ci.yml` and `AGENTS.md`). The PR checklist asks for the
   coverage of touched packages before and after the change.

   Some examples require external services:

   - LLMs:
     - `OPENAI_API_KEY`
     - `ANTHROPIC_API_KEY`
     - `DASHSCOPE_API_KEY` (+ optional `DASHSCOPE_BASE_URL`)
   - Qdrant (for RAG examples you may add):
     - Qdrant running locally or in the cloud.

### Code style & design

- **Go idioms first**
  - Always pass `context.Context` as the first argument for anything that may block or call
    external services.
  - Return `(T, error)` rather than panicking.
  - Keep exported APIs small and composable; favor interfaces that describe behavior, not
    data.

- **Logging**
  - Use the global `agentscope.Log()` (`logrus.Logger`) for internal logs.
  - Choose levels carefully:
    - `Debug` for noisy internal details (request/response metadata, retries, etc.).
    - `Info` for high-level lifecycle events (initialization, upserts, important actions).
    - `Warn` for recoverable problems (retryable errors, degraded behavior).
    - `Error` only when an operation ultimately fails.

- **HTTP / external calls**
  - Prefer the shared `pkg/agentscope/internal/httpx` helpers for outbound JSON APIs
    (LLMs, A2A HTTP, etc.) instead of reimplementing HTTP logic.
  - Make retries, timeouts, and error messages consistent with existing code.

- **RAG / vector DB integrations**
  - Implement new vector backends behind the `rag.Index` and/or `rag.KnowledgeBase`
    interfaces.
  - If adding a new backend (e.g. another vector DB), mirror the Qdrant implementation:
    - Provide a low-level index that assumes vectors are pre-computed.
    - Optionally provide a higher-level “text index” that uses an `Embedder`.

- **Tracing**
  - Use the `tracing.Tracer` interface and `tracing.SetupTracing` to integrate tracing.
  - For OpenTelemetry, prefer wiring through `tracing.OTELTracer` instead of using the
    OTEL SDK directly in business logic.

- **Security & audit**
  - Tool-facing code (anything in `tool/`) MUST NOT weaken existing safety checks.
    New tools should implement `CheckPermissions` with appropriate bypass-immune guards.
  - When adding new file operations or shell commands, check against `DangerousFiles`,
    `DangerousDirectories`, and `systemPathPrefixes` in `safety.go`.
  - New execution paths should emit audit entries via `audit.Logger` (passed through
    `OrchestratorConfig.AuditLogger`). At minimum: action type, tool name, input
    summary, decision (allowed/denied/ask), and duration.
  - Sandbox policy enforcement (`sandbox.Policy`) must remain backwards-compatible:
    nil policy = no restrictions (existing behavior).
  - Always run the shell-safety fuzz smoke above after modifying
    `bash_parser.go` or `safety.go`.

### Documentation & examples

- Keep `README.md` up to date whenever you:
  - Add a new major feature.
  - Introduce a new example under `examples/`, and add it to `docs/examples.md`
    and the README example table. Both tables are counted against `ls examples/`.
  - Change public APIs.

- `README.es-ES.md` follows a convention: feature additions may lag, factual
  corrections may not. If your change fixes a wrong number, a wrong API signature,
  or an overclaim in `README.md`, apply the same fix to `README.es-ES.md` in the
  same commit. New feature sections may wait for a translation pass.

- Code comments may cite internal review artifacts (`HARNESS_DESIGN`,
  `HARNESS review L-n`, `PORTING_PLAN`) that are not published; this repo
  `.gitignore`s them. Treat those tags as historical rationale rather than
  references a reader can look up. Do not add new ones. Write the rationale
  inline instead.

- For new features:
  - Prefer to add a **small, focused example** under `examples/` that demonstrates how to
    use the new capability in a realistic way.
  - If the feature maps to a Python AgentScope capability, briefly mention the mapping in
    the relevant `docs/` guide or in the PR description.

### Commit & PR guidelines

- **Commits**
  - Keep commits focused and logically grouped.
  - Use clear, descriptive messages (for example:
    - `feat(rag): add qdrant text index`
    - `fix(agent): avoid nil pointer in ReActAgent tools`
    - `docs: clarify Anthropic configuration`)

- **Pull requests**
  - Describe *what* and *why* in the PR description.
  - Mention any breaking changes explicitly and explain the migration path.
  - If the change affects public APIs, include or update examples and docs.

### Reporting issues

When filing an issue, please include:

- Go version (`go version`)
- OS / architecture
- Minimal reproduction code or steps
- Expected vs. actual behavior
- Any relevant logs (with secrets removed)

This helps maintainers triage and fix problems more quickly.

---

Thank you again for helping improve `agentscope-go`! Contributions of all kinds are
welcome: bug reports, documentation improvements, examples, and new backend integrations.

