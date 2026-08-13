# Week 17 — CI/CD (and the gate catching its first real failure)

**Date:** Oct 29, 2026 · **Phase:** 5 (Production Orchestration) · **Status:** ✅ Complete

## What this week was about

Making every merge automatically prove itself, and making a tag produce a deployable artifact.

## Jobs split by what a failure MEANS

| Job | A red result means |
|---|---|
| `cpp-correctness` | the engine computes the **wrong answer** |
| `cpp-sanitizers` | a **memory or undefined-behaviour** bug |
| `go-test` | the **service layer** is broken |
| `lint` | style / vet |
| `integration` | the pieces **don't fit together** |

One "test everything" job would be simpler and would lose all of that. The point of a gate is that
its failure tells you what to do *before* you open the logs.

## Timing tests do not gate merges

Excluded from **both** C++ jobs, for two different reasons:

- **`cpp-correctness`**: a shared runner has noisy neighbours, so a performance budget measured there
  is a coin flip.
- **`cpp-sanitizers`**: ASan/UBSan are 2–3× slower, so any budget is meaningless.

They still run and print. **A flaky gate teaches people to ignore red builds**, which costs far more
than the regression it might have caught.

## Other decisions

**A real Redis service container** for the Go tests, because the Week 7/10 tests are about Redis
*semantics* — no per-member TTL, consumer groups, `XPENDING`. A fake would hide the constraints
those designs were built around.

**`-race` and `-count=1`.** The Go layer is all concurrency, and a cached green result is not
evidence the tests ran.

**gofmt is checked, not applied.** A CI job that rewrites your code produces commits nobody reviewed.

**The integration job re-asserts earlier checkpoints on every commit:** the compose stack becomes
healthy (Week 14) and a malformed request gets a 4xx, never a 500 (Week 11). Those were verified once
by hand; now they are verified continuously.

**`concurrency.cancel-in-progress`** — pushing three fixes in a minute should not burn three full
runs when the first two are already irrelevant.

## Release automation

Runs only on a `v*.*.*` tag, and **re-runs the tests** rather than trusting CI's result on the same
commit. A tag can point at any commit, including one that never saw a pull request; "it passed on
main" is a different statement from "it passes at this tag".

Images go to ghcr.io with **both** an immutable version tag and `:latest`. Deployments pin the
version — `:latest` is convenience, and pinning is what makes a rollback one line instead of
archaeology. The automatic `GITHUB_TOKEN` rather than a long-lived PAT: scoped to the repo, expires
with the job.

## The checkpoint, demonstrated the hard way

> ✅ A red test blocks the merge.

The first run **failed**, on something real:

```
can't load config: the Go language version (go1.23) used to build
golangci-lint is lower than the targeted Go version (1.26.4)
```

golangci-lint's prebuilt binary is compiled against whatever Go its release used, and it refuses to
run against a newer module target. **That is the gate doing its job** — my linter configuration was
broken, and I would otherwise have believed lint was passing.

Fixed by installing it with `go install`, which builds it with the *runner's* Go so the two cannot
drift, and migrating the config to golangci-lint v2's schema. It costs about a minute and removes a
whole class of "the linter is broken, not the code" failures.

## Honest gap

**Branch protection is not enabled.** Requiring these checks before a merge is a repository
*setting*, not a file in the repo, so the gates currently report without literally blocking. That is
one settings change away, and it is called out rather than glossed.

## Files touched

`.github/workflows/{ci,release}.yml`, `infrastructure/.golangci.yml`.
