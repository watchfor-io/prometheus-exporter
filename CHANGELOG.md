# Changelog

Releases are tagged `vX.Y.Z`. Dates are the day the tag was pushed.

## 0.1.0 — unreleased

First release.

- Polls `GET /api/v1/metrics` for one or more WatchFor organizations, each
  with its own API key, in the background (default every 60 s, which is also
  the minimum: the endpoint caches for 60 s), with jitter, a timeout, gzip,
  exponential backoff on errors and `Retry-After` on 429.
- Serves the merged result on `/metrics` with an `organization` label on
  every series; a label the endpoint already called `organization` is kept
  as `exported_organization`.
- Last good data is served for up to three intervals after polls start
  failing, then dropped.
- Self-metrics: `watchfor_exporter_up`,
  `watchfor_exporter_last_success_timestamp_seconds`,
  `watchfor_exporter_poll_duration_seconds`,
  `watchfor_exporter_poll_errors_total{reason}` (auth, plan, rate_limited,
  http, network, parse) and `watchfor_exporter_build_info`.
- `/healthz`, `/readyz`, graceful shutdown on SIGTERM.
- Grafana dashboard `dashboards/watchfor-overview.json` (Grafana 11.6–13):
  fleet health, status timeline, uptime against an SLO target, response
  times, certificate and domain expiry, heartbeats and hosts. Works with
  direct scraping and through the exporter; the Docker Compose example
  provisions it automatically.
- Configuration through flags, environment variables and a YAML file;
  `WATCHFOR_API_KEY` alone is enough to start. Keys can come from files
  (`api_key_file`), which are re-read on every poll.
- Server-side filters per organization: `tag`, `type`, `monitor_id`,
  `include_hosts`.
- Static binaries for Linux, macOS and Windows (amd64, arm64) and a
  distroless container image for linux/amd64 and linux/arm64.
