# Code-scanning dispositions

This note records how code-scanning alerts are triaged for `agentscope-go`:
which are **by design**, which are **mitigated by an existing guard**, which are
**false positives**, and which are **genuine gaps pending a fix**. It is the
reference cited when an alert is dismissed, so the
[Code scanning tab](https://github.com/agentscope-ai/agentscope-go/security/code-scanning)
reflects reality rather than an undifferentiated backlog.

Dispositions are stated against a base revision (currently `b81fddf`). A guard
that does not exist at that revision is **not** a basis for dismissal. Genuine
gaps are fixed in code, not dismissed here.

It complements [SECURITY.md](../SECURITY.md): anything confirmed exploitable is
reported privately per that policy, never dismissed publicly.

## Threat model in one paragraph

AgentScope Go runs LLM-driven tools, including shell execution and file access.
Those capabilities are the product. SECURITY.md recommends deployment controls
(the permission engine, sandbox workspace backends, authentication) and notes
that no backend in this repo enforces network allowlists or resource limits;
STABILITY.md ("Codex-alignment non-goals", OS-level command sandbox row) states
that a library cannot confine its host process and that per-process syscall
policy is a deployment concern; "Open hardening work" tracks the remaining
sandbox, network-allowlist and resource-limit enforcement. The permission engine
(`agent.WithPermissionContext`) and the workspace jail (`tool.WithWorkspaceRoot`)
are **opt-in**. By-design dismissals below rely on the intended capability plus
those deployment duties — not on a guard that a given code path does not pass
through.

## go/command-injection

Shell execution is the product (`pkg/agentscope/tool/builtin_bash.go`,
`pkg/agentscope/platform/shell_detect.go`, and the container `Execute` paths).

- The agent tool path can be gated by the permission engine when a deployer
  configures it. But the traced CodeQL flows for the local-shell alerts reach
  execution through direct tool calls (e.g. `pkg/agentscope/mcp/server.go` and
  `pkg/agentscope/thread/participant/lease.go`) that do **not** pass through the
  agent/orchestrator permission gate; `platform.Command`, `LocalBackend`, and
  workspace `Execute` do not themselves enforce it. Disposition: **by design**
  (intended capability), relying on deployment authentication and sandboxing per
  SECURITY.md — not on the permission engine for these paths.
- `pkg/agentscope/workspace/docker.go` / `pkg/agentscope/workspace/applecontainer.go` `Execute`: the
  command runs **inside the container**, the trust boundary. Disposition:
  **by design** (container-scoped).
- The `%q` path interpolation in container *file* operations (alerts #89/#90) is
  a real container-scoped injection. A fix is **open** (single-quoting paths via
  `shellQuote`, PR #194); it is not landed at `b81fddf`, so these alerts are
  **pending a fix, not dismissed**.

## go/request-forgery

`pkg/agentscope/tool/builtin_webfetch.go` (alert #30): the default client is
SSRF-safe (`pkg/agentscope/tool/webfetch_ssrf.go`, `newSSRFSafeClient` /
`ssrfSafeDialContext`). It vetoes **every resolved IP** at dial time (loopback,
RFC1918, ULA, link-local incl. the cloud-metadata endpoint), dials the vetted IP
directly (anti-rebinding), and applies per connection so redirects are covered.
The initial URL is normalized to HTTPS; the dial-time IP guard covers every
connection including redirects. There is no exported setter for a custom client
at this revision, so the guarded path is the only reachable one. Disposition:
**mitigated**; model `ssrfSafeDialContext` as a sanitizer if a client option is
ever added.

## go/path-injection

This rule fires on any user-influenced path reaching a filesystem operation.
The cluster splits into three groups:

1. **Genuine gaps at `b81fddf` — fixes now landed.** The guards were absent from
   `b81fddf`; the fixes have since merged, and the original alerts are re-triaged
   under Post-fix re-flags below:
   - workspace-creation ID validation (`app.WorkspaceManager.GetOrCreate`) —
     #191, **merged at `4dec635`**; alert #78 re-flagged on main and is triaged
     under Post-fix re-flags below;
   - bubblewrap symlink + dangling-chain confinement — #192, **merged at
     `feb0a92`**; original alerts #74-#77 re-flagged on main, triaged below;
   - hub installer card-ID validation (`SkillHub`/`MCPHub.Install`) — #193,
     **merged at `4d53b5a`**;
   - file-tool jail confinement (`view_text_file`/`Glob`/`LSP` routed through
     `resolvePath`) — #195, **merged at `1e46758`**; original alerts
     #57/#58/#63/#70 re-flagged on main, triaged below. At `b81fddf` these three
     tools ignored the jail even when a root was configured.
2. **Guarded by design** (dismiss with this note, citing the base revision):
   - `pkg/agentscope/skill/store.go`: `validateAgentID`/`validSkillDirName` reject
     leading dots and separators (lexical traversal guards, not a positive
     charset allowlist); `SkillDirName` sanitizes names; `copyDir` skips symlink
     entries. These are traversal guards and symlink-skipping copy, not an
     absolute symlink-resolved containment guarantee.
   - `pkg/agentscope/workspace/workspace.go` `LocalWorkspace.resolve`: always-on
     lexical **and** symlink containment (`evalSymlinksLenient` + separator-aware
     re-check).
   - `pkg/agentscope/tool/backend.go` `LocalBackend`: a raw adapter by design;
     confinement is the tool/workspace layer's responsibility, not the backend's.
   - `pkg/agentscope/tool/read_cache.go`: `os.Stat` freshness probes; metadata
     only, no content sink. The built-in local Read callers pass already-resolved
     paths; `ReadCache` itself accepts arbitrary caller paths.
   - `pkg/agentscope/internal/fsutil`: a sink helper. The traced callers (`builtin_write`,
     `builtin_apply_patch`, `builtin_notebook`) reach it through either the
     conditional tool jail (`resolvePath`, unconfined when no root is set) or the
     always-on `LocalWorkspace.resolve`.
3. **Confinement-mechanism metadata reads**: a confinement check reads path
   metadata (`Lstat`/`Readlink`/`EvalSymlinks`) on the user-influenced path while
   resolving it. These are part of the containment mechanism and are
   metadata-only. Note the check is not atomic with the later filesystem
   operation (a check→use residual shared with `LocalWorkspace`), and lenient
   symlink resolution can fall back to the original path on error — so these are
   not a blanket "proven contained" guarantee. Triage each against its guard
   rather than dismissing the class wholesale.

The workspace jail is opt-in; with no root configured, file tools are
unconfined by design (the deployer's choice).

## go/allocation-size-overflow

`pkg/agentscope/agent/compress.go:1048`
`make([]*message.Msg, 0, len(msgsToCompress)+3)` (alert #123): the operand is the
length of an already-allocated in-memory slice. Overflow would require a slice
near 2^63 elements on 64-bit (or beyond the 32-bit address space), unreachable on
every supported platform. Disposition: **false positive** (unreachable).

## Post-fix re-flags: CodeQL does not model the added guard

A correct fix can still leave a CodeQL alert: the rule re-flags the sink because
the new guard is not a modeled sanitizer. These alerts appear on the fix PR's
ref before merge and can reappear on main once the fix merges and CodeQL
re-analyzes; they are triaged against the guard's source, not dismissed as a
class. Dismissing a PR-ref re-flag does not close the original `b81fddf` alert;
that is re-triaged when the fix merges and main is re-analyzed.

- **workspace ID validation (PR #191, alert #78, merged at `4dec635`)**:
  `GetOrCreate` and `Share` validate the workspace ID with `validWorkspaceID`
  (`pkg/agentscope/app/workspace_manager.go:49`), which rejects empty IDs,
  leading dots and any `/` or `\` (a single safe path component), before
  `createWorkspace` joins it under `baseDir` and `NewLocalWorkspace` calls
  `os.MkdirAll` (`workspace.go:108`). In `Share` the `sessionID` only keys the
  bindings map, never the path. CodeQL does not model `validWorkspaceID`, so the
  sink re-flagged on main after the merge. Disposition: **false positive**
  (validation guard present at `4dec635`).
- **bubblewrap symlink peel (PR #192, alerts #139/#140/#141)**: `resolve`
  returns `cleaned`, which is independently contained (separator-aware lexical
  check plus the `evalSymlinksLenient` re-check). The added peel loop's
  `os.Lstat`/`os.Readlink` (`pkg/agentscope/workspace/bubblewrap.go:215/219/229`)
  are metadata-only reads that decide whether to **reject** a dangling symlink
  chain; the peeled `target` is never returned and never used for content I/O.
  Disposition: **false positive** (confinement mechanism, category 3 above).
- **container file paths (PR #194, alerts #142/#143)**: the flagged `sh -c`
  sinks are the `WriteFile` parent-directory `mkdir`
  (`applecontainer.go:111`, `shellQuote(dir)`) and the docker write
  (`docker.go:108`, `cat > shellQuote(fullPath)`). Both interpolate the path
  through `shellQuote` (`pkg/agentscope/workspace/k8s.go:517`), the canonical
  POSIX single-quote escape (`'` -> `'\''`); the applecontainer `tee` write
  passes the path as argv (no shell). CodeQL still flags the sinks because
  `shellQuote` is not a modeled sanitizer. The commands run **inside the
  container** (the trust boundary). Disposition: **false positive** (correctly
  quoted; container-scoped).

- **bubblewrap file sinks (PR #192, alerts #74/#75/#76/#77 on main `1e46758`)**:
  the `os.MkdirAll`/`os.WriteFile`/`os.ReadFile`/`os.ReadDir` sinks
  (`bubblewrap.go:106/110/119/129`) are all fed by `w.resolve`, which now applies
  separator-aware lexical containment plus `evalSymlinksLenient` and the
  dangling-chain peel (#192). CodeQL does not model `resolve` as a sanitizer, so
  the sinks re-flagged on main after the merge. Disposition: **false positive**
  (confinement guard present at `1e46758`).
- **file-tool jail sinks (PR #195, alerts #57/#58/#63/#70 on main `1e46758`)**:
  `view_text_file`'s `os.Stat`/`os.ReadFile` (`builtin.go:138/152`), Glob's
  `os.Stat` (`builtin_glob.go:87`, with `confineGlobMatches` dropping out-of-root
  matches) and the LSP `didOpen` `os.ReadFile` (`lsp/server.go:100`) are fed by
  `resolvePath`, which routes through the workspace jail when a root is set
  (#195). CodeQL does not model `resolvePath` as a sanitizer. The jail is opt-in;
  with no root configured the tools are unconfined by design. Disposition:
  **false positive** (jail guard present at `1e46758`; opt-in confinement).

## OpenSSF Scorecard

- **Branch-Protection (#1) / Code-Review (#26)**: at `b81fddf`, `main` is **not
  protected** and required status checks are empty; Code-Review reports recent
  changeset approval coverage (e.g. 5/11). Enabling required reviews + required
  status checks is the remediation, not a completed disposition.
- **CIIBestPractices (#27)**: the CII Best Practices badge is an optional
  registration. Disposition: **won't fix** (optional) unless pursued.
- **Vulnerabilities (#28)**: the OSV list matches the whole dependency graph
  (32 advisories at `b81fddf`). `.github/workflows/security.yml` runs
  `govulncheck` as an **advisory** reachability scan (`continue-on-error: true`,
  `./pkg/...`), separate from Scorecard's OSV findings; it is not a blocking gate
  at this revision. The reachability scan reports 15 called vulnerabilities:
  Go standard-library issues (`net/http`, `crypto/x509`, `crypto/tls`,
  `net/textproto`, `html/template`) fixed only in Go 1.26.x;
  `golang.org/x/net@v0.57.0` (fixed in v0.60.0, which declares Go 1.26); and
  `google.golang.org/grpc@v1.83.0` (fixed in v1.83.1, which declares Go 1.25).
  Only the grpc patch bump is actionable on Go 1.25; the x/net and
  standard-library fixes require the Go 1.26 migration that `security.yml`
  defers ("advisory until Go 1.26"). This alert is **open pending the grpc bump
  and the Go 1.26 migration**, not a false positive: the findings are reachable.
- **Pinned-Dependencies**: addressed by pinning GitHub Actions and container
  images by hash; dependabot (`.github/dependabot.yml`) keeps the Actions and
  Dockerfile digests current (it does not update the apidiff commit pin).

## Keeping the backlog actionable

New Critical/High alerts should be triaged to an explicit disposition (fix,
dismiss-with-reason citing this note and the base revision, or false positive)
rather than left open. The intended gate is branch protection requiring the
CodeQL and CI checks plus required reviews before merge; it is not yet enabled
at `b81fddf`.
