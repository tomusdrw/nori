# Deploy Bot

Self-hosted deployment control panel for Docker-based services. Watches GHCR for new
images, runs your bash deploy scripts, and provides a dashboard to manage services.

## Requirements

- Docker socket access (`/var/run/docker.sock`)
- Go 1.25+ (for building from source)
- Bash and tmux (for the browser terminal)

## Quick start

For a local binary, set the application secrets yourself:

```bash
# Generate secrets
export DEPLOYBOT_KEY=$(head -c 32 /dev/urandom | base64)
export DEPLOYBOT_SESSION_KEY=$(head -c 32 /dev/urandom | base64)
export DEPLOYBOT_ADMIN_HASH=$(go run ./cmd/deploybot hash-password 'your-password')

make build
./bin/deploybot
```

Open http://localhost:8080 and log in with your password.

## Docker (recommended)

The published image talks to the **host Docker daemon** via a mounted socket. This is
intentional — deploybot orchestrates containers on the host by running your bash scripts
(which call `docker`). The socket mount makes deploybot root-equivalent on that host, so
keep auth enabled and put Cloudflare Access (or similar) in front.

### Pull from GHCR

```bash
docker pull ghcr.io/tomusdrw/github-deploy-bot:latest
```

### docker compose

```bash
export DEPLOYBOT_IMAGE=ghcr.io/tomusdrw/github-deploy-bot:latest
docker compose run --rm -it launcher up \
  --image "$DEPLOYBOT_IMAGE" \
  --port "${DEPLOYBOT_PORT:-8080}:8080"
```

The first run asks for an admin password, generates the encryption and session
keys, writes them to the `deploybot-config` volume, and creates the long-running
`deploybot` container. Later `up` invocations read that saved configuration and
recreate the container without prompting or changing any secrets.

For a scripted first boot, generate the admin password hash without a local Go
install and pass it to `up`:

```bash
export DEPLOYBOT_ADMIN_HASH=$(docker run --rm ghcr.io/tomusdrw/github-deploy-bot:latest \
  hash-password 'your-password')
docker compose run --rm launcher up \
  --image "$DEPLOYBOT_IMAGE" \
  --port "${DEPLOYBOT_PORT:-8080}:8080" \
  --admin-password-hash "$DEPLOYBOT_ADMIN_HASH"
```

### docker run

```bash
docker run --rm -it \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v deploybot-config:/config \
  ghcr.io/tomusdrw/github-deploy-bot:latest \
  up --image ghcr.io/tomusdrw/github-deploy-bot:latest
```

Use `--data-volume`, `--config-volume`, `--container-name`, or repeat `--port`
on first boot to change the defaults. The launcher stores a human-editable
`run.json` and `deploybot.env` on the config volume; `deploybot.env` contains
plaintext secrets, so it has the same sensitive trust boundary as `docker.sock`.

The image ships `bash`, `tmux`, and the `docker` CLI so your deploy scripts,
launcher, and browser terminal can call Docker against the host daemon through
the mounted socket.

### Existing installations

An existing container started directly with `docker run` has no launcher config
volume and therefore cannot safely self-update yet. Re-bootstrap through `up`
once, passing the old `DEPLOYBOT_KEY`, `DEPLOYBOT_SESSION_KEY`, and
`DEPLOYBOT_ADMIN_HASH` as `--key`, `--session-key`, and
`--admin-password-hash` on that first run if you need to keep the existing
encrypted service environment values. The migration is deliberate; the launcher
never guesses a container's run configuration.

## Browser terminal

The **Terminal** link opens an interactive Bash shell in the app container. It is backed
by one named tmux session, so closing the tab or losing the WebSocket connection only
detaches the browser: commands keep running and the next connection reattaches to the
same shell. Type `exit` when you intentionally want to end the session. Restarting or
replacing the container ends the shell process, while files written under `/data` remain
on the persistent volume.

The shell starts in `DEPLOYBOT_TERMINAL_DIR` (`.` by default and `/data` in the supplied
Docker image). Reverse proxies must support WebSocket upgrades for `/terminal/ws`.

The terminal is protected by the same admin session as the rest of the app and rejects
cross-origin WebSocket connections. It can run arbitrary commands with the app's
permissions, including use of the mounted Docker socket, so treat terminal access as
root access to the Docker host.

## Per-service configuration

