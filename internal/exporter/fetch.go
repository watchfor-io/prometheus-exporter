package exporter

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// Error reasons, the values of the reason label on
// watchfor_exporter_poll_errors_total.
const (
	ReasonAuth        = "auth"
	ReasonPlan        = "plan"
	ReasonRateLimited = "rate_limited"
	ReasonHTTP        = "http"
	ReasonNetwork     = "network"
	ReasonParse       = "parse"
)

// Reasons lists every reason, so each counter exists from the start.
var Reasons = []string{ReasonAuth, ReasonPlan, ReasonRateLimited, ReasonHTTP, ReasonNetwork, ReasonParse}

const (
	// maxBodyBytes caps a decompressed response. A large organization
	// (tens of thousands of monitors at ~12 series each) stays far below it.
	maxBodyBytes  = 128 << 20
	maxErrorBytes = 4096
	maxRetryAfter = time.Hour
)

// PollError is a failed poll, classified.
type PollError struct {
	Reason     string
	Status     int           // HTTP status, 0 when no response arrived
	Message    string        // what to tell the operator; never contains the key
	RetryAfter time.Duration // from a 429's Retry-After, 0 otherwise
}

func (e *PollError) Error() string { return e.Message }

// apiError is the JSON envelope /api/v1 uses for every error:
// {"error":{"code":"...","message":"..."}}.
type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// fetch performs one GET against the native endpoint and parses the body.
func fetch(ctx context.Context, client *http.Client, url, key, userAgent string, now func() time.Time) (map[string]*dto.MetricFamily, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, &PollError{Reason: ReasonNetwork, Message: "building the request: " + err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "text/plain;version=0.0.4;q=1,*/*;q=0.1")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("User-Agent", userAgent)

	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, &PollError{Reason: ReasonNetwork, Message: "timed out waiting for " + url}
		}
		// The URL never carries the key (it travels in a header), so the
		// transport error may name it.
		return nil, &PollError{Reason: ReasonNetwork, Message: clean(err.Error())}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, statusError(resp, url, now)
	}

	body := io.Reader(resp.Body)
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		zr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, &PollError{Reason: ReasonParse, Status: resp.StatusCode, Message: "response claims gzip but is not: " + err.Error()}
		}
		defer zr.Close()
		body = zr
	}

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		mt, _, _ := mime.ParseMediaType(ct)
		if mt == "text/html" || mt == "application/json" {
			return nil, &PollError{Reason: ReasonParse, Status: resp.StatusCode,
				Message: fmt.Sprintf("expected Prometheus text format from %s, got %s; check base_url", url, mt)}
		}
	}

	limited := &io.LimitedReader{R: body, N: maxBodyBytes + 1}
	parser := expfmt.NewTextParser(model.LegacyValidation)
	families, err := parser.TextToMetricFamilies(limited)
	if limited.N <= 0 {
		return nil, &PollError{Reason: ReasonParse, Status: resp.StatusCode, Message: fmt.Sprintf("response is larger than %d MiB", maxBodyBytes>>20)}
	}
	if err != nil {
		return nil, &PollError{Reason: ReasonParse, Status: resp.StatusCode, Message: "parsing the response: " + err.Error()}
	}
	if len(families) == 0 {
		return nil, &PollError{Reason: ReasonParse, Status: resp.StatusCode, Message: "the response contained no metrics"}
	}
	return families, nil
}

func statusError(resp *http.Response, url string, now func() time.Time) *PollError {
	var env apiError
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
	_ = json.Unmarshal(raw, &env)
	code, msg := env.Error.Code, clean(env.Error.Message)
	detail := ""
	if msg != "" {
		detail = ": " + msg
	}
	e := &PollError{Status: resp.StatusCode}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		e.Reason = ReasonAuth
		e.Message = "API key invalid, expired or revoked (401)" + detail
	// The API answers a plan restriction with 403 and a message naming
	// the plan; plan_required is accepted too should it get its own code.
	case resp.StatusCode == http.StatusForbidden && (code == "plan_required" || strings.Contains(strings.ToLower(msg), "plan")):
		e.Reason = ReasonPlan
		e.Message = "the organization's plan does not include this (403" + codeSuffix(code) + "); Prometheus metrics need the Pro plan or above" + detail
	case resp.StatusCode == http.StatusForbidden:
		e.Reason = ReasonAuth
		e.Message = "API key is not allowed to read metrics (403" + codeSuffix(code) + ")" + detail
	case resp.StatusCode == http.StatusTooManyRequests:
		e.Reason = ReasonRateLimited
		e.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), now())
		e.Message = "rate limited (429); the endpoint allows 30 requests per minute per organization, shared by every client polling it"
		if e.RetryAfter > 0 {
			e.Message += fmt.Sprintf("; server asked to wait %s", e.RetryAfter)
		}
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		e.Reason = ReasonHTTP
		e.Message = fmt.Sprintf("%s redirected (%d) to %q; set base_url to the final address", url, resp.StatusCode, resp.Header.Get("Location"))
	case resp.StatusCode == http.StatusNotFound:
		e.Reason = ReasonHTTP
		e.Message = fmt.Sprintf("%s not found (404); check base_url", url) + detail
	default:
		e.Reason = ReasonHTTP
		e.Message = fmt.Sprintf("unexpected HTTP %d from %s", resp.StatusCode, url) + detail
	}
	return e
}

func codeSuffix(code string) string {
	if code == "" {
		return ""
	}
	return " " + clean(code)
}

// parseRetryAfter reads delta-seconds or an HTTP date, capped at an hour.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	var d time.Duration
	if n, err := strconv.Atoi(v); err == nil {
		d = time.Duration(n) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = t.Sub(now)
	}
	if d < 0 {
		return 0
	}
	return min(d, maxRetryAfter)
}

// clean makes server-provided text safe for a log line.
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	if r := []rune(s); len(r) > 300 {
		s = string(r[:300]) + "…"
	}
	return strings.TrimSpace(s)
}
