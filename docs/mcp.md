# MCP maintenance notes

Nori owns its authorization server because this feature must work with the
existing administrator login, without an external identity provider. The
official Go MCP SDK owns protocol parsing, tool schemas and Streamable HTTP.
Keep those responsibilities separate. The MCP transport is stateless: every
HTTP request passes through bearer-token validation, including requests made
on an already initialized client connection.

## Code map

| Area | Entry points |
|------|--------------|
| Public OAuth routes, metadata and rate limits | `internal/mcpauth/server.go` |
| Registration and public/confidential client authentication | `internal/mcpauth/clients.go` |
| Admin consent, exact redirects and PKCE challenges | `internal/mcpauth/authorization.go`, embedded `consent.html` |
| Code exchange, refresh rotation and token-family revocation | `internal/mcpauth/tokens.go` |
| Bearer validation and per-tool scope checks | `internal/mcpauth/resource.go` |
| Credential consumption and revocation markers | `internal/store/mcp_oauth.go` |
| Instance toggle, public origin and revocation epoch | `internal/store/mcp_settings.go` |
| Tools and input validation | `internal/web/mcp.go` |
| Service backup tools | `internal/web/mcp_backup.go` |
| Backup storage, disks, and recovery-key tools | `internal/web/mcp_backup_admin.go` |
| Archive transfer and restore tools | `internal/web/mcp_backup_recovery.go` |
| Resource measurements, resource settings, and configuration revisions | `internal/web/mcp_diagnostics.go` |
| Atomic service saves and conflict detection | `internal/store/service_config.go` |
| Bounded deployment-log reads | `internal/store/deployment_read.go` |
| Live scheduled-service configuration | `internal/scheduler/scheduler.go`, `internal/store/schedule.go` |

## Security boundaries to preserve

- The configured public origin supplies issuer, resource and allowed Host.
  Never derive these from arbitrary forwarding headers. HTTP is allowed only
  for loopback development. Proxy TLS termination is supported by the secure
  cookie context set in `web.Server.mcpSecureCookies`.
- Browser sessions authorize consent, not MCP requests. Consent requires an
  authenticated admin session, CSRF token, matching Origin and explicit POST.
  GET navigation from OAuth clients may have an external, loopback, absent or
  `null` Origin; it opens login or renders consent and never grants access.
  The consent GET uses `Referrer-Policy: same-origin`: `no-referrer` suppresses
  the Origin on browser form POSTs and breaks approval with `invalid_origin`.
  Do not add CORS read access to the consent page. Keep the template escaped,
  its parameter whitelist, and its CSP/frame restrictions when changing it.
- Codes bind client, exact registered redirect, S256 challenge, resource,
  scope and current epoch. Tokens bind client, resource, scope and epoch.
  Refresh may narrow scopes but cannot add permissions or extend the original
  grant deadline.
- `ConsumeOAuth` is a conditional database write, not a read followed by an
  unconditional update. It must remain single-use across separate Store and
  Server instances. A process-local mutex is insufficient.
- Replay revokes the whole token family. Its persisted revocation marker also
  prevents an exchange already in flight from inserting new usable tokens.
  Keep that marker longer than the maximum grant lifetime: currently 31 days
  versus 30 days. Never delete used refresh records early; they detect replay.
- Settings receives a dedicated, safe projection of approved client
  registrations and grants. It must never receive generic OAuth records: they
  contain credential hashes and protocol-private metadata. A one-grant revoke
  tombstones only that token family and preserves the client registration and
  every other family. Safe grant projections are retained through the same
  post-expiry revocation window so a recent revoked connection is not shown as
  active after a restart. Their last-use timestamp advances monotonically only
  after successful bearer authentication at the MCP resource boundary; token
  issuance, refresh, and rejected credentials do not count as use. If the
  timestamp cannot be persisted, the request is rejected before MCP dispatch so
  the inventory never silently misses accepted activity.
- Credentials are hashed before they reach storage. Do not log credentials,
  authorization query strings, deploy scripts or environment contents.
- Disabling MCP, changing its public URL or revoking all access changes the
  epoch and removes registrations and grants in one transaction. An ordinary
  settings save must never restore an old epoch. A request already admitted
  may finish, and a deployment already launched continues running.
