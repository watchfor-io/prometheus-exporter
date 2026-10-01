package exporter

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/common/expfmt"

	"github.com/watchfor-io/prometheus-exporter/internal/config"
)

var update = flag.Bool("update", false, "rewrite golden files")

const testKey = "wf_live_test_0123456789abcdef"

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Unix(1_790_870_400, 0)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// serve answers like the native endpoint: the given body, after checking the
// key. gz compresses the response when the client accepts it.
func serve(t *testing.T, body []byte, gz bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+testKey {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":{"code":"unauthorized","message":"Invalid API key."}}`)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		if gz && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			zw := gzip.NewWriter(w)
			zw.Write(body)
			zw.Close()
			return
		}
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func newTestExporter(clk *clock, targets ...config.Target) *Exporter {
	return New(Options{
		Targets:   targets,
		Interval:  time.Minute,
		Timeout:   5 * time.Second,
		Version:   "test",
		Revision:  "abc",
		GoVersion: "go1.test",
		Now:       clk.Now,
		Rand:      func() float64 { return 0 },
	})
}

func tgt(org, baseURL string) config.Target {
	return config.Target{Organization: org, APIKey: testKey, BaseURL: baseURL}
}

func gatherText(t *testing.T, g prometheus.Gatherer) string {
	t.Helper()
	mfs, err := prometheus.Gatherers{g}.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var buf bytes.Buffer
	enc := expfmt.NewEncoder(&buf, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, mf := range mfs {
		if err := enc.Encode(mf); err != nil {
			t.Fatal(err)
		}
	}
	return buf.String()
}

// TestGoldenMerge parses two realistic responses, one gzip-compressed, and
// checks the merged output byte for byte.
func TestGoldenMerge(t *testing.T) {
	acme, _ := serve(t, readTestdata(t, "acme.prom"), true)
	globex, _ := serve(t, readTestdata(t, "globex.prom"), false)
	clk := newClock()
	e := newTestExporter(clk, tgt("acme", acme.URL), tgt("globex", globex.URL))
	for _, tg := range e.targets {
		if err := e.poll(context.Background(), tg); err != nil {
			t.Fatalf("poll %s: %v", tg.cfg.Organization, err)
		}
	}
	got := gatherText(t, e)
	golden := filepath.Join("testdata", "merged.golden.prom")
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want := string(readTestdata(t, "merged.golden.prom"))
	if got != want {
		t.Errorf("merged output differs from %s (run go test ./internal/exporter -update to accept):\n%s", golden, diff(want, got))
	}
}

func diff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < max(len(w), len(g)); i++ {
		var a, b string
		if i < len(w) {
			a = w[i]
		}
		if i < len(g) {
			b = g[i]
		}
		if a != b {
			return "line " + strconv.Itoa(i+1) + ":\n  want: " + a + "\n  got:  " + b
		}
	}
	return ""
}

func TestRequestShape(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		io.WriteString(w, "# TYPE watchfor_health_score gauge\nwatchfor_health_score 1\n")
	}))
	defer srv.Close()
	no := false
	target := tgt("acme", srv.URL)
	target.Filters = config.Filters{Tag: []string{"prod", "web"}, Type: []string{"http"}, IncludeHosts: &no}
	e := newTestExporter(newClock(), target)
	if err := e.poll(context.Background(), e.targets[0]); err != nil {
		t.Fatal(err)
	}
	checks := map[string]string{
		"path":            got.URL.Path,
		"query":           got.URL.RawQuery,
		"Authorization":   got.Header.Get("Authorization"),
		"User-Agent":      got.Header.Get("User-Agent"),
		"Accept-Encoding": got.Header.Get("Accept-Encoding"),
	}
	want := map[string]string{
		"path":            "/api/v1/metrics",
		"query":           "include_hosts=false&tag=prod&tag=web&type=http",
		"Authorization":   "Bearer " + testKey,
		"User-Agent":      "watchfor-prometheus-exporter/test",
		"Accept-Encoding": "gzip",
	}
	for k, w := range want {
		if checks[k] != w {
			t.Errorf("%s = %q, want %q", k, checks[k], w)
		}
	}
}

