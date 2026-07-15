# GitHub Deploy Bot — Design

**Date:** 2026-07-15
**Status:** Approved design, pending implementation plan

## Problem

Projects live on GitHub, are built by CI, and are published as Docker images to the
public GitHub Container Registry (GHCR). A single server runs everything as Docker
containers. For each project there is an env file plus a bash script that: pulls the
latest image, backs up the database/assets, stops the old container(s), and starts new
one(s). Today this is triggered by hand — SSH in, run the script.

The goal is a **self-hosted deployment control panel** that automates and manages this:
one small app that watches for new images, runs the existing deploy scripts, and gives a
dashboard to see and control every service.

## Goals

1. Move each service's env vars and bash deploy script **into the app** (SQLite, with backups).
2. A **dashboard** of all services: running version, running/stopped status, latest
   available version, and actions.
3. Inspect a service's container **logs** (`docker logs`) from the UI.
4. **Manual** deploy, plus start/stop of a service.
5. **Configurable auto-deploy** per service: immediate (on new build), manual only, or scheduled.

## Non-goals (v1)

- **The bot does not manage its own lifecycle.** It deploys *other* services; it cannot
  safely stop/replace itself mid-deploy (chicken-and-egg). It runs as a plain
  container/systemd unit, updated manually or by a tiny separate script.
- **No Kubernetes, no clustering.** Single server, single instance.
- **No multi-user / RBAC.** One admin.

## Approaches considered

- **Off-the-shelf (Coolify / Dokploy / Portainer / Watchtower):** cover most of the
  dashboard/env/logs/deploy surface, but fight three specific requirements — the
  host-side *backup-before-swap* step, storing *existing bash scripts*, and
  *scheduled* deploys. Watchtower's lifecycle hooks run inside the container, not as host
  bash, so the backup step has no clean home. Building a focused tool is justified by
  these gaps and the desire for exact fit and ownership.