Each service is configured with a watched image, one complete `.env` document, and a
Bash deployment script. The form uses code editors with syntax highlighting, line
numbers, and live validation. Invalid dotenv or Bash syntax cannot be saved, and Bash
is checked again immediately before every deployment.

The `.env` document is kept exactly as entered, including comments and blank lines. The
complete document is encrypted at rest in SQLite because any value may be sensitive.

## Per-service contract

Each service requires two declarations in your deploy script:

1. **Watched image** — configured in the UI (e.g. `ghcr.io/you/app:latest`). This image's
   digest drives "update available" and auto-deploy.
2. **Container label** — add `--label deploybot.service=$SERVICE` to every `docker run`.
   The app injects `$SERVICE` and `$TARGET_DIGEST` into your script's environment.

Example deploy script snippet:

```bash
docker pull ghcr.io/you/app:latest
# ... backup steps ...
docker stop myapp || true && docker rm myapp || true
docker run -d --name myapp \
  --label deploybot.service=$SERVICE \
  ghcr.io/you/app:latest
```

## Auto-deploy policies

| Policy | Behavior |
|--------|----------|
| `manual` | Deploy only via dashboard button |
| `immediate` | Auto-deploy when registry digest changes (polled every 60s) |
| `scheduled` | Deploy on cron schedule (e.g. `0 3 * * *`) |

## Environment variables

| Variable | Required | Description |
|----------|----------|-------------|
| `DEPLOYBOT_KEY` | yes | Base64-encoded 32-byte AES key for encrypting secret env vars |
| `DEPLOYBOT_SESSION_KEY` | yes | Base64-encoded 32+ byte key for signing session cookies |
| `DEPLOYBOT_ADMIN_HASH` | yes | Bcrypt hash of admin password (`deploybot hash-password`) |
| `DEPLOYBOT_DB` | no | SQLite path (default: `deploybot.db`) |
| `DEPLOYBOT_LISTEN` | no | Listen address (default: `:8080`) |
| `DEPLOYBOT_DOCKER_HOST` | no | Docker host override |
| `DEPLOYBOT_TERMINAL_DIR` | no | Initial terminal directory (default: current directory; Docker image: `/data`) |
| `DEPLOYBOT_POLL_INTERVAL` | no | Registry poll interval (default: `60s`) |

When started by the launcher, `DEPLOYBOT_KEY`, `DEPLOYBOT_SESSION_KEY`, and
`DEPLOYBOT_ADMIN_HASH` are generated once and read from `/config/deploybot.env`.
The launcher also sets `DEPLOYBOT_CONFIG_VOLUME`, `DEPLOYBOT_SELF_CONTAINER`,
and `DEPLOYBOT_SELF_IMAGE`; do not set only some of these manually.

## Self-updates

Launcher-managed installations automatically add a protected **deploybot**
service to the dashboard. Its policy defaults to manual, so a newly published
image appears as an update that you deploy while watching. The normal deploy
history is used: after the handoff starts, the row remains `running` until the
new instance starts and verifies its own digest.

There is intentionally no health check or automatic rollback. If a self-update
leaves deploybot unavailable, run a detached launcher manually with the saved
config volume, for example:

```bash
docker run --rm \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v deploybot-config:/config \
  ghcr.io/tomusdrw/github-deploy-bot:latest rollback
```

## Security

This app mounts `docker.sock`, making it **root-equivalent on the host**. Requirements:

- Always use authentication (enabled by default)
- Put **Cloudflare Access** (or similar) in front as a second layer
- Never expose without TLS in production
- Treat browser terminal access as unrestricted administrator access to the host

## CI recommendation

Stamp images with a readable version for the dashboard:

```yaml
- run: docker build -t ghcr.io/${{ github.repository }}:${{ github.sha }} .
- run: docker tag ... :latest
```

Or set the OCI label `org.opencontainers.image.version`.

## Commands

```bash
deploybot                    # start server
deploybot hash-password PWD  # generate bcrypt hash for DEPLOYBOT_ADMIN_HASH
deploybot seed-demo          # insert a demo service row
deploybot up --image IMAGE   # bootstrap/recreate from launcher config
deploybot update --target-digest sha256:...  # swap to an image digest
deploybot rollback            # swap to the previous recorded digest
```

The browser editor and terminal bundles are committed, so building the Go binary does
not require a JavaScript toolchain. If you change `internal/web/editor.js` or
`internal/web/terminal.js`, rebuild them with Bun:

```bash
make assets
```
