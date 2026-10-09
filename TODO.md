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

Running on roland (x86-nvidia: L4, EPYC 7763, Ubuntu 24.04) since 2026-10-09, host id `c931f405`; first
entry `exact-ref-0.1.5` scored there end to end. What the setup took, for the next machine:

- `/etc/subuid` and `/etc/subgid` ranges for the user (root; without them images don't unpack).
- podman 5 from nix (`~/.nix-profile/bin`), `~/.config/containers/policy.json` (default accept).
- GPU: `nvidia-ctk` from nix ships no `nvidia-cdi-hook`; a wrapper `~/.local/bin/nvidia-cdi-hook`
  (`exec <nix>/nvidia-ctk hook "$@"`), the spec generated with `--nvidia-cdi-hook-path` into
  `~/.config/cdi`, and `cdi_spec_dirs` in `~/.config/containers/containers.conf`. Redo both after a
  nix upgrade of the toolkit (the wrapper names a store path).
- pixi matching the lock file (0.71.2, in `~/.pixi/bin`, first on PATH): an older pixi rewrites
  `pixi.lock`, the checkout turns dirty and the runner refuses to run.
- uv (`~/.local/bin`), the base image built there (`podman build --build-arg OB=…@4e3e526…`),
  the runner cross-compiled (`CGO_ENABLED=0 GOOS=linux`), config copied over ssh, `chmod 600`.
- Started with `setsid nohup`; PATH = `~/.pixi/bin:~/.nix-profile/bin:~/.local/bin:$PATH`.


- [x] Rootless podman with CDI for the GPU, the conda cache warm (roland, as `ben`; a dedicated user later).
- [ ] systemd unit: `-push -watch 5m -listen 127.0.0.1:8080`, `Restart=on-failure`.
- [ ] Reverse proxy with TLS in front of the live page; `public-url` set to it.
- [x] Host id `c931f405`, next to the laptop's `50f1ae8f` on the scoreboard.

## Later (README roadmap)

- Contributors run the runner locally (`try`), with the dataset fetched and verified by hash.
- GPU smoke runs on labelled PRs, pulled by the runner, with a commit status on the PR.
- Re-score everything in `submissions/` when the plan hash changes.
- Baselines on freshly resolved environments, daily, to see library-update trends (not urgent):
  https://github.com/btraven00/sc-brrr/issues/4
- A GitHub App instead of the machine account; a check run next to the comment.
- Serve the run's detailed `runner.log` (redacted) next to the summary log.
- Results keyed by input content, not the size label (sc-brrr `docs/design.typ`, caveats).
- The fidelity gate (kNN purity, edge Jaccard, ARI vs the reference): until then the scoreboard
  ranks speed without checking correctness.
