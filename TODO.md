# TODO: the end-to-end workflow

State on 2026-10-09: every piece works and is tested in throwaway clones (scored, setup failed,
rejected; live log; host paths redacted). Nothing of the runner is on GitHub yet, and no real PR
has gone through it.

## 1. First real run (no bot account needed)

PR comments are off (`comment: false`), so nothing is posted under a person's name; pushes use the
scoring machine's git credentials (`gh auth setup-git`), commits its git identity.

- [ ] Push sc-brrr's local commits: the `incoming/` queue (CI now checks `incoming/`, `score.py`'s
      OUTCOME contract) and the docs.
- [ ] Create `btraven00/sc-brrr-runner` on GitHub (public, like the other two) and push this repo.
- [ ] Open a submission PR adding `incoming/btraven00/exact-ref-0.1.4.yaml` + `.env.yml` (pinned to
      `f6ea2d7`); CI must go green on `incoming/`.
- [ ] Merge it.
- [ ] On the scoring machine: `sc-brrr-runner.yaml` from the example (`chmod 600`), then
      `./sc-brrr-runner -push -listen 127.0.0.1:8080`.
- [ ] Check: the entry moved to `submissions/` on main; the result (with `score.log`) and the
      rebuilt scoreboard are in sc-brrr-results; the scoreboard page shows 0.1.4.
- [ ] A deliberately broken entry (e.g. an entrypoint that doesn't exist) through the same path:
      it lands in `failed/` with its `outcome.json`, nothing in the results repo.

## 2. Bot identity, then PR comments

- [ ] Create the machine account (e.g. `omnibot`) at github.com/signup: only a person can do this.
- [ ] Invite it as a collaborator (write) on `sc-brrr` and `sc-brrr-results`
      (`gh api -X PUT repos/btraven00/<repo>/collaborators/<bot> -f permission=push`); accept as the bot.
- [ ] As the bot, create a token. Fine-grained tokens can't target another user's personal repos,
      so: a classic token with only `public_repo`, or move the repos to an organization and use a
      fine-grained one (Contents rw on both; Pull requests + Issues rw on sc-brrr).
- [ ] Put it in `sc-brrr-runner.yaml` (`github-token`), set `comment: true` and `public-url`.
- [ ] One more submission PR: the bot comments with the live-log link, then edits the comment with
      the outcome.

## 3. Running it for real on the public machine

- [ ] A user for the runner, rootless podman with CDI for the GPU, the conda cache warm.
- [ ] systemd unit: `-push -watch 5m -listen 127.0.0.1:8080`, `Restart=on-failure`.
- [ ] Reverse proxy with TLS in front of the live page; `public-url` set to it.
- [ ] Check the host id on that machine (it differs from the laptop's `50f1ae8f`), and that the
      scoreboard's host filter shows both.

## Later (README roadmap)

- Contributors run the runner locally (`try`), with the dataset fetched and verified by hash.
- GPU smoke runs on labelled PRs, pulled by the runner, with a commit status on the PR.
- Re-score everything in `submissions/` when the plan hash changes.
- A GitHub App instead of the machine account; a check run next to the comment.
- Serve the run's detailed `runner.log` (redacted) next to the summary log.
- Results keyed by input content, not the size label (sc-brrr `docs/infrastructure.typ`, caveats).
- The fidelity gate (kNN purity, edge Jaccard, ARI vs the reference): until then the scoreboard
  ranks speed without checking correctness.
