// Package exporter polls the WatchFor Prometheus endpoint for each configured
// organization and serves the merged result.
package exporter

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/watchfor-io/prometheus-exporter/internal/config"
)

const (
	// StaleAfter is how many intervals the last good data is served after
	// polls start failing. After that the organization's series disappear
	// instead of going flat forever.
	StaleAfter = 3
	// maxBackoff caps the wait between failing polls.
	maxBackoff = 10 * time.Minute
	// jitterFraction spreads polls by up to this share of the interval.
	// Jitter only ever lengthens the wait, never shortens it below the
	// interval: the server's 60 s cache makes a shorter one pointless.
	jitterFraction = 0.1
	maxStartJitter = 5 * time.Second
)

// Options configure an Exporter.
type Options struct {
	Targets   []config.Target
	Interval  time.Duration
	Timeout   time.Duration
	Version   string
	Revision  string
	GoVersion string
	Logger    *slog.Logger

	// For tests.
	Client *http.Client
	Now    func() time.Time
	Rand   func() float64 // in [0, 1)
}

// Exporter owns one poll loop per target and implements prometheus.Gatherer
// for the data those loops collected.
type Exporter struct {
	targets  []*target
	interval time.Duration
	timeout  time.Duration
	ua       string
	client   *http.Client
	log      *slog.Logger
	now      func() time.Time
	rand     func() float64
	ready    atomic.Bool

	registry     *prometheus.Registry
	up           *prometheus.GaugeVec
	lastSuccess  *prometheus.GaugeVec
	pollDuration *prometheus.GaugeVec
	pollErrors   *prometheus.CounterVec
}

type target struct {
	cfg config.Target

	mu          sync.Mutex
	families    []*dto.MetricFamily // labelled; nil when there is nothing to serve
	lastSuccess time.Time
	failing     bool // the last poll failed
}

// New builds an Exporter. It does not start polling; call Run.
func New(o Options) *Exporter {
	e := &Exporter{
		interval: o.Interval,
		timeout:  o.Timeout,
		ua:       "watchfor-prometheus-exporter/" + o.Version,
		client:   o.Client,
		log:      o.Logger,
		now:      o.Now,
		rand:     o.Rand,
		registry: prometheus.NewRegistry(),
	}
	if e.client == nil {
		e.client = newHTTPClient()
	}
	if e.log == nil {
		e.log = slog.New(slog.DiscardHandler)
	}
	if e.now == nil {
		e.now = time.Now
	}
	if e.rand == nil {
		e.rand = rand.Float64
	}
	for _, t := range o.Targets {
		e.targets = append(e.targets, &target{cfg: t})
	}

	e.up = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "watchfor_exporter_up",
		Help: "1 if the last poll of the organization's WatchFor metrics succeeded, 0 otherwise.",
	}, []string{OrganizationLabel})
	e.lastSuccess = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "watchfor_exporter_last_success_timestamp_seconds",
		Help: "Unix time of the last successful poll; 0 until the first one.",
	}, []string{OrganizationLabel})
	e.pollDuration = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "watchfor_exporter_poll_duration_seconds",
		Help: "Duration of the last poll, successful or not.",
	}, []string{OrganizationLabel})
	e.pollErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "watchfor_exporter_poll_errors_total",
		Help: "Failed polls by reason: auth, plan, rate_limited, http, network, parse.",
	}, []string{OrganizationLabel, "reason"})
	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "watchfor_exporter_build_info",
		Help: "Build information of the exporter; always 1.",
	}, []string{"version", "revision", "goversion"})
	buildInfo.WithLabelValues(o.Version, o.Revision, o.GoVersion).Set(1)

	for _, t := range e.targets {
		org := t.cfg.Organization
		e.up.WithLabelValues(org).Set(0)
		e.lastSuccess.WithLabelValues(org).Set(0)
		e.pollDuration.WithLabelValues(org).Set(0)
		for _, r := range Reasons {
			e.pollErrors.WithLabelValues(org, r)
		}
	}
	e.registry.MustRegister(e.up, e.lastSuccess, e.pollDuration, e.pollErrors, buildInfo)
	return e
}

// Registry holds the exporter's own metrics; callers may add the Go and
// process collectors to it.
func (e *Exporter) Registry() *prometheus.Registry { return e.registry }

// Ready reports whether at least one poll has succeeded.
func (e *Exporter) Ready() bool { return e.ready.Load() }

// Run polls every target until ctx is cancelled, then returns.
func (e *Exporter) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, t := range e.targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.loop(ctx, t)
		}()
	}
	wg.Wait()
}

