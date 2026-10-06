package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/agentfeedback/agentfeedback/v4/internal/core"
	"github.com/agentfeedback/agentfeedback/v4/internal/store"
)

// Outcomes recorded on submissions_created_total. core does not tell a
// keyed replay from a keyless duplicate on 200, so both are "existing".
const (
	outcomeCreated  = "created"
	outcomeExisting = "existing"
	outcomeMismatch = "mismatch"
	outcomeRejected = "rejected"
)

type metrics struct {
	requests   *prometheus.CounterVec
	duration   *prometheus.HistogramVec
	submission *prometheus.CounterVec
}

// newMetrics registers every metric on registry. Labels are bounded: route is
// the matched ServeMux pattern, method is allow-listed, code is a status code —
// never raw client input.
func newMetrics(registry *prometheus.Registry, svc *core.Service, db *store.DB) *metrics {
	m := &metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total number of HTTP requests, by matched route, method and status code.",
		}, []string{"route", "method", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request duration in seconds, by matched route, method and status code.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route", "method", "code"}),
		submission: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "submissions_created_total",
			Help: "Create attempts by outcome: created (201), existing (200), mismatch (409), rejected (other 4xx).",
		}, []string{"outcome"}),
	}
	registry.MustRegister(m.requests, m.duration, m.submission)
	if svc != nil && db != nil {
		registry.MustRegister(&stateCollector{svc: svc, db: db})
	}
	return m
}

var allowedMethods = map[string]struct{}{
	http.MethodGet: {}, http.MethodPost: {}, http.MethodPut: {}, http.MethodDelete: {},
	http.MethodHead: {}, http.MethodOptions: {}, http.MethodPatch: {},
}

func normalizeMethod(method string) string {
	if _, ok := allowedMethods[method]; ok {
		return method
	}
	return "OTHER"
}

func (m *metrics) observeRequest(route, method string, status int, d time.Duration) {
	labels := prometheus.Labels{
		"route":  route,
		"method": normalizeMethod(method),
		"code":   strconv.Itoa(status),
	}
	m.requests.With(labels).Inc()
	m.duration.With(labels).Observe(d.Seconds())
}

func (m *metrics) observeSubmission(outcome string) {
	m.submission.WithLabelValues(outcome).Inc()
}

// observeCreateError records a failed create: 409 is mismatch, any other
// 4xx rejected; server errors are not an outcome of the submission.
func (m *metrics) observeCreateError(err error) {
	var p *core.Problem
	if !errors.As(err, &p) || p.Status >= 500 {
		return
	}
	if p.Status == http.StatusConflict {
		m.observeSubmission(outcomeMismatch)
		return
	}
	m.observeSubmission(outcomeRejected)
}

// stateCollector reports database state at scrape time, so the numbers are
// never a stale cache.
type stateCollector struct {
	svc *core.Service
	db  *store.DB
}

var (
	unprocessedDesc = prometheus.NewDesc(
		"agentfeedback_submissions_unprocessed",
		"Submissions awaiting processing (processed_at IS NULL).", nil, nil)
	dbBytesDesc = prometheus.NewDesc(
		"agentfeedback_db_bytes",
		"Size in bytes of the SQLite database file plus its write-ahead log.", nil, nil)
	busyDesc = prometheus.NewDesc(
		"agentfeedback_sqlite_busy_total",
		"Write transactions that failed because the database stayed locked past the busy timeout.", nil, nil)
)

func (c *stateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- unprocessedDesc
	ch <- dbBytesDesc
	ch <- busyDesc
}

func (c *stateCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	top := 0
	if st, err := c.svc.Stats(ctx, core.StatsParams{Top: &top}); err == nil {
		ch <- prometheus.MustNewConstMetric(unprocessedDesc, prometheus.GaugeValue, float64(st.Open))
	} else {
		slog.Warn("metrics: unprocessed count unavailable; gauge omitted from this scrape", "error", err)
	}
	ch <- prometheus.MustNewConstMetric(dbBytesDesc, prometheus.GaugeValue, float64(c.db.FileBytes()))
	ch <- prometheus.MustNewConstMetric(busyDesc, prometheus.CounterValue, float64(c.db.BusyWrites()))
}
