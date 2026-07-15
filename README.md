# Deploy Bot

Self-hosted deployment control panel for Docker-based services. Watches GHCR for new
images, runs your bash deploy scripts, and provides a dashboard to manage services.

## Requirements

- Docker socket access (`/var/run/docker.sock`)
- Go 1.23+ (for building from source)

## Quick start

```bash
# Generate secrets
export DEPLOYBOT_KEY=$(head -c 32 /dev/urandom | base64)
export DEPLOYBOT_SESSION_KEY=$(head -c 32 /dev/urandom | base64)
export DEPLOYBOT_ADMIN_HASH=$(go run ./cmd/deploybot hash-password 'your-password')

make build
./bin/deploybot
```

Open http://localhost:8080 and log in with your password.

## Docker

```bash
docker build -t deploybot .
docker run -d \
  -p 8080:8080 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v deploybot-data:/data \
  -e DEPLOYBOT_KEY \
  -e DEPLOYBOT_SESSION_KEY \
  -e DEPLOYBOT_ADMIN_HASH \
  deploybot
```

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
| `DEPLOYBOT_POLL_INTERVAL` | no | Registry poll interval (default: `60s`) |

## Security

This app mounts `docker.sock`, making it **root-equivalent on the host**. Requirements:

- Always use authentication (enabled by default)
- Put **Cloudflare Access** (or similar) in front as a second layer
- Never expose without TLS in production

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
```