func TestPollErrors(t *testing.T) {
	retryDate := time.Unix(1_790_870_400, 0).Add(90 * time.Second).UTC().Format(http.TimeFormat)
	cases := []struct {
		name       string
		status     int
		header     map[string]string
		body       string
		reason     string
		msg        string
		retryAfter time.Duration
	}{
		{"401 invalid key", 401, nil, `{"error":{"code":"unauthorized","message":"Invalid or revoked API key"}}`, ReasonAuth, "API key invalid", 0},
		{"403 plan (what the API sends)", 403, nil, `{"error":{"code":"forbidden","message":"The Prometheus metrics endpoint is included in the Pro plan and above. Upgrade under Settings > Subscription, then scrape this URL with the same API key."}}`, ReasonPlan, "Prometheus metrics need the Pro plan or above", 0},
		{"403 plan_required code", 403, nil, `{"error":{"code":"plan_required","message":"Upgrade."}}`, ReasonPlan, "plan does not include this (403 plan_required)", 0},
		{"403 no API access on the plan", 403, nil, `{"error":{"code":"forbidden","message":"API access is not included in your current plan"}}`, ReasonPlan, "API access is not included", 0},
		{"403 other", 403, nil, `{"error":{"code":"forbidden","message":"This account has no organization yet"}}`, ReasonAuth, "not allowed to read metrics (403 forbidden)", 0},
		{"429 seconds", 429, map[string]string{"Retry-After": "120"}, `{"error":{"code":"rate_limited","message":"Slow down."}}`, ReasonRateLimited, "rate limited", 120 * time.Second},
		{"429 http date", 429, map[string]string{"Retry-After": retryDate}, ``, ReasonRateLimited, "rate limited", 90 * time.Second},
		{"429 absurd retry capped", 429, map[string]string{"Retry-After": "999999"}, ``, ReasonRateLimited, "rate limited", time.Hour},
		{"500", 500, nil, `{"error":{"code":"internal_error","message":"boom"}}`, ReasonHTTP, "unexpected HTTP 500", 0},
		{"404", 404, nil, ``, ReasonHTTP, "not found (404); check base_url", 0},
		{"redirect not followed", 301, map[string]string{"Location": "https://elsewhere.example/"}, ``, ReasonHTTP, "redirected (301)", 0},
		{"200 html", 200, map[string]string{"Content-Type": "text/html"}, `<html>login</html>`, ReasonParse, "expected Prometheus text format", 0},
		{"200 garbage", 200, map[string]string{"Content-Type": "text/plain"}, "this is { not metrics\n", ReasonParse, "parsing the response", 0},
		{"200 empty", 200, map[string]string{"Content-Type": "text/plain"}, "", ReasonParse, "no metrics", 0},
		{"200 bad gzip", 200, map[string]string{"Content-Encoding": "gzip"}, "not gzip", ReasonParse, "claims gzip", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tc.header {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			clk := newClock()
			e := newTestExporter(clk, tgt("acme", srv.URL))
			err := e.poll(context.Background(), e.targets[0])
			var pe *PollError
			if !errors.As(err, &pe) {
				t.Fatalf("want *PollError, got %v", err)
			}
			if pe.Reason != tc.reason {
				t.Errorf("reason = %q, want %q (%s)", pe.Reason, tc.reason, pe.Message)
			}
			if !strings.Contains(pe.Message, tc.msg) {
				t.Errorf("message %q does not contain %q", pe.Message, tc.msg)
			}
			if strings.Contains(pe.Message, testKey) {
				t.Errorf("message leaks the API key: %q", pe.Message)
			}
			if pe.RetryAfter != tc.retryAfter {
				t.Errorf("RetryAfter = %s, want %s", pe.RetryAfter, tc.retryAfter)
			}
			if v := testutil.ToFloat64(e.pollErrors.WithLabelValues("acme", tc.reason)); v != 1 {
				t.Errorf("poll_errors_total{reason=%q} = %v, want 1", tc.reason, v)
			}
			if v := testutil.ToFloat64(e.up.WithLabelValues("acme")); v != 0 {
				t.Errorf("up = %v, want 0", v)
			}
			if e.Ready() {
				t.Error("ready after a failed poll")
			}
		})
	}
}

func TestNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	e := newTestExporter(newClock(), tgt("acme", url))
	err := e.poll(context.Background(), e.targets[0])
	var pe *PollError
	if !errors.As(err, &pe) || pe.Reason != ReasonNetwork {
		t.Fatalf("want network error, got %v", err)
	}
}

func TestTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	e := newTestExporter(newClock(), tgt("acme", srv.URL))
	e.timeout = 50 * time.Millisecond
	err := e.poll(context.Background(), e.targets[0])
	var pe *PollError
	if !errors.As(err, &pe) || pe.Reason != ReasonNetwork || !strings.Contains(pe.Message, "timed out") {
		t.Fatalf("want a network timeout, got %v", err)
	}
}

func TestKeyFileReadEveryPoll(t *testing.T) {
	srv, _ := serve(t, readTestdata(t, "globex.prom"), false)
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("wf_live_old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := config.Target{Organization: "globex", APIKeyFile: path, BaseURL: srv.URL}
	e := newTestExporter(newClock(), target)
	var pe *PollError
	if err := e.poll(context.Background(), e.targets[0]); !errors.As(err, &pe) || pe.Reason != ReasonAuth {
		t.Fatalf("old key: want auth error, got %v", err)
	}
	if err := os.WriteFile(path, []byte(testKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.poll(context.Background(), e.targets[0]); err != nil {
		t.Fatalf("rotated key: %v", err)
	}
}

func TestSuccessSelfMetrics(t *testing.T) {
	srv, _ := serve(t, readTestdata(t, "globex.prom"), false)
	clk := newClock()
	e := newTestExporter(clk, tgt("globex", srv.URL))
	if err := e.poll(context.Background(), e.targets[0]); err != nil {
		t.Fatal(err)
	}
	if v := testutil.ToFloat64(e.up.WithLabelValues("globex")); v != 1 {
		t.Errorf("up = %v, want 1", v)
	}
	if v := testutil.ToFloat64(e.lastSuccess.WithLabelValues("globex")); v != float64(clk.Now().Unix()) {
		t.Errorf("last_success = %v, want %v", v, clk.Now().Unix())
	}
	if !e.Ready() {
		t.Error("not ready after a successful poll")
	}
}

// TestStaleExpiry: after polls start failing the last good data is served
// for StaleAfter intervals, then dropped; the next success brings it back.
func TestStaleExpiry(t *testing.T) {
	var fail atomic.Bool
	body := readTestdata(t, "globex.prom")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Write(body)
	}))
	defer srv.Close()
	clk := newClock()
	e := newTestExporter(clk, tgt("globex", srv.URL))
	tg := e.targets[0]
	series := func() int {
		mfs, _ := e.Gather()
		n := 0
		for _, mf := range mfs {
			n += len(mf.Metric)
		}
		return n
	}

	if err := e.poll(context.Background(), tg); err != nil {
		t.Fatal(err)
	}
	fresh := series()
	if fresh == 0 {
		t.Fatal("no series after a successful poll")
	}

	fail.Store(true)
	steps := []struct {
		advance time.Duration
		poll    bool
		want    int
	}{
		{1 * time.Minute, true, fresh},
		{1 * time.Minute, true, fresh},
		{59 * time.Second, false, fresh}, // 2m59s: still within 3 intervals
		{1 * time.Second, true, fresh},   // exactly 3m: still served
		{1 * time.Second, false, 0},      // 3m01s: gone, even without a poll
		{1 * time.Minute, true, 0},
	}
	for i, s := range steps {
		clk.Advance(s.advance)
		if s.poll {
			_ = e.poll(context.Background(), tg)
		}
		if got := series(); got != s.want {
			t.Fatalf("step %d: %d series, want %d", i, got, s.want)
		}
	}
	if v := testutil.ToFloat64(e.up.WithLabelValues("globex")); v != 0 {
		t.Errorf("up = %v, want 0", v)
	}
	if v := testutil.ToFloat64(e.pollErrors.WithLabelValues("globex", ReasonHTTP)); v != 4 {
		t.Errorf("http errors = %v, want 4", v)
	}

	fail.Store(false)
	clk.Advance(time.Minute)
	if err := e.poll(context.Background(), tg); err != nil {
		t.Fatal(err)
	}
	if got := series(); got != fresh {
		t.Errorf("after recovery: %d series, want %d", got, fresh)
	}
}

