package exporter

import (
	"fmt"
	"html"
	"log/slog"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const landingPage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>WatchFor Prometheus exporter</title></head>
<body>
<h1>WatchFor Prometheus exporter</h1>
<p>Version %s</p>
<ul>
<li><a href="/metrics">/metrics</a> &mdash; WatchFor metrics for every configured organization, plus the exporter's own</li>
<li><a href="/healthz">/healthz</a> &mdash; the process is alive</li>
<li><a href="/readyz">/readyz</a> &mdash; at least one poll has succeeded</li>
</ul>
<p><a href="https://github.com/watchfor-io/prometheus-exporter">github.com/watchfor-io/prometheus-exporter</a></p>
</body></html>
`

// Handler serves /metrics, /healthz, /readyz and a small landing page.
func Handler(e *Exporter, version string, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(
		prometheus.Gatherers{e.Registry(), e},
		promhttp.HandlerOpts{
			ErrorHandling:       promhttp.ContinueOnError,
			ErrorLog:            slogPrinter{log},
			MaxRequestsInFlight: 10,
		},
	))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if !e.Ready() {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintln(w, "not ready: no successful poll yet")
			return
		}
		fmt.Fprintln(w, "ready")
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, landingPage, html.EscapeString(version))
	})
	return mux
}

type slogPrinter struct{ log *slog.Logger }

func (p slogPrinter) Println(v ...any) { p.log.Error(fmt.Sprint(v...)) }
