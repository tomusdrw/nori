# MCP access

- MCP is the primary interface for working with Nori. Provide MCP access to every operation users can perform in the dashboard. An operation may be available only in the dashboard with the repository owner's explicit approval.
- Make every new Nori feature available to agents through MCP as part of the same change. When adding a feature to the dashboard, cover the corresponding read and write operations in MCP as applicable. Reuse the feature's validation and enforce the appropriate MCP permissions.
- If you are unsure whether agents should have access to a feature or any of its operations, ask the repository owner for explicit approval before deciding. Name the operation in question and explain the tradeoff. Any exception to MCP coverage requires the owner's approval.
- Update MCP tool descriptions and documentation so agents can discover and use the feature. Verify that the new behavior works through MCP with the intended permissions.
- The repository owner approved dashboard-only instance settings and OAuth administration, dashboard controls, the interactive terminal, and operation of the launcher-managed Nori self-service. Backup storage, recovery, and disk settings remain in scope for MCP.
