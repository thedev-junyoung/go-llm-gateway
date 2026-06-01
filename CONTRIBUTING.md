# Contributing

`go-llm-gateway` is built with agent-assisted development. The decisions, code, and tests in this repository pass through a human-in-the-loop workflow that combines an LLM agent (for drafting and scaffolding) with a maintainer (for design judgment and review). This document describes that workflow so contributors know what to expect and how to participate.

## Status

Pre-release. Building toward v0.1.0 (target 2026-07-06). Public API may shift until that tag — see the "Stable in v0.1.0-rc" list in [README](README.md) for what is locked.

## Development workflow

Every change ships through the same loop. There are no shortcuts for "small" changes; the floor for a commit on `main` is the same as for a feature.

1. **Issue first.** Open a GitHub issue describing the problem, the proposed approach, the acceptance criteria, and what is intentionally out of scope. Title prefix follows Conventional Commits (`feat:`, `fix:`, `chore:`, `docs:`, `refactor:`, `test:`, `perf:`, `ci:`, `build:`).
2. **Branch from main.** Branch name pattern is `<type>/<issue-number>-<short-slug>`. Branching off another feature branch needs an explicit reason.
3. **Commit in small, atomic steps.** Conventional Commits title, explicit paths (`git add path/to/file` — never `git add .`), no `--no-verify`. The `Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>` trailer stays on every commit as transparency about how the code was produced.
4. **Open a PR linking to the issue with `Closes #N`.** PR body is plain prose: a short summary of what changed and any context a reviewer needs to evaluate the change. Use checkboxes only when there is a real action item.
5. **Wait for the AI review.** [`.github/workflows/ai-review.yml`](.github/workflows/ai-review.yml) runs two Claude Code Action jobs on every PR — `code-reviewer` against [`.claude/skills/pr-reviewer`](.claude/skills/pr-reviewer/SKILL.md), and `adr-critic` against [`.claude/skills/adr-critic`](.claude/skills/adr-critic/SKILL.md) when `docs/adr/**` changed. The maintainer reads the comments, responds with reasoning when disagreeing, and pushes fixups for legitimate findings.
6. **Squash-merge to main.** `main` history stays linear and one-commit-per-issue. The squash commit message uses the PR title and body.

`main` is protected: PRs only, status checks (`test`, `lint`, `code-reviewer`) required, conversation resolution required.

## Decision discipline

Architectural choices land as Architectural Decision Records under [`docs/adr/`](docs/adr) before the code that depends on them. The ADR captures context, the decision, alternatives considered, consequences, and open questions. The interface, error vocabulary, and routing/retry policy decisions in this repository were all locked through ADRs before any production code was written — that is the deliberate pace.

ADRs may be drafted with agent assistance; the header makes that explicit. They move from `Status: Proposed` to `Status: Accepted` after the maintainer has filled in the per-decision rationale in their own voice. Code may reference an ADR in either state.

## Architecture

`docs/design/architecture.md` lays out the module boundaries:

- `pkg/provider` is the base interface layer. Every other `pkg/*` (router, ratelimit, metrics, logging) depends on it in one direction only.
- Peer feature packages do not import each other. Composition happens at the top-level `gateway` package, never inside `pkg/*`.
- `internal/requestctx` is the shared request-id context helper; external callers reach it through `gateway.WithRequestID` / `gateway.RequestIDFromContext`.

## Style

- Go 1.24, formatted with `gofmt -s`, linted with `golangci-lint` v2.11.4 against [`.golangci.yml`](.golangci.yml). CI fails on either.
- Standard error patterns: `errors.Is` / `errors.As` with sentinels declared in `pkg/provider`. Vendor strings stay behind `*ProviderError.VendorMessage()`; never returned to external clients.
- `context.Context` is the first argument on any function that does I/O.
- Compile-time interface satisfaction (`var _ Iface = (*Impl)(nil)`) at file scope in the production file, not in tests.

## What you can contribute

- New provider adapters (Gemini, Bedrock, Vertex, ...) following the OpenAI/Anthropic templates in `pkg/provider/openai` and `pkg/provider/anthropic`.
- Ratelimit backends beyond in-memory and Redis (planned).
- Observability hooks (metrics, tracing, structured logging) once ADR-006 lands.
- Test coverage gaps — recorded provider fixtures (`go-vcr`), benchmark suite, fuzz targets.
- Documentation, examples, blog post critique.

For larger ideas (a new feature in `gateway.Config`, a new ADR-worthy decision), open an issue first to align on the design before opening a PR.

## What stays out

- Streaming and embedding APIs are deferred to v0.2 (see [`docs/design/v0.1-scope.md`](docs/design/v0.1-scope.md)).
- Semantic cache is intentionally out of scope (see [ADR-001](docs/adr/0001-why-go-llm-gateway.md) for the reasoning).
- An HTTP proxy mode is on the v0.2 roadmap, not v0.1.

## Reporting issues

Bugs, design questions, and feature requests all go to GitHub Issues. Reference the ADR or PR you are responding to where it helps. The AI review pipeline will not auto-respond to issues; the maintainer triages them.
