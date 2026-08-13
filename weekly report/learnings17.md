# Learnings — Week 17 (CI/CD, Quality Gates)

Report: [week17.md](week17.md)

## 1. Split jobs by what a failure MEANS

```
cpp-correctness  -> the engine computes the wrong answer
cpp-sanitizers   -> a memory / UB bug
go-test          -> the service layer is broken
lint             -> style
integration      -> the pieces don't fit together
```

One "test everything" job is simpler and throws away the diagnosis. **The value of a gate is that its
name tells you what to do before you open the logs.**

## 2. Never gate on a flaky signal

Timing tests are excluded from the merge gate for two *different* reasons:

- **shared runners** have noisy neighbours → a performance budget is a coin flip
- **sanitizers** are 2–3× slower → any budget is meaningless

They still run and print. **A flaky gate teaches people to ignore red builds**, and that costs more
than the regression it might catch. Measure everything; gate only on what is deterministic.

## 3. `-count=1` and `-race`

Go caches test results. A green run may mean **nothing ran**. `-count=1` disables it.

`-race` on any concurrent codebase, always. A data race that only appears under load is exactly what
must not reach production.

## 4. Check formatting, don't apply it

```yaml
- run: gofmt -l . | ... exit 1 if non-empty
```

A CI job that rewrites your code produces commits nobody reviewed, and a diff that appears out of
nowhere on your branch.

## 5. Real dependencies in CI when the semantics matter

The Go tests run against a **real Redis**, because they test Redis *semantics* — no per-member TTL,
consumer groups, `XPENDING`. A fake would hide the constraints those designs exist to work around.

**Mock what you own; run what you don't.**

## 6. Re-run tests at the tag

`release.yml` re-runs everything rather than trusting CI's result on the same commit. **A tag can
point at any commit**, including one that never went through a pull request. "It passed on main" is
a different claim from "it passes at this tag".

## 7. Version tags AND `:latest`

Publish both. **Deployments pin the version** — that is what makes a rollback one line rather than an
archaeology exercise. `:latest` is for convenience only.

Use the automatic scoped token (`GITHUB_TOKEN`), not a long-lived PAT: repo-scoped, expires with the
job, nothing to leak.

## 8. The failure that proved the gate works

```
can't load config: the Go language version (go1.23) used to build
golangci-lint is lower than the targeted Go version (1.26.4)
```

golangci-lint's **prebuilt binary** is compiled against its release's Go and refuses to run against a
newer module target. My linter config was broken and I would otherwise have believed lint was green.

Fix: `go install` it, so it is built with the **runner's** Go and the versions cannot drift.

**The general lesson: a tool with its own toolchain version is a version-skew bug waiting to happen.**
Building from source trades a minute of CI for a class of failures that look like your code is broken
when it isn't.

## 9. `concurrency.cancel-in-progress`

Three pushes in a minute should not burn three full runs when the first two are already irrelevant.
Free, one block of YAML.

## 10. Gates are not enforcement

Workflows **report**; **branch protection** blocks. That is a repository *setting*, not a file, so a
repo can have beautiful CI and still merge red. Worth knowing the distinction exists — and worth
saying so when it isn't enabled.

---

## Self-test
1. Why split CI into jobs by failure meaning rather than by convenience?
2. Give two distinct reasons not to gate merges on timing tests.
3. What does `-count=1` prevent?
4. Why check formatting rather than apply it?
5. Why re-run tests on a tag if CI already passed on that commit?
6. Why publish both a version tag and `:latest`, and which should a deployment use?
7. Your linter fails with a Go version error. What is happening and how do you fix it durably?
8. Your CI is green on every job but a red PR merged anyway. What is missing?
