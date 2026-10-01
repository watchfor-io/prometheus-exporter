# Contributing

Thank you for looking. The exporter is small on purpose: it polls one
endpoint, adds one label and serves the result. Changes that keep it that
way are welcome.

## Building and testing

```sh
make build            # CGO_ENABLED=0, -trimpath, version stamped
make test             # go test -race ./...
make lint             # vet, gofmt, staticcheck, govulncheck, gosec
make docker           # the container image
```

Go 1.26 or newer; the `toolchain` line in `go.mod` fetches the exact patch
release. Tests need no network and no WatchFor account: the endpoint is
played by `httptest` servers. The sample responses in
`internal/exporter/testdata/` (`acme.prom`, `globex.prom`) were rendered by
WatchFor's own exposition code from a fixed snapshot, so they match what
the endpoint sends, HELP texts included.

If you change how responses are merged or labelled, update the golden file
and review the diff:

```sh
go test ./internal/exporter -run TestGoldenMerge -update
git diff internal/exporter/testdata/
```

## Pull requests

- One change per PR, with a test that fails without it.
- `main` takes pull requests only, with CI green: `gofmt`, `go mod tidy`,
  vet, race tests, staticcheck, govulncheck, gosec (G304 is excluded on
  purpose: the config file and `api_key_file` are paths the operator names).
- Comments explain a decision, not the code.
- User-facing changes get a line in `CHANGELOG.md` under the unreleased
  version and, where they change behaviour, a matching edit in `README.md`.
- Dependencies: the Prometheus client libraries, YAML, and the standard
  library. A new dependency needs a good reason.

## Security

Do not open an issue for a vulnerability; see [SECURITY.md](SECURITY.md).