func TestNextDelay(t *testing.T) {
	e := newTestExporter(newClock())
	e.interval = time.Minute
	rl := &PollError{Reason: ReasonRateLimited, RetryAfter: 5 * time.Minute}
	cases := []struct {
		name     string
		err      error
		failures int
		want     time.Duration
	}{
		{"success", nil, 0, time.Minute},
		{"first failure retries at the interval", &PollError{Reason: ReasonNetwork}, 1, time.Minute},
		{"second failure doubles", &PollError{Reason: ReasonNetwork}, 2, 2 * time.Minute},
		{"third failure doubles again", &PollError{Reason: ReasonNetwork}, 3, 4 * time.Minute},
		{"capped", &PollError{Reason: ReasonAuth}, 10, maxBackoff},
		{"huge failure count stays capped", &PollError{Reason: ReasonAuth}, 1000, maxBackoff},
		{"retry-after wins when longer", rl, 1, 5 * time.Minute},
		{"backoff wins when longer than retry-after", rl, 5, maxBackoff},
		{"short retry-after never polls faster than the interval", &PollError{Reason: ReasonRateLimited, RetryAfter: time.Second}, 1, time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := e.nextDelay(tc.err, tc.failures); got != tc.want {
				t.Errorf("nextDelay = %s, want %s", got, tc.want)
			}
		})
	}
	e.rand = func() float64 { return 0.999 }
	if got := e.nextDelay(nil, 0); got < time.Minute || got > time.Minute+6*time.Second {
		t.Errorf("jittered delay %s outside [60s, 66s]", got)
	}
}

func TestHandler(t *testing.T) {
	srv, _ := serve(t, readTestdata(t, "acme.prom"), true)
	e := newTestExporter(newClock(), tgt("acme", srv.URL))
	h := Handler(e, "test", nil)

	get := func(path string) (int, string) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code, rec.Body.String()
	}
	if code, _ := get("/healthz"); code != 200 {
		t.Errorf("/healthz = %d before any poll, want 200", code)
	}
	if code, _ := get("/readyz"); code != 503 {
		t.Errorf("/readyz = %d before any poll, want 503", code)
	}
	code, body := get("/metrics")
	if code != 200 || !strings.Contains(body, `watchfor_exporter_up{organization="acme"} 0`) {
		t.Errorf("/metrics before a poll: %d\n%s", code, body)
	}
	if strings.Contains(body, "watchfor_monitor_up") {
		t.Error("/metrics serves monitor data before any poll")
	}

	if err := e.poll(context.Background(), e.targets[0]); err != nil {
		t.Fatal(err)
	}
	if code, _ := get("/readyz"); code != 200 {
		t.Errorf("/readyz = %d after a poll, want 200", code)
	}
	_, body = get("/metrics")
	for _, want := range []string{
		`watchfor_exporter_up{organization="acme"} 1`,
		`watchfor_exporter_build_info{goversion="go1.test",revision="abc",version="test"} 1`,
		`watchfor_exporter_poll_errors_total{organization="acme",reason="plan"} 0`,
		`watchfor_monitor_up{monitor_id="0fe0d9ba-c062-48f5-a9ec-ead9a4076ebd",organization="acme"} 1`,
		`watchfor_organization_info{exported_organization="acme",organization="acme",plan="pro"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics lacks %s", want)
		}
	}
	if code, _ := get("/nope"); code != 404 {
		t.Errorf("/nope = %d, want 404", code)
	}
}

// TestRunPollsAndStops drives the real loop with a short interval and checks
// that it polls repeatedly and returns promptly on cancellation.
func TestRunPollsAndStops(t *testing.T) {
	srv, hits := serve(t, readTestdata(t, "globex.prom"), false)
	e := New(Options{
		Targets:  []config.Target{tgt("globex", srv.URL)},
		Interval: 20 * time.Millisecond,
		Timeout:  time.Second,
		Version:  "test",
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for hits.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if hits.Load() < 3 {
		t.Fatalf("%d polls, want at least 3", hits.Load())
	}
	if !e.Ready() {
		t.Error("not ready")
	}
}
