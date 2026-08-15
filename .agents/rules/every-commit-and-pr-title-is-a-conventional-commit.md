# Every commit message and every pull request title is a Conventional Commit

This repository follows the [Conventional Commits 1.0.0](https://www.conventionalcommits.org/en/v1.0.0/)
specification. It is the standard, not a local variant — when this file and the spec disagree,
the spec wins.

```
<type>[optional scope][optional !]: <description>

[optional body]

[optional footer(s)]
```

**Types.** `feat` and `fix` are the two the spec defines; the rest are the commonly used set and
are what this repo uses.

| Type | Use for | SemVer |
|---|---|---|
| `feat` | a new feature | MINOR |
| `fix` | a bug fix | PATCH |
| `docs` | documentation only — including `AGENTS.md`, `.agents/rules/`, ADRs | — |
| `refactor` | a change that neither fixes a bug nor adds a feature | — |
| `perf` | a change that improves performance | — |
| `test` | adding or correcting tests | — |
| `build` | the build system or dependencies — `go.mod`, goreleaser | — |
| `ci` | CI configuration and workflows | — |
| `chore` | anything that changes neither source nor tests | — |
| `revert` | reverts an earlier commit | — |

**Scope** is optional, and is a noun naming the part of the codebase affected — here, naturally
the package or layer: `fix(logging):`, `feat(api):`, `refactor(repository):`.

**Breaking changes** are marked with `!` before the colon, a `BREAKING CHANGE: <description>`
footer, or both. `BREAKING CHANGE` is the one token the spec requires to be uppercase.

**The convention governs the subject line only.** It says nothing about the body, and this repo
deliberately writes long, explanatory bodies — what was tried, what was rejected, what was
measured. Adopting Conventional Commits is not an instruction to write shorter commits.

## Applies to

| | |
|---|---|
| every commit message | the subject line |
| **every pull request title** | this is the one that is easy to forget, and the one that matters most — see `## Why` |
| the **Merge Commit Message** section required in each PR description | it becomes a commit, so it follows the same format |
| release automation, if it is added later | `feat`/`fix`/`BREAKING CHANGE` are what a changelog or a semver bump would be derived from |

## Example

**WRONG** — a PR title that reads like a sentence:

```
Scaffold the core module, its agentic documentation, and CI
```

**RIGHT** — the same change, typed:

```
feat: scaffold the core module, its agentic documentation, and CI
```

More, with scopes and a breaking change:

```
fix(logging): follow a credential across terminal wrapping when redacting
docs(adr): record why the daemon owns its own binary
ci: add bulwark scan and the coverage gate
feat(api)!: require a bearer token on every route

BREAKING CHANGE: unauthenticated requests to /v1/health now return 401.
```

## Why

**Pull requests here are squash-merged, so the PR title is the commit message on `main`.** Not a
label, not metadata — the literal history. `main` currently shows both outcomes side by side:

```
3f6dd4f fix: pin claude code to 2.1.233 and redact credentials across terminal wrapping (#4)
7dc70e4 Scaffold the core module, its agentic documentation, and CI (#1)
```

The second one is what this rule exists to prevent, and it shows why care over the branch's own
commit messages is not sufficient: however well-formed the commits on a branch are, squashing
discards all of them and keeps the title. A conventional branch under a prose PR title still
produces a non-conventional `main`.

The payoff is a history that a machine can read. `feat` and `fix` map onto MINOR and PATCH, and
`BREAKING CHANGE` onto MAJOR, so a changelog and a version bump can be derived from the log
rather than maintained by hand — which matters here specifically, because tumika self-updates
and compares releases by semver. Nothing generates that today; the convention is what keeps the
option open without a history rewrite.
