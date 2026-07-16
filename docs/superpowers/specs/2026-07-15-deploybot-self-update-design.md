# Nori — Self-Update Design

**Date:** 2026-07-15
**Status:** Approved design, pending implementation plan
**Supersedes:** the "The bot does not manage its own lifecycle" non-goal in
`2026-07-15-github-deploy-bot-design.md`.

## Problem

deploybot deploys other services by watching a GHCR image, and on a new digest running a
bash script that pulls the image and swaps the container(s). It cannot do this to itself:
a normal deploy script runs `docker stop deploybot && docker rm deploybot && docker run …`
**from inside the deploybot container**, so `docker stop deploybot` kills the very process
running the script, mid-line. The new container never starts. This can't be dodged with
`nohup`/`setsid`/`disown`, because `docker stop deploybot` tears down the whole container's
PID namespace — any detached child inside it dies too.

The goal: let deploybot update itself using the **same trigger and history mechanism** it
uses for every other service (poller / scheduler / manual button → executor → deployment
record), without the swap killing itself mid-flight.

## Core idea

The swap must be performed by a process running **outside** the deploybot container. We
introduce a one-shot **launcher**: the deploybot image invoked with `up` / `update`
subcommands. The launcher **owns deploybot's run config** and is what creates the deploybot
container in the first place. Because the launcher created deploybot, the "update" path
recreates it from the *same canonical config* — there is no `docker inspect` reconstruction
anywhere, and first-boot and update share one code path so they cannot drift.

The launcher is **one-shot**: it runs, does its job (`up` = create, `update` = swap), and
exits. There is no long-running supervisor and no IPC channel — deploybot is alive at the
moment it launches the updater, so it hands off directly. Docker's own `--restart` policy
handles crash-restarts.

## Decisions (locked during brainstorming)

1. **Failure safety: minimal.** No health check, no auto-rollback. A bad image leaves
   deploybot down; recovery is manual.
2. **Launcher is one-shot**, not a resident supervisor.
3. **Launcher is the deploybot image + subcommands** (`up`, `update`), not a second image —
   one artifact, launcher improves in lockstep with the app. The image already ships `bash`
   + the `docker` CLI.
4. **Config lives in a bespoke file on a shared volume** (`deploybot-config` at `/config`),
   the canonical source of truth. Not docker-compose (would force the compose plugin into
   the image and exclude `docker run` users, and can't cleanly prompt on first boot).
5. **Deploy-record finalization: reconcile on next startup.** The self-deploy row is left
   `running`; the newly-booted instance resolves it to `success`/`failed`.
6. **Auto-seed a self-service** visible in the dashboard, with a **managed** deploy script,
   default **policy = manual**.

## Components

### 1. Launcher subcommands (`cmd/deploybot`)

`main.go` already dispatches subcommands off `os.Args[1]` (`hash-password`, `seed-demo`).
Add:

- **`up`** — bootstrap (first boot) then create/recreate the deploybot container from config.
- **`update`** — swap the running deploybot container to a target digest.
- **`rollback`** *(optional, YAGNI-cuttable)* — recreate on the previous digest saved in config.

A new `internal/launcher` package holds config I/O, bootstrap, and the create/swap logic.
The launcher **shells out to the `docker` CLI** (already in the image) so the run-spec maps
directly and transparently to `docker run` flags, consistent with the bash-script ethos of
the rest of the app.

### 2. Launcher config (shared volume)

`deploybot-config` volume, mounted at `/config`. Single source of truth, human-editable,
holds two concerns:

- **Run spec:** image ref (tag, e.g. `ghcr.io/tomusdrw/nori:latest`), container
  name, port mappings, volume mounts (data volume, docker socket, config volume), labels
  (incl. `deploybot.service=<self-service-name>`), restart policy.
- **App env:** `DEPLOYBOT_KEY`, `DEPLOYBOT_SESSION_KEY`, `DEPLOYBOT_ADMIN_HASH`, plus any
  user-supplied env, plus self-identity vars (below).
- **`previous_image_digest`:** written after each successful handoff, for manual rollback.

Format: an env file for the app env (matches the user's "auto-generate the env" mental
model) plus a small structured run-spec (JSON, zero-dep in Go), both under `/config`. Exact
layout is a plan-phase detail. **Secrets are stored in plaintext** here — the same trust
boundary as the docker socket the launcher already mounts. This is documented, not hidden.

### 3. First boot — `up` with no config present

1. Generate `DEPLOYBOT_KEY` and `DEPLOYBOT_SESSION_KEY` (32 random bytes, base64).
2. **Prompt for the admin password** over the TTY, bcrypt-hash it via the existing
   `auth.HashPassword` → `DEPLOYBOT_ADMIN_HASH`. Non-interactive escape hatch
   (`--admin-password-hash <hash>` flag or env var) for scripted installs.
3. Write the config (run spec + env) to `/config`.
4. Create the deploybot container from the spec, injecting the self-identity env
   (`DEPLOYBOT_CONFIG_VOLUME`, `DEPLOYBOT_SELF_CONTAINER`, `DEPLOYBOT_SELF_IMAGE`) and the
   `--label deploybot.service=<self>` label.

**Idempotent:** if config already exists, skip steps 1–3 entirely and just (re)create from
it. This rule is what guarantees `DEPLOYBOT_KEY` is generated exactly once — regenerating it
would make every encrypted env value in the SQLite DB undecryptable.

The launcher must be told the config volume's **external name** (flag/env) so it can
propagate it to deploybot as `DEPLOYBOT_CONFIG_VOLUME`; deploybot needs the name (not a
mount) to spawn the updater with the volume attached.

### 4. Self-service seeding (deploybot boot)

When deploybot starts and detects the launcher-managed env vars, it **idempotently ensures a
self-service row** exists:

- name: reserved (e.g. `deploybot`), watched image = `DEPLOYBOT_SELF_IMAGE`,
- **policy = manual** (default; user-changeable in the UI),
- deploy script = the **managed handoff** (section 5), rendered read-only in the UI,
- a **self flag** distinguishing it (new `is_self` column via a schema migration).

It then renders in the dashboard like any service and shows "update available" when its
running digest ≠ latest.

**Default policy rationale:** immediate + minimal-safety + "this is the control panel itself"
stacks three risks on the one component that is unrecoverable from the browser. Manual keeps
one-click self-update while preventing an unattended bad push from taking the panel down.

### 5. The handoff — managed self-deploy script

The self-service's managed deploy script launches a detached updater and returns at once:

```bash
docker run --rm -d \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v $DEPLOYBOT_CONFIG_VOLUME:/config \
  $DEPLOYBOT_SELF_IMAGE@$TARGET_DIGEST \
  update --target-digest $TARGET_DIGEST
```

`$SERVICE` and `$TARGET_DIGEST` are injected by the executor for every service today. The
self-identity vars `$DEPLOYBOT_SELF_IMAGE` and `$DEPLOYBOT_CONFIG_VOLUME` are **not** in the
script environment by default: `OSRunner.Run` sets `cmd.Env` without inheriting deploybot's
process env (`executor.go:42`). So the executor, when building the **self-service's** script
env, must read these two values from deploybot's own process env (set by the launcher at
boot) and inject them into the script environment.

