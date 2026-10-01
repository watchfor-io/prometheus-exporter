# Security

## Reporting

Please report vulnerabilities privately through GitHub's
[private vulnerability reporting](https://github.com/watchfor-io/prometheus-exporter/security/advisories/new)
on this repository, or by email to hello@watchfor.io with "security" in the
subject. You will get an answer within three working days. Please do not open
a public issue for anything that could be exploited before a fix is out.

How WatchFor handles security in general, including the service this
exporter talks to: [watchfor.io/docs/security](https://watchfor.io/docs/security).

Supported: the latest release. Fixes ship as a new release, never as a
rewritten one.

## What the exporter does, and does not do

- Outbound HTTPS to one URL per configured organization,
  `<base_url>/api/v1/metrics`, with that organization's API key as a bearer
  token. Plain HTTP is refused unless the host is `localhost` or a loopback
  address. Redirects are not followed, so the key is only ever sent to the
  configured `base_url`. TLS 1.2 or newer.
- API keys come from the config file, a file it names (`api_key_file`) or an
  environment variable. They are never logged, never put in a URL and never
  exposed on `/metrics`; error messages from configuration validation and
  from failed polls are tested not to contain them.
- Inbound: one HTTP listener (`:10056` by default) serving `/metrics`,
  `/healthz`, `/readyz` and a static landing page. It has no authentication
  and no TLS of its own. **`/metrics` carries your monitors' names, targets
  and tags.** Keep the port on a private network, behind a NetworkPolicy, or
  behind a proxy that adds authentication.
- It runs nothing from configuration or from the server's responses. A
  response is parsed as Prometheus text and nothing else; the decompressed
  body is capped at 128 MiB, and families in the exporter's own
  `watchfor_exporter_` namespace are dropped from responses so the server
  cannot impersonate the exporter's health metrics.
- It writes nothing to disk. The container image is distroless/static,
  runs as uid 65532 and works with a read-only root filesystem and no
  capabilities.
- Release archives are static binaries built from the tag by GoReleaser,
  with a `checksums.txt` and a GitHub build-provenance attestation
  (`gh attestation verify <file> --repo watchfor-io/prometheus-exporter`).
  The container image carries an attestation too. Every release is rebuilt
  from its tag on a fresh runner and compared byte for byte with the
  published binaries.
