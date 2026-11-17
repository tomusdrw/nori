# Manual Script Service Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:executing-plans` to implement this plan task-by-task.

**Goal:** Add a no-image manual Bash deployment mode that can be launched in Nori and is clearly marked as manual in the UI and agent interface.

**Architecture:** Add `manual_script` to the existing deployment mode values. Keep its deployment on the Bash runner with the existing encrypted environment and deployment-history paths, while bypassing registry digest resolution and polling. Render a dedicated manual status and action in the service UI.

**Tech Stack:** Go 1.26, SQLite-backed store, templ UI, Bash deployment executor, MCP service tools.

**Spec:** `docs/superpowers/specs/2026-11-17-manual-script-service-design.md`

## Global Constraints

- Keep implementation within the existing `DeploymentMode` representation and Bash runner.
- `PolicyManual` remains the policy value for the new mode; it is a human-triggered script run, not an image update policy.
- Cover dashboard and MCP conversion and explicit script execution in regression tests.

## Review Focus

- Existing image-backed custom and template modes keep their current digest lookup and polling behavior.
- `manual_script` does not accidentally parse as a structured template in executor or MCP helpers.
- Immediate/scheduled policies are rejected for `manual_script` so it cannot auto-deploy.
- Empty image values are accepted only for `manual_script`; invalid non-empty image references remain rejected.
- Dashboard/detail rendering never presents a blank image version as an update or as a deploy target.

---

### Task 1: Define and validate the manual script mode

**Files:**
- Modify: `internal/store/models.go`
- Modify: `internal/web/template_config.go`
- Modify: `internal/web/service_write.go`
- Modify: `internal/web/server.go`
- Modify: `internal/web/mcp.go`

**Interfaces:**
- Produces: `store.DeploymentModeManualScript DeploymentMode = "manual_script"`.
- Produces: `normalizedTemplateConfig` accepts `manual_script` and normalizes its unused template config to `{}`.
- Consumes: the existing `Service.DeploymentMode`, `PolicyManual`, and validation paths.

- [x] Add the new deployment mode constant and ensure `normalizedTemplateConfig` recognizes it without passing it to the structured template parser.
- [x] In ordinary service validation, allow an empty `WatchedImage` only in `manual_script`; validate any non-empty image reference as before; require `PolicyManual`; require a non-empty syntactically valid Bash script.
- [x] In create/update form and MCP inputs, preserve the selected mode, permit omitted `watched_image` only in this mode, and update the MCP schema description to include `manual_script`.
- [x] Check the relevant validation and conversion paths by source inspection and regression tests.

### Task 2: Run scripts without registry lookups

**Files:**
- Modify: `internal/executor/executor.go`
- Modify: `internal/poller/poller.go`

**Interfaces:**
- Consumes: `store.DeploymentModeManualScript` from Task 1.
- Produces: `Executor.Deploy` accepts manual-script services without calling `LatestDigestFunc`; their deployment history has an empty `TargetDigest` and their Bash environment retains `SERVICE`, `ENV_FILE`, and decrypted env vars.

- [x] Branch digest resolution in `Executor.Deploy` so only `manual_script` bypasses `e.latest`; preserve current image-backed behavior and use empty digest for the manual run.
- [x] Ensure `buildEnv` can build the manual script environment with an empty digest and does not inject a fake image/digest value.
- [x] Skip `manual_script` services in `Poller.Tick`; do not call the registry or trigger automatic deployment for them.
- [x] Inspect every `DeploymentMode` branch in the executor and verify manual script mode cannot enter structured-template deployment.

### Task 3: Mark the mode in the service UI

**Files:**
- Modify: `internal/web/forms.go`
- Modify: `internal/web/view.go`
- Modify: `internal/web/service_form.templ`
- Modify: `internal/web/dashboard.templ`
- Modify: `internal/web/service_detail.templ`
- Modify: `internal/web/server.go`
- Modify: `internal/web/static/app.css`
- Modify: `internal/web/static/template-form.js`

**Interfaces:**
- Consumes: the service deployment mode in dashboard and detail views.
- Produces: a `manual_script` selection with no required image input; a visible “Manual deployment” badge; a “Run script” action.

- [x] Make the image input optional only when `manual_script` is selected, including toggling its HTML `required` property as the selector changes, and explain that no image is monitored; keep image required for other modes.
- [x] Add manual-script help in the script panel that documents `$SERVICE`, `$ENV_FILE`, and service environment variables, and that image/digest variables are unavailable.
- [x] Add a `ManualScript` view flag in `ServiceView` and `ServiceFormData`, populate it in `internal/web/server.go`, then render a prominent “Manual deployment” badge on dashboard cards and service details.
- [x] Hide image version/update presentation for this mode and show “Run script” for the existing explicit deploy action; retain state, health, logs, and history.
- [x] Regenerate templ Go sources with the repository's existing `templ generate` workflow and run `go build ./...` to verify generated sources compile.

### Task 4: Review the completed behavior

**Files:**
- Review: all files changed in Tasks 1–3.

- [x] Inspect create, update, MCP, polling, explicit deploy, dashboard, and detail paths for the five review-focus cases above.
- [x] Review the final diff for migration requirements and confirm none are needed because empty strings fit the existing `watched_image` column and the mode is stored in the existing deployment-mode column.
- [x] Run `go build ./...` after the final edits; verify the regression cases with the Go test suite.
