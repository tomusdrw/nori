# Manual script services

## Intent

Let Nori track services that are deployed only when an operator or agent explicitly runs a script, without requiring a placeholder watched image. Make these services easy to spot as potentially temporary.

## Goals

- Add a deployment mode for a manual Bash script that does not watch or require a container image.
- Keep the Nori “Deploy” action for explicitly running that script, and retain environment-file encryption, deployment history, and logs.
- Mark the mode clearly on the dashboard card and service detail page; label its action as a manual script run.
- Skip image polling, digest lookup, and image-version/update UI for this mode.
- Expose the mode through the agent-facing MCP create/update service configuration.

## Non-goals

- Automatically deploy, schedule, or poll registry images for this mode.
- Hide service/container health, logs, history, or environment configuration.
- Convert existing image-watching services automatically.

## Approaches

1. Add a separate manual-script deployment mode. It reuses the existing Bash executor and deployment history, with digest lookup bypassed. This makes the no-image contract explicit and visible to agents. **Selected.**
2. Keep the existing deployment mode and add an independent “watch image” switch. This creates combinations such as a template with no image and would require additional validation rules.
3. Treat this as a dashboard-only inventory record. This would remove Nori's script-run action and does not match the requested ability to run manual deployments from Nori.

## Design

Use a new deployment mode value (for example, `manual_script`) as the explicit source of deployment behavior. Its configuration consists of a Bash deploy script and the existing encrypted environment file; no watched image is required. A manual script may still use Docker or deploy an external artifact itself. Store the mode in the existing deployment-mode column, so no schema migration is needed. Existing services remain unchanged.

The UI will present the mode as “Manual script (no watched image)” in the deployment-mode selector. The script help will explain that image/digest variables are unavailable; `$SERVICE`, `$ENV_FILE`, and environment variables remain available. The dashboard and detail page will show a prominent “Manual deployment” badge, omit image-version/update indicators, and label the explicit action “Run script”. Regular container state, health, logs, history, and environment handling remain available.

The executor will skip registry resolution for this mode, use an empty target digest in deployment history, and build the environment without image digest substitutions. The image poller will skip it. Validation will allow an empty watched image only for this mode, require a valid Bash script, and reject scheduled/immediate policy for it so it cannot be mistaken for an automatic deployment. Agent-facing create/update tools will accept and describe the new mode; creating one will not require `watched_image`.

## Constraints and calls

- Keep the implementation within the existing `DeploymentMode` representation and Bash runner.
- `PolicyManual` remains the policy value for the new mode; it is a human-triggered script run, not an image update policy.
- Converting a service clears its watched image and cron expression. An omitted policy defaults to manual when selecting this mode. An explicit conflicting policy is rejected.
- Regression coverage includes dashboard and MCP conversion and script execution without registry access.