- **Push (webhook) vs pull (poll):** the server already has public ingress (Cloudflare),
  so a webhook is viable — but pull-based polling is simpler, self-healing (no
  missed-event class of bugs), needs no GitHub-side config, and is auth-free because the
  packages are public. **Chosen: polling**, embedded in the app (it also powers the
  dashboard's "latest version"). A webhook "check now" poke can be added later as an
  optimization; the poller remains the backstop.

## Service model & contract

A **service** is not one container — it is a small multi-container stack (e.g. app + db)
that the user's **bash script** orchestrates. Execution and introspection are decoupled:
the script owns *how the stack comes up*; the app reads *what is running* directly from
Docker.

"Less structured" still requires exactly **two declarations per service** so the app can
power the dashboard; everything else stays in the opaque bash script:

1. **Watched image** — e.g. `ghcr.io/user/app:latest`. A multi-container service has
   several images; the app must be told which one's new digest means "a new version
   shipped" (drives "update available" and immediate auto-deploy).
2. **Container-grouping label** — the app injects `$SERVICE` into the script's
   environment, and the script adds `--label deploybot.service=$SERVICE` to each
   `docker run`. The dashboard finds a service's containers by this label for
   status / logs / start / stop.

Everything else — backup logic, ordering, the multi-container dance — stays as bash the
app executes verbatim and streams to the log view.

### Version & status semantics

- **Running version:** digest from `docker inspect` of the service's containers. A
  human-readable version is shown if the image carries one (`:{git-sha}` tag or an OCI
  `org.opencontainers.image.version` label). *Recommendation: have CI stamp images so the
  dashboard shows `v1.4.2` rather than `sha256:9f3c…`.*
- **Latest version:** registry digest for the watched image's tag.
- **Update available:** running digest ≠ latest digest.

### Start / stop / redeploy semantics (multi-container)

- **Stop:** stop all containers carrying the service label.
- **Start:** start those containers again.
- **Redeploy:** re-run the bash script (which recreates them).

## Architecture

One Go binary running as its own container with `docker.sock` mounted. It is
simultaneously a web server, registry poller, scheduler, and deploy executor, backed by
SQLite. Built around clean module boundaries **behind interfaces**, so each piece is
understood and tested in isolation and the Docker/registry surfaces can be faked.

### Modules (each has one job)

- **`store`** — SQLite persistence. Repositories for services, deployments (history), and
  settings. Owns schema/migrations. **Encrypts secret env values at rest** with a key
  from app config (never stored in the DB). Hides that the backend is SQLite.
- **`docker`** — wraps the official Docker SDK: `ListContainers(serviceLabel)`, `Logs`,
  `Start`/`Stop(serviceLabel)`, `RunningDigest(...)`. The introspection + control surface.
- **`registry`** — "is there a newer image?" via `go-containerregistry`
  (`crane.Digest(ref)`), auth-free (public packages). Exposes `LatestDigest(imageRef)`.
- **`executor`** — the deploy engine. Assembles env (decrypts secrets, injects `$SERVICE`
  and the target digest), runs the bash script via `os/exec`, streams stdout/stderr to
  the deployment log, and records status. Holds a **per-service lock** (a service never
  deploys twice concurrently) and a **failure cooldown** (a broken image does not flap).
- **`poller`** — background ticker. For each service, fetch the latest digest → update the
  dashboard's cached "latest" and, for `immediate` services, trigger the executor when it
  differs from what is running. Self-healing: a missed tick is caught by the next.
- **`scheduler`** — `robfig/cron`; fires deploys for `scheduled` services at their
  configured time (deploying the latest available digest).
- **`web`** — `chi` router + **templ** components + **htmx**. Handlers call the modules
  above. **SSE** for live log tailing and status refresh. Auth + CSRF middleware.
- **`auth`** — single admin login (hashed password, session cookie); CSRF on mutating
  requests. Assumes Cloudflare Access in front as a second layer.
- **`main`** — wiring; starts web + poller + scheduler.

### View layer

**templ** (`github.com/a-h/templ`): typed, component-based views compiled to Go — no
HTML string-gluing, compile-time checked. Requires a `templ generate` codegen step.
Views: dashboard (service cards), service detail (env editor, script editor, deploy
history, live logs), and a streaming deploy-log pane.

## Data model (SQLite)

- **service**: id, name, watched_image_ref, auto_deploy_policy
  (`immediate` | `manual` | `scheduled`), cron_expr (nullable), deploy_script,
  created_at, updated_at.
- **env_var**: id, service_id, key, value, is_secret (secret values encrypted at rest).
- **deployment**: id, service_id, trigger (`auto` | `manual` | `scheduled`),
  target_digest, status (`pending` | `running` | `success` | `failed`), started_at,
  finished_at, log (text). Powers history + live view.

Running status/version are **never stored** — always read live from Docker.

## Key flows

- **Auto (immediate):** poller sees a new digest → `executor.Deploy` → the bash script
  recreates containers → dashboard reflects the new running digest.
- **Manual / scheduled:** dashboard button or cron → the same `executor.Deploy`; live log
  streamed over SSE.
- **Dashboard render:** per service, `docker.ListContainers(label)` for status + running
  digest, joined with the cached latest digest → templ cards. htmx polls every few
  seconds to refresh; "update available" = running digest ≠ latest.
- **Logs:** service detail streams `docker.Logs(container)` over SSE into a log pane.

## Security

The app talks to `docker.sock`, which makes it **root-equivalent on the host that runs
every project**, and it sits behind a public URL. Therefore:

- **Authentication is mandatory** — real login, not optional.
- **Cloudflare Access** in front is strongly recommended as a second layer.
- **Secret env values are encrypted at rest**, with the key supplied via app config/env,
  never stored in the DB.
- **CSRF protection** on all state-changing (htmx) requests.
- The deploy script runs as the bot's user with Docker access — inherent and documented.

## Tech stack

- **Language:** Go (single static binary, first-class Docker SDK).
- **Router:** `chi`.
- **Views:** `templ` + `htmx` (+ a little Alpine.js where needed); **SSE** for live logs/status.
- **Storage:** SQLite (file-based; backups = copy/export the file).
- **Registry:** `go-containerregistry` (`crane`).
- **Docker:** official Go client (`github.com/docker/docker/client`).
- **Scheduling:** `robfig/cron`.
- **Crypto:** AES-GCM (or `nacl/secretbox`) for secret env values.

## Testing strategy

- `docker`, `registry`, `store`, and `executor` sit behind interfaces → unit-testable
  with fakes/mocks.
- `executor`: test env assembly and status transitions with a fake command runner.
- Optional integration test (behind a build tag) that exercises the real Docker client
  against a throwaway container.

## Future (explicitly deferred)

- Webhook "check now" endpoint to cut auto-deploy latency from minutes to seconds, with
  the poller kept as the self-healing backstop.
- Notifications (Slack/email) on deploy failure.
- Rollback to a previous digest as a first-class action.