- All tools go through `addNoriTool` and its scope check before side effects.
  `WithIdentity` is for trusted internal callers/tests; it must not replace
  `Protect` on an HTTP route. Environment reads return only
  variable names and fixed placeholders. `nori:secrets` permits only setting a
  declared variable or applying a saved environment revision, together with
  `nori:write`. Environment values are never returned.
- Write access can run arbitrary Bash with Nori's permissions and Docker
  socket. It can read host secrets and affect Nori itself. The explicit
  self-service guards prevent accidental edits through service tools; they
  are not a sandbox against a client granted write access. Known current dotenv values are
  redacted from MCP output, including scripts and logs. Unknown, encoded and
  historical credentials are outside that redaction guarantee.
- Logs must be bounded at the read boundary, not only after loading the full
  output. Container logs must belong to the selected service; preserve byte,
  tail and timeout limits and Docker multiplex-frame validation. Reads include
  at most one maximum-value-length of extra context to redact secrets crossing
  the 256 KiB output window. Redaction happens before the final output cap.
- The MCP wrapper snapshots dotenv values before each tool operation, including
  legacy and self-service values, refreshes after reads, and redacts decoded
  response strings with both snapshots. A longer newly written value makes a
  truncated log read fail closed if its captured context is insufficient.
  Multiline values also redact individual nonempty lines because Docker applies
  its line tail before the server receives logs. Failure
  to load/parse the values fails closed. The environment template response is
  constructed separately from names and markers so redaction cannot corrupt it.

## Public URL setup and settings checks

