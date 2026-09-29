package api

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/agentfeedback/agentfeedback/internal/v3/core"
)

// Outcomes recorded on submissions_created_total.
const (
	outcomeCreated   = "created"
	outcomeReplayed  = "replayed"
	outcomeDuplicate = "duplicate"
	outcomeMismatch  = "mismatch"
	outcomeRejected  = "rejected"
)

type metrics struct {
	requests   *prometheus.CounterVec
	duration   *prometheus.HistogramVec
	submission *prometheus.CounterVec
}

// newMetrics registers every metric on registry. Labels are bounded: route is
// the matched ServeMux pattern, method is allow-listed, code is a status code —
// never raw client input.
func newMetrics(registry *prometheus.Registry, svc *core.Service) *metrics {
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
			Help: "Submission write attempts by family and outcome.",
		}, []string{"family", "outcome"}),
	}

	registry.MustRegister(m.requests, m.duration, m.submission)
	if svc != nil {
		registry.MustRegister(&stateCollector{svc: svc})
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

func (m *metrics) observeSubmission(family, outcome string) {
	m.submission.WithLabelValues(family, outcome).Inc()
}

// stateCollector reports database state at scrape time, so the numbers are
// never a stale cache.
type stateCollector struct {
	svc *core.Service
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

	if n, err := c.svc.CountUnprocessed(ctx); err == nil {
		ch <- prometheus.MustNewConstMetric(unprocessedDesc, prometheus.GaugeValue, float64(n))
	}
	ch <- prometheus.MustNewConstMetric(dbBytesDesc, prometheus.GaugeValue, float64(c.svc.DB().FileBytes()))
	ch <- prometheus.MustNewConstMetric(busyDesc, prometheus.CounterValue, float64(c.svc.DB().BusyWrites()))
}
