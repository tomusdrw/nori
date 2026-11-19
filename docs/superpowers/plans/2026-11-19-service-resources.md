# Service resource monitoring implementation plan

Implement [issue 57](https://github.com/tomusdrw/nori/issues/57) using the existing Docker client, SQLite store, authenticated dashboard, and administrator notification channels.

1. Collect runtime counters and storage inventory in separate workers. Key runtime baselines by container identity and start time. Collect physical sources once and record all associated service IDs. Keep missing values distinct from measured zero.
2. Store timestamped observations atomically. Keep detailed samples for a day and hourly observations for 30 days. Persist service limits and alert state.
3. Add an instance ranking and per-service source breakdown, history, deployment markers and settings. Compare measurements against their timestamps. Separate physical storage usage, logical PostgreSQL estimates, and disk throughput.
4. Run independent sampling loops with cancellation and bounded deadlines. Send usage transitions and sustained-growth alerts through existing administrator channels, including links to the resource breakdown.
5. Verify reset counters, partial measurements, overlapping ownership, stale history, alert transitions, and the authenticated dashboard. Run the Go suite and check a real daemon when available.
