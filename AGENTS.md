# Contributing with coding agents

This guide applies to work in **agentscope-go**, the Go implementation of
[AgentScope](https://github.com/agentscope-ai/agentscope). It is written for
coding agents and the people reviewing their contributions. The contributor
remains responsible for the accuracy, scope and quality of submitted work.

Read this file first, then the relevant parts of the architecture map in
[CLAUDE.md](CLAUDE.md). Use these sources for their respective responsibilities:

| Topic | Source |
|---|---|
| Contributor setup and PR process | [CONTRIBUTING.md](CONTRIBUTING.md) |
| Architecture and implementation conventions | [CLAUDE.md](CLAUDE.md), the relevant source and tests |
| API stability and known limitations | [STABILITY.md](STABILITY.md) |
| Executable checks and tool configuration | [Makefile](Makefile), [CI workflow](.github/workflows/ci.yml), [lint configuration](.golangci.yml), [go.mod](go.mod) |
| PR checklist | [PR template](.github/PULL_REQUEST_TEMPLATE.md) |
| Community conduct | [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) |
| Private vulnerability reporting | [SECURITY.md](SECURITY.md) |
| Release history and user guides | [CHANGELOG.md](CHANGELOG.md), [docs/](docs/) |

When prose and implementation disagree, inspect the source and tests, identify
the discrepancy, and correct the affected documentation. Do not describe an
intended design as shipped behavior or weaken a check to make a change pass.

## Repository essentials

- The module is `github.com/agentscope-ai/agentscope-go/v2`; keep `/v2` in
  repository imports. Library code is under `pkg/agentscope/`, runnable
  examples under `examples/`.
- `go.mod` declares Go 1.25.0. Keep changes compatible with Go 1.25; a successful
  build on a newer toolchain alone does not establish minimum-version support.
- The project is Apache-2.0 licensed. Source files do not carry SPDX or license
  headers; do not add them.
- For capabilities already present in Python AgentScope, inspect the upstream
  implementation before designing the Go change. Record the relevant upstream
  link and explain deliberate differences; parity does not require copying
  Python APIs or dependencies.
- Check stability commitments in `STABILITY.md` before changing an exported API,
  configuration default, serialized format or observable behavior.

## Working on a contribution

1. **Establish the starting point.** Inspect the branch, worktree status and
   relevant history. Read the issue and existing discussion; check for related
   PRs before duplicating work. Preserve unrelated edits and commits. Never
   reset, clean or overwrite someone else's work to obtain a clean baseline.
2. **Verify the problem.** Trace the affected call path and reproduce the failure
   where practical. Record the commit, configuration and environment used.
   Distinguish source inspection, local fixture tests and live-provider results.
   Existing tests passing is not evidence that an untested behavior is correct.
3. **Keep the change reviewable.** Prefer a focused fix or feature with a clear
   outcome. Separate independent improvements and avoid unrelated formatting,
   dependency upgrades or refactors. Discuss substantial API or architectural
   choices before committing to an implementation.
4. **Test the behavior.** For changed or new code behavior, add a regression or
   acceptance test and observe the relevant failure before implementing it.
   Exported APIs need tests, including in packages without existing tests.
   Exercise boundaries and error paths; do not write assertions that merely
   repeat the implementation.
   Documentation-only edits need fact, link and example checks, not artificial
   behavior tests.
5. **Update the affected documentation.** Keep public API and behavior changes
   consistent with the relevant guides, README sections and architecture map.
   Update example listings when examples change. Keep factual corrections in
   `README.md` and `README.es-ES.md` in the same commit; follow `CONTRIBUTING.md`
   for translation of new feature sections. Add an `[Unreleased]` changelog entry
   for notable changes, or explain in the PR why one is unnecessary.
6. **Review and validate.** Complete the checks and independent evaluator review
   below. Review the final diff, including generated files and documentation,
   before staging only the intended changes.

Use a fork and a topic branch from current `main` for normal contributions
(`fix/...`, `feat/...` or `docs/...`), then open a PR against `main`. An explicit
maintainer instruction to commit or push directly to `main` changes the delivery
route, not the quality gate. Before pushing, fetch and check the destination;
incorporate intervening changes and repeat affected checks and review. Do not
force-push shared branches.

Local inspection, edits and validation can proceed within the requested scope.
Posting comments, opening PRs, pushing commits or contacting people requires
authorization for that action. An explicit request already supplies that
authorization; do not ask for it again. Repository access alone is not permission
to publish on a contributor's behalf.

## Issues, reviews and community communication

Follow `CODE_OF_CONDUCT.md`. Write in the language of the discussion where
practical, use plain language, and focus criticism on the behavior or proposal.

- Acknowledge useful reports and explain what you could verify. Cite a source
  location or commit when it helps someone reproduce a finding.
- Separate confirmed defects, configuration limitations and proposed features.
  Document an existing workaround accurately without treating it as a fix.
- Ask only for information needed to resolve uncertainty, such as a version,
  minimal reproduction or sanitized log. Do not block an independently
  reproducible fix on unrelated environment details.
- Offer a manageable contribution path: a focused PR, regression test or docs
  improvement. Participation is optional; avoid assigning work to the reporter
  or promising a merge, release date or review on someone else's behalf.
- Keep public comments specific and concise. Avoid canned praise, repeated issue
  summaries and internal planning or evaluator transcripts. State validation
  results honestly; do not claim tests or deployments that were not performed.
- When a task requires evaluator review of a public reply, review the complete
  draft before posting and verify that the published text matches it.

Report suspected vulnerabilities privately through `SECURITY.md`, never in a
public issue. Keep credentials, private data and unredacted logs out of commits,
examples, comments and review artifacts shared publicly.

## Validation

Run commands from the repository root. The following are the commit checks;
`make check` is a convenience target, not a substitute for the whole list:

```bash
go build ./...
go build ./examples/...
go vet ./...
go test -race -count=1 ./...
golangci-lint run ./...
make cover-check
```

`golangci-lint` must be v2; CI installs
`github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest`. Follow
`.golangci.yml` and use a linter built with a toolchain compatible with the module.

For focused iteration, use a real test name, for example:

```bash
go test ./pkg/agentscope/agent -run '^TestContextConfigDefaults$' -count=1 -v
```

`make check` runs formatting, module tidying, vet, build and race tests. It does
**not** run lint or the coverage gate, and it can change files throughout the
worktree. Use it deliberately and inspect the diff afterward. `make cover`
reports statement coverage for both `./...` and `./pkg/...`. The Makefile uses
POSIX shell utilities and `/tmp`; on Windows use a compatible shell or the
individual Go commands and the equivalent coverage check in CI.

CI currently builds, vets and race-tests on Ubuntu, macOS and Windows with Go
1.25. The Ubuntu job also runs coverage, parser fuzz smoke and loop benchmarks.
Separate jobs run lint and Linux cross-compilation for arm64, arm, mips64le and
riscv64, including a stripped arm64 edge-binary size check. Treat the workflow as
the source for matrix details and limits. Local success on one platform does not
establish that the other jobs pass.

### Coverage and regression policy

- The library statement-coverage floor is **70.0%**. Keep `COVERAGE_MIN` in CI
  and the default in `make cover-check` synchronized. The floor is a ratchet:
  raise it when a change establishes a higher baseline. Lowering it requires an
  explicit justification and is a blocking review finding, not a way to pass CI.
- Measure `./pkg/...` separately with `go test -coverprofile`; do not substitute
  the `./...` profile or add `-coverpkg` when checking this floor. Those measure
  different coverage attribution. Build constraints also change the denominator:
  verify a proposed floor against the Ubuntu CI result before relying on a
  measurement from another OS. Examples must compile but are outside the
  library coverage gate.
- Report before/after coverage for touched Go packages using the same toolchain,
  package selection and test flags. Inspect `go tool cover -func` for the affected
  functions as well as totals. Touched packages must not lose coverage. For
  documentation-only changes, record package coverage comparison as not
  applicable; the repository coverage gate still runs.
- Do not publish a historical coverage number as the current result. Record the
  measured result with its command and revision.

Changes to shell safety checks or content-block parsing require the corresponding
fuzz smoke in addition to regression tests:

```bash
go test -run=x -fuzz='FuzzBashSafety$' -fuzztime=1000000x ./pkg/agentscope/tool/
go test -run=x -fuzz='FuzzUnmarshalContentBlocks$' -fuzztime=5000000x ./pkg/agentscope/message/
```

These are execution-count budgets (`Nx`), including seed replay and minimization,
not fixed durations or a guarantee of equivalent coverage across runs. They avoid
the [Go 1.25 timed-fuzz cancellation race](https://github.com/golang/go/issues/75804).
CI gives the combined fuzz step a separate 10-minute timeout; exceeding it fails
the job. Keep fuzz assertions and nonzero exit statuses intact.

Run the relevant tagged tests when changing code excluded from the default build
(for example the `mqtt` build tag). Document any required service, platform or
hardware and distinguish a skipped test from a passing one.

## Mandatory evaluator review

For issue-driven fixes, use an independent evaluator at two stages: challenge
the reproduced root cause and proposed fix before implementation, then review
the final code, tests and documentation. The evaluator should look for an
alternative explanation, a counterexample that still fails, and regressions
introduced by the fix. A design PASS does not replace final-diff review.

Before committing and pushing, have an independent evaluator (an adversarial
reviewer, such as a separate review agent) examine the final diff. Self-review
alone does not satisfy this gate. Give the evaluator the scope, relevant source,
base revision, validation results and known limitations.

The review must cover:

- **Code:** correctness, nil and error handling, cancellation, concurrency,
  resource ownership, boundary conditions, API compatibility and security.
- **Tests:** a meaningful regression signal, realistic fixtures, failure paths
  and coverage of each changed behavior. A mock result must not be presented as
  proof of live-service or hardware behavior.
- **Documentation:** API names and signatures checked against source, runnable
  examples checked by compilation or execution as appropriate, working links,
  reproducible numeric claims, and consistency with CI and stability policy.
- **Scope and communication:** an accurate description of the final change,
  migration guidance where needed, no unrelated edits and no unsupported claims.

The evaluator must return an explicit **PASS** or **FAIL**, with findings ranked
by severity and tied to evidence. Resolve correctness and factual findings,
then submit the revisions for re-review. PASS requires no unresolved blocking
findings; HIGH-severity findings always block. Keep the review result available
to the maintainer without committing internal transcripts to the repository.

Before publishing an issue reply, PR description or review, have the evaluator
review the complete text and its conclusion, including an approval or
request-changes decision. Claims must match the reviewed revision and distinguish
local fixtures from live-provider tests, as well as proposed, merged and released
behavior. Require an explicit publication PASS and verify the posted text matches
the approved draft. A publication PASS means the communication is sound; it does
not mean the PR being discussed has passed code review.

Commit only when build, vet, race tests, lint (zero issues), coverage and any
applicable fuzz checks pass, and the evaluator returns PASS. Use the full suite
by default; affected-package race tests may support a narrowly scoped change
when the scope and reason are recorded, but the other gates still apply and CI
runs the full suite. Record failures and missing checks honestly; do not relabel
an incomplete gate as PASS. If required validation or independent review cannot
be completed, report the blocker before committing or pushing.

Review applies to the diff that will be submitted. Subsequent changes need
review and validation appropriate to their impact; an earlier PASS does not
cover unreviewed edits.

## Commits, dependencies and releases

- Use Conventional Commit subjects, such as `fix(model): ...` or `docs: ...`.
  Explain the problem, resulting behavior and relevant validation in the PR;
  use the PR template and tick only checks that were actually completed.
- Declare source and behavior breaks explicitly, use `!` for breaking commits,
  and give a migration path consistent with `STABILITY.md`.
- Commit messages must not mention AI assistants or include `Co-Authored-By`
  trailers for them.
- Prefer standard-library solutions and small dependencies with compatible
  licenses (Apache-2.0, MIT or BSD). Call out new dependencies and their purpose
  in the PR. Use `go get` and `go mod tidy`; commit dependency metadata in
  `go.mod` and `go.sum` alongside the necessary source changes. `vendor/` is
  ignored and must not be committed.
- Tags are cut from `main` on the `/v2` semver line. Release work promotes
  `[Unreleased]` into a version section. Keep historical release references in
  the changelog; keep current support/version statements in `SECURITY.md` and
  `STABILITY.md`. Avoid hard-coded "latest" tags in guides. Distinguish behavior
  available on `main` from behavior in a published release.