The **executor special-cases the self-service**: after this handoff script exits 0, it does
**not** finalize the deployment — it leaves the row `running`, because the actual swap has
not happened yet. (Every other service still finalizes normally.) If the handoff itself fails
(exit ≠ 0), the normal `failed` path applies, since deploybot is still alive to record it.

### 6. `update` — inside the detached updater container

1. Read config from `/config`.
2. `docker pull <image>@<target-digest>`.
3. `docker stop <name>` + `docker rm <name>` (the old deploybot; safe — separate container).
4. `docker run` the new deploybot from the config's run spec, **pinned to the target digest**,
   with the same labels / mounts / env.
5. Write `previous_image_digest` into config (for rollback).
6. Exit (container is `--rm`).

### 7. Reconciliation on next boot

On startup, deploybot:

1. Reads its own now-running digest (via the `deploybot.service=<self>` label, the same way
   the dashboard reads any service's running digest).
2. Finds self-service deployments still in `running` status.
3. Marks each `success` if its `target_digest` equals the running digest, else `failed`.

This is the only truthful completion signal and, by nature of self-update, it lands **after**
the restart completes, not during. Accepted tradeoff.

## Data flow (update)

```
poller / scheduler / manual button
  → executor.Deploy(self-service)
    → managed handoff script: docker run -d … update --target-digest <D>   (returns 0)
    → executor leaves deployment row = running (special-case: no finalize)
  → detached updater: pull <image>@<D>, stop+rm old deploybot, run new deploybot
  → old deploybot process dies during stop; new deploybot boots on <D>
    → boot reconciliation: running digest == <D> ? mark success : mark failed
  → dashboard shows the resolved self-deploy in history
```

## Failure & recovery (minimal, as chosen)

- No health check, no auto-rollback.
- A broken new image → deploybot is down. Recover by hand:
  `docker run --rm -v …/docker.sock -v deploybot-config:/config <image>@<previous-digest> update --target-digest <previous-digest>`,
  or the optional `rollback` verb which reads `previous_image_digest` from config.
- Docker's `--restart` policy covers process crashes, **not** a bad image.
- The launcher-owns-config model makes even manual recovery a one-liner, which is why the
  chosen minimal safety is tolerable for a single-instance self-hosted tool.

## Changes to existing code

- **`cmd/deploybot/main.go`** — dispatch `up` / `update` (/ `rollback`) alongside the
  existing subcommands.
- **`internal/launcher`** (new) — config read/write, first-boot bootstrap (key gen, password
  prompt, hash), container create + swap via the `docker` CLI.
- **`internal/executor`** — recognize the self-service and skip finalization after a
  successful handoff.
- **`internal/store`** — `is_self` column + schema migration; a seed helper for the reserved
  self-service.
- **Boot wiring (`main.go`)** — in launcher-managed mode, seed the self-service and run the
  dangling-self-deploy reconciliation before starting the poller/scheduler/web.
- **`internal/web`** — mark the self-service as managed (read-only deploy script, a badge).
- **`README.md` / `docker-compose.yml`** — the documented front door becomes the launcher
  (`up`).

## Testing strategy

- **Launcher config** — write/read round-trip; run-spec → `docker run` flag rendering.
- **Bootstrap** — key generation (length, base64), password hashing, with a fake prompt and
  the non-interactive path; idempotency (existing config is never regenerated).
- **Executor** — self-service handoff leaves the deployment `running` (no finalize); a failed
  handoff still records `failed`.
- **Reconciliation** — running == target → `success`, else `failed`, driven by a fake docker.
- **`update` swap** — via a fake command runner / fake docker: pull → stop → rm → run in
  order, pinned to the target digest.

## Migration note

Existing installs started via plain `docker run` have **no launcher and no config volume**.
Adopting self-update requires re-bootstrapping once through the launcher (`up`), which will
generate fresh `DEPLOYBOT_KEY`/`DEPLOYBOT_SESSION_KEY` unless the operator seeds the config
with their existing values first (to preserve already-encrypted DB secrets). This is not
automatic and must be documented in the README.

## Explicitly out of scope

- Health checks and automatic rollback (chosen: minimal safety).
- A resident supervisor / long-running launcher.
- docker-compose as the config source.
- Multi-instance / clustered self-update.