func (e *Exporter) loop(ctx context.Context, t *target) {
	// Targets start within a few seconds of each other rather than all at
	// once, and the first data arrives right away.
	delay := time.Duration(e.rand() * float64(min(maxStartJitter, time.Duration(float64(e.interval)*jitterFraction))))
	failures := 0
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		err := e.poll(ctx, t)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			failures = 0
		} else {
			failures++
		}
		timer.Reset(e.nextDelay(err, failures))
	}
}

// nextDelay is the wait before the next poll: the interval after a success,
// an exponential backoff (capped) after failures, and never less than what
// a 429's Retry-After asked for. Jitter is added on top.
func (e *Exporter) nextDelay(err error, failures int) time.Duration {
	d := e.interval
	if err != nil && failures > 1 {
		d = e.interval << min(failures-1, 16)
		if d > maxBackoff || d <= 0 {
			d = max(maxBackoff, e.interval)
		}
	}
	var pe *PollError
	if errors.As(err, &pe) && pe.RetryAfter > d {
		d = pe.RetryAfter
	}
	return d + time.Duration(e.rand()*jitterFraction*float64(e.interval))
}

// poll fetches one target once, updates its data and the self-metrics, and
// returns the classified error, if any.
func (e *Exporter) poll(ctx context.Context, t *target) error {
	org := t.cfg.Organization
	start := e.now()
	families, err := e.fetchTarget(ctx, t)
	elapsed := e.now().Sub(start)
	e.pollDuration.WithLabelValues(org).Set(elapsed.Seconds())

	if err != nil {
		if ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
			return err
		}
		var pe *PollError
		if !errors.As(err, &pe) {
			pe = &PollError{Reason: ReasonNetwork, Message: err.Error()}
		}
		e.up.WithLabelValues(org).Set(0)
		e.pollErrors.WithLabelValues(org, pe.Reason).Inc()
		level := slog.LevelWarn
		if pe.Reason == ReasonAuth || pe.Reason == ReasonPlan {
			level = slog.LevelError
		}
		e.log.Log(ctx, level, "poll failed", "organization", org, "reason", pe.Reason, "status", pe.Status, "error", pe.Message)
		t.mu.Lock()
		t.failing = true
		if t.families != nil && e.expired(t.lastSuccess) {
			t.families = nil
			e.log.Warn("dropping the last good data; it is older than the stale limit", "organization", org, "last_success", t.lastSuccess.UTC().Format(time.RFC3339))
		}
		t.mu.Unlock()
		return pe
	}

	labelled := withOrganization(families, org)
	now := e.now()
	t.mu.Lock()
	recovered := t.failing
	t.failing = false
	t.families = labelled
	t.lastSuccess = now
	t.mu.Unlock()
	e.up.WithLabelValues(org).Set(1)
	e.lastSuccess.WithLabelValues(org).Set(float64(now.UnixNano()) / 1e9)
	if !e.ready.Swap(true) || recovered {
		e.log.Info("poll succeeded", "organization", org, "families", len(labelled), "duration", elapsed.Round(time.Millisecond).String())
	} else {
		e.log.Debug("poll succeeded", "organization", org, "families", len(labelled), "duration", elapsed.Round(time.Millisecond).String())
	}
	return nil
}

func (e *Exporter) fetchTarget(ctx context.Context, t *target) (map[string]*dto.MetricFamily, error) {
	key, err := t.cfg.Key()
	if err != nil {
		return nil, &PollError{Reason: ReasonAuth, Message: "reading the API key: " + err.Error()}
	}
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	return fetch(ctx, e.client, t.cfg.MetricsURL(), key, e.ua, e.now)
}

func (e *Exporter) expired(last time.Time) bool {
	return e.now().Sub(last) > StaleAfter*e.interval
}

// Gather implements prometheus.Gatherer with the organizations' data only;
// combine it with Registry() for a full /metrics.
func (e *Exporter) Gather() ([]*dto.MetricFamily, error) {
	per := make([][]*dto.MetricFamily, 0, len(e.targets))
	for _, t := range e.targets {
		t.mu.Lock()
		if t.families != nil && e.expired(t.lastSuccess) {
			t.families = nil
			e.log.Warn("dropping the last good data; it is older than the stale limit", "organization", t.cfg.Organization, "last_success", t.lastSuccess.UTC().Format(time.RFC3339))
		}
		if t.families != nil {
			per = append(per, t.families)
		}
		t.mu.Unlock()
	}
	return merge(per, func(name string) {
		e.log.Warn("metric family has different types across organizations; skipping the later one", "family", name)
	}), nil
}

func newHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	// Accept-Encoding is set and gzip decoded by hand, so the response size
	// cap applies to the decompressed body.
	tr.DisableCompression = true
	tr.MaxIdleConnsPerHost = 4
	return &http.Client{
		Transport: tr,
		// A redirect is reported, not followed: the key goes only to the
		// configured base_url.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
