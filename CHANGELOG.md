# Changelog

Releases are tagged `vX.Y.Z`. Dates are the day the tag was pushed.

## 0.1.2 — 2026-10-22

- **Built with Go 1.27.3.** Go 1.27.2's `crypto/x509` has GO-2026-6809,
  reachable when the exporter verifies the WatchFor API's TLS certificate,
  so every binary and image needs a rebuild; nothing else changes in
  behaviour.
- Runtime image: distroless `static-debian13:nonroot` refreshed to the
  current digest.

## 0.1.1 — 2026-10-15

- **Built with Go 1.27.2.** 0.1.0 was compiled with Go 1.26.8, whose
  `net/http` and HTTP/2 code has known vulnerabilities (GO-2026-6611,
  GO-2026-6612, GO-2026-6613, GO-2026-6617). The exporter both serves
  `/metrics` and polls the WatchFor API over HTTP, so every binary and
  image is rebuilt; nothing else changes in behaviour.
- Dependencies: `prometheus/client_golang` 1.25.0, `prometheus/common`
  0.72.1.

## 0.1.0 — 2026-10-01

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
