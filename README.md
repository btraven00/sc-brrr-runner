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
cp sc-brrr-runner.example.yaml sc-brrr-runner.yaml && chmod 600 sc-brrr-runner.yaml   # then fill it in
./sc-brrr-runner                 # drain the queue once (settings from sc-brrr-runner.yaml)
./sc-brrr-runner -push -watch 5m # the service: poll, score, push
```

**Configuration.** Every flag can be set in `sc-brrr-runner.yaml` (git-ignored; `-config` picks
another file) under its own name; flags on the command line win. `github-token` exists only there
(or as `$GH_TOKEN`), so it never shows in `ps`. The runner warns when the file is readable by others.

| key / flag | default | |
|---|---|---|
| `repo` | `../sc-brrr` | challenge checkout; cloned from `repo-url` if missing |
| `results` | `../sc-brrr-results` | results checkout; cloned from `results-url` if missing |
| `size` | `10k` | input size to score on |
| `push` | off | push both repos after each entry; without it everything stays local |
| `watch` | `0` | poll at this interval; `0` drains once and exits |
| `listen` | off | serve the live page and the kept logs here |
| `public-url` | none | the live page's public address, linked from PR comments |
| `state` | `~/.local/state/sc-brrr-runner` | kept logs and the lock (one runner per host) |
| `github-token` | `$GH_TOKEN` | config file only; see below |

Both checkouts must be clean: the runner commits, and won't sweep up stray changes. It
fast-forwards them before each pass.

**The token** is used for three things: git pushes (through a credential helper that reads it from
the environment: never in a URL or on a command line), finding the PR that added an entry, and
commenting on it. It is removed from the environment of `score.py` and `scoreboard.py`: their setup
step builds submitted envs with network on and must not see it. A fine-grained token needs Contents
read/write on both repos and Pull requests + Issues read/write on `sc-brrr`. Comments appear as the
token's owner: use a machine user (e.g. `sc-brrr-bot`, a collaborator on both repos) so they
don't come from a person; a GitHub App (`sc-brrr[bot]`) is the step after.

**PR comments.** When scoring starts, the runner finds the PR that added the entry (GitHub's
commits → pulls lookup on the commit that added the file) and comments with a link to the live
log; when it ends, it edits that comment with the outcome and a link to the result (results repo)
or to the failure record (`failed/…outcome.json`). No token, no PR found, or an API error: a warning
in the runner's log, never a stop.

**Logs are kept.** Every entry's log stays in `<state>/logs/<time>-<account>_<name>-<ver>.log`, and a
scored entry's log is also committed with its result (`score.log`). ponytail: no rotation; at about
10 KB per entry that is years of entries.

**Live page** (`listen`): `/` shows the running entry, the outcomes since the runner started and
every kept log; `/logs/<name>` serves a log, streaming it while its entry runs. Only names the runner
makes are served (`[\w.-]+\.log`, inside the logs dir).

**On a public machine.** Listen on localhost and put a reverse proxy with TLS in front (caddy,
nginx); set `public-url` to it. The page is read-only and unauthenticated by design: logs are of
public submissions, but they do contain the scoring host's paths. Run the runner as its own user,
with rootless podman, under systemd (`Restart=on-failure`: it exits on any error of its own).

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
- **A bot identity:** a GitHub App, so comments come from `sc-brrr[bot]` with short-lived tokens.
- **A check run on the PR** next to the comment, so the outcome shows in the PR's checks.
- **Run as a service:** a systemd unit with `Restart=on-failure` (the runner exits on any error of
  its own, by design).