On the Settings page, an empty public URL input is filled from
`window.location.origin`. This is an editable browser-side suggestion, not the
canonical issuer until the administrator saves it and server validation passes.
Existing input values are preserved. Behind a reverse proxy, open Settings at
the intended public HTTPS address or correct the suggestion before saving.
See the [user setup instructions](../README.md#mcp-agent-access).

When verifying changes to this page:

- With no configured URL, open `/settings` and check that the suggestion contains
  only the browser's scheme, host and port. MCP must remain disabled until saved.
- With a saved URL that differs from the browser's origin, confirm that the saved
  value remains in the input. Confirm that a manually edited value can be saved.
- After enabling and saving, verify that the connection notes show the saved
  URL plus `/mcp`, alongside the read/write access explanation. Check note padding
  and the separate revoke-access section at desktop and mobile widths.
- The Connected OAuth clients section lists only approved connections. Its
  rows show the original approved scopes, approval, last-use and expiry times,
  and lifecycle status; unused grants show `Never`. They must not reveal codes,
  tokens, credential hashes, client secrets, redirect URLs, or token-family
  identifiers. Check long, untrusted client names at desktop and narrow widths.
- Individual revocation starts with a confirmation page that names the selected
  client and scope. A confirmed revoke must leave sibling grants usable. It
  requires the signed administrator session, CSRF proof, a body no larger than
  16 KiB, the saved public Host, and exactly one matching non-null Origin. The
  server deliberately ignores forwarding headers. An instance without a saved
  public URL may complete first-time Settings setup with the existing session
  and CSRF protections.
- Confirm that disabling access or revoking all access requires clients to
  authorize again, as described in the form notes.

## Persistence and concurrency

Dashboard and MCP ordinary-service writes use the same validation and atomic
store operation. A service read returns `config_version`; MCP clients must send
that value as `expected_config_version` for every update, template replacement,
and secret write. The store begins a short SQLite write transaction, advances
the generation only when it matches the supplied value, and then writes service
fields, encrypted environment contents, and revision history as one commit. A
stale generation returns a fixed reload-and-retry result without a partial save;
clients must read the service again rather than retrying the old command.

The environment tools use transactional `SetEnvTemplate` / `SetEnvSecret`
operations so they cannot restore an earlier service configuration or silently
restore a stale secret value. Concurrent SQLite write conflicts fail without
partial writes; clients can retry. Templates normalize dotenv syntax and omit
comments; values are always quoted to retain leading zeros and literal dollars.
Names and managed status are immutable through MCP.

## Service descriptions

`create_service` and `update_service` accept an optional `description` string,
returned by service reads and shown in the dashboard. Agents should describe
what the service does and record deployment-specific context: environment,
domains, dependencies, persistent storage, and operational notes. Keep this
context current when changing a deployment. Descriptions are plain text, not
secret storage; never put credentials or other secrets in them.

Omitting `description` on update preserves it; an empty string clears it.
Existing services default to an empty description. Description updates use the
same configuration version checks as other fields.

## Dashboard groups

`list_services` and `get_service` return the service's current `group`.
`create_service` accepts an optional `group` string. `update_service` keeps
the current group if the field is omitted. An empty string or `Ungrouped`
assigns the default group. Any new name creates a named group when the service
is saved.

Use the group requested by the user. Otherwise, consult `list_services` and
reuse an existing group for clearly related services, such as an API and
worker in the same application. Keep ambiguous services in `Ungrouped` and
preserve existing assignments unless a move is intended.

The store commits membership alongside the service configuration. The
`service_group` table holds dashboard order and folded state independently
of membership. An absent `service_group_member` row means `Ungrouped`.

## Service backup policies

Use `get_service_backup_policy` with `service_id` and `nori:read` to read the
effective policy. The result includes detected Docker mounts and whether the
instance has S3 uploads enabled. A service can upload copies when
`s3_upload_enabled` is true and `policy.local_only` is false. Check
`list_service_backups` for `upload_status: "uploaded"` to confirm that a copy
reached S3. When `source_discovery_failed` is true, mount discovery failed
and the returned policy is still valid. A successful scan can find no mounts
before the service is deployed. Inspect the deployment template when
planning backups for managed volumes.

Use `update_service_backup_policy` with `nori:write` and the fields to change.
For example, keep 14 local copies and schedule backups for 03:00 in the
server's timezone.

```json
{
  "service_id": 42,
  "schedule": "0 3 * * *",
  "keep": 14
}
```

An empty `schedule` selects manual backups. A supplied `sources` list replaces
the current list and `[]` clears it. Each entry has a `type` and a `path`. Use
`volume` for a Docker volume name, `managed` for a logical template volume
name, and `bind` for an absolute directory. `skip_postgres` excludes the
managed PostgreSQL dump. `stop_app` stops the managed application container
during capture. Reducing `keep` can expire older local copies immediately.

The same tools read and write custom backup commands. Use `quiesce` and
`resume` to pause and resume writes, and `export` and `import` to save and
restore application data. Each pair must be either fully configured or
empty. Set both members to empty strings to remove a pair. Export writes to
`$NORI_BACKUP_WORK`, and import reads from it. Make quiesce and resume safe
to retry. Reference credentials through the service environment. MCP
responses redact known dotenv values from commands. A command containing a
redaction marker should be left out of subsequent updates unless replacing it.
The same fields are available under **Advanced backup commands** in the panel.

Omitted fields are preserved. If the policy or service configuration changes
during a write, read the policy again before retrying.

Use `create_service_backup` with `nori:write` and `service_id` to queue a backup
under the current policy. It returns the new `backup_id`. An active backup
or incomplete restore blocks a new backup. Use `list_service_backups` with
`nori:read` and the same service ID to inspect up to 100 recent backups,
their capture and upload status, and failure messages. Omit the service ID
for recent backups across the instance, including backups of deleted services.

Use `retry_service_backup_upload` with `nori:write`, `service_id`, and
`backup_id` to retry uploading an existing complete local archive. This
requires enabled S3 uploads and a policy that allows them.

### Backup storage and recovery

- `get_backup_disks` reads free space, paths, and warning and critical thresholds.
  `set_backup_disk_policy` replaces the thresholds (percentages and bytes)
  and additional paths. These tools require `nori:read` and `nori:write`,
  respectively.
- `list_remote_backups` lists the configured S3 prefix with `nori:read`.
  Pass `next_cursor` as `cursor` to get the next page.
- `get_backup_storage` and `set_backup_storage` require `nori:backup_admin`
  in addition to `nori:read` and `nori:write`, respectively. Setting storage
  replaces the settings. Omitting the secret key preserves it for the same
  destination and access key ID. The secret key is write-only.
- `get_backup_recovery_key` requires `nori:read` and `nori:backup_admin`.
  It returns the instance's private key when Nori manages it. Keep it in
  secure storage. The default OAuth grant excludes `nori:backup_admin`.

To download an encrypted local backup, call `get_backup_archive_download`
with its `backup_id`, then GET the returned path on Nori's origin with the
same OAuth bearer token. The endpoint supports HTTP Range requests.
To restore a local or S3 backup, call `prepare_backup_restore` with
`backup_id` or `s3_key` and, if needed, an external `recovery_key`.
To restore an archive file, call `get_backup_archive_upload` and POST the
file as `application/octet-stream` to the returned path with the same OAuth
bearer token. Set `Nori-Recovery-Key` when the archive uses an external key.
The upload responds with `draft_id` and `expires_at`, just like
`prepare_backup_restore`. This keeps the binary archive out of MCP messages.

Call `get_backup_restore_review` with `draft_id` to inspect the original
service and backup policy. Then use `confirm_backup_restore` with that ID,
a new service name, and `review_commands: true`. Supply fresh destinations
for non-managed volumes and binds in `targets`, keyed by the zero-based
index in `policy.sources`. Change the network, domain, or Custom deployment
script when needed. `cancel_backup_restore` discards the draft. Staging and
restoring require `nori:write`, and the draft stays with its originating OAuth
grant for up to 30 minutes.

## Service diagnostics

`get_service_resources` requires `nori:read` and `service_id`. It returns alert
settings and the latest recorded runtime and storage samples with timestamps
and readings owned by the service. Treat missing measurements as unavailable.
CPU is measured in percent, memory and storage in bytes, and network and disk
rates in bytes per second.

`update_service_resource_settings` requires `nori:write` and `service_id`.
Supply the fields to change. Set `usage_bytes` or `growth_bytes` to zero to
disable the corresponding storage alert. A supplied `runtime` object replaces
the runtime thresholds, and a supplied `directories` array replaces the
monitored directories. Empty values clear those collections. The
`postgres_details` option enables database size collection for managed
PostgreSQL. These thresholds generate alerts. Docker resource constraints
are part of the deployment configuration.

`get_instance_resources` returns instance totals, usage by service, storage
sources, and unattributed usage. `get_service_resource_history` returns
historical samples and deployment markers for one service. Select `1h`,
`24h`, `7d`, or `30d`. Each stream has up to 48 actual observations. For
live diagnostics, poll `get_container_logs` or `get_deployment` with
`include_logs: true`, as the dashboard does with its own endpoints.

`list_service_config_revisions` requires `nori:read`, `service_id`, and
`kind` (`script` or `env`). It returns up to 100 recent version numbers and
timestamps. Pass a version to `get_service_config_revision` to read a script
or the redacted dotenv template. To apply a full revision, use
`apply_service_config_revision` with `service_id`, `kind`, `version`, and
the destination's `expected_config_version`. Set `source_service_id` to
copy a revision from another service. The tool requires `nori:write` and
also `nori:secrets` for environment revisions. It writes archived values
without returning them. Applying a revision saves the configuration.
Use `deploy_service` when you are ready to deploy.

## Manual script services

To register a script for explicit execution, call `create_service` with these arguments:

```json
{
  "name": "container-inventory",
  "deployment_mode": "manual_script",
  "deploy_script": "docker ps"
}
```

Call `deploy_service` with the returned service ID to run it. Nori returns a
deployment ID for reading status and logs with `get_deployment`.

To convert a service, pass its `service_id`, current `expected_config_version`,
and `"deployment_mode": "manual_script"` to `update_service`. Nori clears the
watched image and cron expression and selects the manual policy. Supply
`deploy_script` as well if the service needs a new script. The script receives
`SERVICE`, `ENV_FILE`, and the service environment. If explicitly supplied,
`policy` must be `manual`.

## Deployment templates

For managed deployments, `create_service` and `update_service` accept
`single_container` or `postgres` as the `deployment_mode`, along with a typed,
non-secret `template_config`.
The exact same parser and validator serve the Dashboard and MCP. Template
services do not accept a generated Bash script as their durable configuration;
Custom services retain the established script contract when mode/configuration
are omitted.

`preview_service_template` returns the planned actions and managed resources
without dotenv values. `convert_service_to_custom` is explicit and one-way: it
writes the inspectable rendered Bash script, switches the service to `custom`,
and clears its template configuration. It does not reveal or interpolate
environment values. MCP exposes no raw Docker command or socket operation.

Template deployments require the standard `nori.service` label plus Nori's
template/service-ID/role labels, use digest-pinned application images, and fail
before touching the serving application when ownership or database readiness is
wrong. PostgreSQL templates retain their owned data volume across application
redeploys; database identity and major-version changes require an explicit
operator migration outside ordinary service mutation.

The scheduler reads only scheduled IDs, names and cron expressions once per
second. It preserves unchanged entries, removes stale entries and rechecks a
job's policy/expression when it fires. Invalid legacy expressions are remembered
until edited to avoid logging the same error every second. Do not replace this
with remove-and-recreate polling: that resets `@every` schedules.

## Lifetimes and capacity

OAuth lifetime constants live together in `mcpauth/server.go`. Pending client
registrations expire after 10 minutes; explicit consent extends them to one
year. Codes expire after 5 minutes. Access tokens expire after at most 10 minutes
and never outlive the grant. Refresh tokens retain the grant's original 30-day
deadline even as they rotate.

Storage is bounded at 1,000 client records and 20,000 total OAuth records,
including used tokens and revocation markers. Inserts remove expired records;
pending registrations cannot persist indefinitely without admin consent.
Registration has its own budget of 10 requests/minute. Valid credential
exchanges have a separate budget of 120 requests/client/minute. These budgets
are process-local; database consumption and revocation are persistent.

On a `503 temporarily_unavailable`, first check database availability. To inspect
capacity without exposing credentials, query counts only:

```sql
SELECT kind, used, count(*) AS records
FROM mcp_oauth
GROUP BY kind, used;
```

Global revocation in Settings clears capacity but also disconnects every client
and requires new registration/consent. Do not silently evict approved clients
or replay-detection records to make room.

## Verification

Run `make test` and `go vet ./...`; for concurrency or OAuth-storage changes,
also run:

```sh
go test -race ./internal/mcpauth ./internal/store ./internal/web ./internal/scheduler
```

The MCP tests use temporary SQLite databases, fake Docker and a real MCP SDK
client. They do not deploy containers on a live host.

- `mcpauth/server_test.go`: consent and denial, PKCE/resource/redirect checks,
  scopes, anonymous registration isolation, refresh replay, confidential
  clients, expiry, Host/Origin checks and epoch invalidation.
- `mcpauth/tokens_test.go`: advertised/stored access expiry, concurrent code and
  refresh exchange through two independent stores, live grants and revoked
  families across restart, and scope-escalation rejection.
- `web/mcp_settings_test.go`: real OAuth-to-MCP HTTP flow, default write access,
  write-only secret protection, Settings CSRF/origin/body limits, global and
  individual revocation isolation, safe rendered inventory, and Secure cookie
  upgrade behind a proxy.
- `web/mcp_test.go`: tool lifecycle, partial updates, scopes, self-service guards,
  and validation.
- `docker/logs_test.go`: bounded container log decoding, oversized frames and
  malformed streams.
- Store tests cover encryption, rollback, conflicting writes and bounded log
  reads. Scheduler tests cover creation, changes, deletion and invalid-then-fixed
  schedules without restart.

No browser UI test or independent external security audit is represented by
these checks. A rendered consent page and an actual supported OAuth client's
registration/refresh behavior still need release-level compatibility checks.

## Follow-up GitHub issues

### [Share service mutation and validation between the dashboard and MCP](https://github.com/tomusdrw/nori/issues/24)

Move ordinary service writes behind a common operation layer while retaining
the launcher's special self-service workflow. Apply consistent validation,
atomic config/environment saves and conflict handling to both interfaces.
Acceptance: dashboard/MCP parity tests; overlapping writes cannot silently
restore old configuration; invalid input never partially saves.

### [Verify OAuth interoperability and browser consent before release](https://github.com/tomusdrw/nori/issues/26)

Exercise discovery, registration, consent/denial, redirect return, refresh and
reauthorization with the intended native or backend MCP clients. Add browser
coverage at narrow/wide viewports, including long untrusted client names and
redirect URLs. Arrange a focused independent review of the built-in OAuth
boundaries; test-suite success alone is not an external audit.
