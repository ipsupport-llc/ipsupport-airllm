// Package metrics exposes Prometheus instrumentation for the gateway on a
// dedicated registry. All helper methods are nil-safe so a Server built without
// metrics (in tests) never panics.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds the gateway's collectors and their registry.
type Metrics struct {
	reg          *prometheus.Registry
	httpRequests *prometheus.CounterVec
	httpDuration *prometheus.HistogramVec
	component    *prometheus.HistogramVec
	tokens       *prometheus.CounterVec
	cost         *prometheus.CounterVec
	rateLimited  *prometheus.CounterVec
	dlpSkipped   *prometheus.CounterVec
	dlpDuration  prometheus.Histogram
}

// New builds and registers the collectors on a fresh registry.
func New() *Metrics {
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "airllm_http_requests_total", Help: "Total HTTP requests by ingress and status.",
		}, []string{"ingress", "status"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "airllm_http_request_duration_seconds", Help: "HTTP request duration by ingress.",
			Buckets: prometheus.DefBuckets,
		}, []string{"ingress"}),
		component: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "airllm_component_duration_seconds", Help: "Per-component latency.",
			Buckets: prometheus.DefBuckets,
		}, []string{"component"}),
		tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "airllm_tokens_total", Help: "Tokens metered by ingress and direction.",
		}, []string{"ingress", "direction"}),
		cost: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "airllm_cost_usd_total", Help: "Cost in USD metered by ingress.",
		}, []string{"ingress"}),
		rateLimited: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "airllm_rate_limited_total", Help: "Requests rejected with 429 by reason.",
		}, []string{"reason"}),
		dlpSkipped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "airllm_dlp_model_skipped_total", Help: "DLP model scans skipped by reason (e.g. all_busy).",
		}, []string{"reason"}),
		dlpDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "airllm_dlp_model_duration_seconds", Help: "DLP model (BERT) scan duration.",
			Buckets: prometheus.DefBuckets,
		}),
	}
	m.reg.MustRegister(m.httpRequests, m.httpDuration, m.component, m.tokens, m.cost, m.rateLimited, m.dlpSkipped, m.dlpDuration)
	return m
}

// Handler serves the registry in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

func (m *Metrics) RecordRequest(ingress string, status int, d time.Duration) {
	if m == nil {
		return
	}
	m.httpRequests.WithLabelValues(ingress, strconv.Itoa(status)).Inc()
	m.httpDuration.WithLabelValues(ingress).Observe(d.Seconds())
}

func (m *Metrics) ObserveComponent(component string, d time.Duration) {
	if m == nil {
		return
	}
	m.component.WithLabelValues(component).Observe(d.Seconds())
}

func (m *Metrics) RecordUsage(ingress string, prompt, completion int, cost float64) {
	if m == nil {
		return
	}
	m.tokens.WithLabelValues(ingress, "prompt").Add(float64(prompt))
	m.tokens.WithLabelValues(ingress, "completion").Add(float64(completion))
	m.cost.WithLabelValues(ingress).Add(cost)
}

func (m *Metrics) IncRateLimited(reason string) {
	if m == nil {
		return
	}
	m.rateLimited.WithLabelValues(reason).Inc()
}

func (m *Metrics) DLPModelObserve(d time.Duration) {
	if m == nil {
		return
	}
	m.dlpDuration.Observe(d.Seconds())
}

func (m *Metrics) DLPModelSkipped(reason string) {
	if m == nil {
		return
	}
	m.dlpSkipped.WithLabelValues(reason).Inc()
}

// RegisterModelInflight registers a gauge reading the live in-flight DLP model
// (BERT) scan count from fn (sourced from the model pool).
func (m *Metrics) RegisterModelInflight(fn func() float64) {
	if m == nil {
		return
	}
	m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "airllm_dlp_model_requests_inflight", Help: "In-flight DLP model (BERT) scans across the pool.",
	}, fn))
}

// RegisterModelEndpoints registers a gauge reading the resolved sidecar
// endpoint count from fn.
func (m *Metrics) RegisterModelEndpoints(fn func() float64) {
	if m == nil {
		return
	}
	m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "airllm_dlp_model_endpoints", Help: "Resolved DLP model (BERT) sidecar endpoints in the pool.",
	}, fn))
}

// RegisterCaptureDropped registers a gauge that reads the capture pipeline's
// cumulative dropped count from fn.
func (m *Metrics) RegisterCaptureDropped(fn func() float64) {
	if m == nil {
		return
	}
	m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "airllm_capture_dropped", Help: "Capture records dropped due to a full buffer.",
	}, fn))
}
