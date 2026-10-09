# sc-brrr-runner

The scoring service of the [sc-brrr](https://github.com/btraven00/sc-brrr) challenge: one Go
binary, standard library only. It drains the submission queue, runs each entry through the
challenge repo's own `score.py` (podman, one container per job), records the outcome in git and
rebuilds the [scoreboard](https://btraven00.github.io/sc-brrr-results/).

## The queue

In a checkout of `sc-brrr`:

```
incoming/<account>/<name>-<X.Y.Z>.yaml (+ .env.yml)     merged PR = reviewed and approved, waiting
submissions/<account>/…                                 scored: a result exists (jobs ok or failed)
failed/<account>/… (+ <name>-<X.Y.Z>.outcome.json)      not scored: rejected, or setup failed
```

For each entry in `incoming/`, oldest first (by the commit that added it):

1. `./score.py --no-commit --results <results> <entry>` in the challenge repo, with main's code.
2. By its `OUTCOME {json}` line (the last one; exit codes 0 / 10 / 20 / 30 say the same):
   - `ok` or `jobs_failed`: commit the result to the results checkout, rebuild the scoreboard
     (`scoreboard.py`) and commit it, `git mv` the entry to `submissions/`.
   - `rejected` or `setup_failed`: `git mv` the entry to `failed/` with `<name>-<X.Y.Z>.outcome.json`
     (outcome, reason, time, the last 80 lines of the log). To retry, `git mv` it back to `incoming/`.
   - no OUTCOME line: the scorer itself broke. The entry stays in `incoming/` and the runner stops.
3. With `-push`, push the results repo, then the challenge repo.

A failed run is never lost: scored failures (OOM, timeout, contract) are results like any other and
show on the scoreboard as failed; unscored ones are in `failed/` with their reason, and every move
is a commit.

## Running it

```sh
go build -o sc-brrr-runner .
./sc-brrr-runner                       # drain the queue once, commit locally, no push
./sc-brrr-runner -push -watch 5m       # the service: poll, score, push
./sc-brrr-runner -listen 127.0.0.1:8080  # plus the live page
```

| flag | default | |
|---|---|---|
| `-repo` | `../sc-brrr` | challenge checkout; cloned from `-repo-url` if missing |
| `-results` | `../sc-brrr-results` | results checkout; cloned from `-results-url` if missing |
| `-size` | `10k` | input size to score on |
| `-push` | off | push both repos after each entry; without it everything stays local |
| `-watch` | `0` | poll at this interval; `0` drains once and exits |
| `-listen` | off | serve the live page here |
| `-state` | `~/.local/state/sc-brrr-runner` | per-entry logs and the lock (one runner per host) |

Both checkouts must be clean: the runner commits, and won't sweep up stray changes. It
fast-forwards them before each pass.

**Credentials.** With `$GH_TOKEN` set, git authenticates with it (passed through a credential
helper, never in a URL or on the command line); otherwise git uses whatever it is configured with,
e.g. `gh auth setup-git`. The token needs push access to both repos, nothing else.

**Live page** (`-listen`): `/` shows the running entry and the recent outcomes; `/log` streams the
running entry's log as it grows, until the entry finishes. No authentication: bind it to localhost,
or put it behind a proxy that adds auth before exposing it.

## Roadmap

- **Contributors test their submission locally.** The same binary on a contributor's machine:
  `sc-brrr-runner try incoming/<account>/<name>-<X.Y.Z>.yaml` would clone the challenge repo,
  fetch the public test dataset, run `score.py` against a throwaway results checkout (no commits,
  no push) and print the outcome and the metrics, so an entry fails on the contributor's machine
  before it fails in the queue. Needs: the dataset published with a content hash (Zenodo, the plan's
  TODO), so the runner can download and verify it instead of reading the organisers' `file://`
  paths; `podman` (and CDI for `cuda`) on the contributor's machine.
- **Smoke runs on PRs, GPU included.** Not a self-hosted GitHub Actions runner: on a public repo a
  fork's PR could run code on it. Instead the runner pulls: it polls open PRs that carry an
  organiser's label (e.g. `smoke`), fetches only their `incoming/` files, runs them on the 10k
  input in the same sandbox (no network at run time, the GPU only for entries that require `cuda`),
  and posts a commit status to the PR. The label is the organiser's approval to run unreviewed code.
- **Re-scoring.** When the plan changes (new plan hash), re-run every entry in `submissions/`
  against it; the version gate already allows it.
- **Live page in public.** Per-run pages at unguessable URLs and a check run on the PR linking to
  them (the design in sc-brrr's `docs/infrastructure.typ`, Live progress).
- **Run as a service:** a systemd unit with `Restart=on-failure` (the runner exits on any error of
  its own, by design).
