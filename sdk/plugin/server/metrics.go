package server

import (
	"sync"
	"time"

	pluginpb "github.com/Tencent/WeKnora/sdk/plugin/proto"
)

// Metric kinds reported through GetMetrics.
const (
	MetricKindCounter   = "counter"
	MetricKindGauge     = "gauge"
	MetricKindHistogram = "histogram"
)

// MetricsRegistry is a thread-safe in-process metric store for plugin
// authors. Counters, gauges, and histograms are exposed to the host through
// the lifecycle GetMetrics RPC — the host polls periodically and surfaces
// samples through its management API.
//
// Usage:
//
//	metrics := pluginsdk.NewMetricsRegistry()
//	metrics.Counter("documents_parsed", map[string]string{"type": "md"}).Add(1)
//	metrics.Gauge("queue_depth").Set(42)
//	metrics.Histogram("parse_seconds").Observe(0.125)
//	// then attach: s.Metrics = metrics
type MetricsRegistry struct {
	mu         sync.Mutex
	counters   map[string]*Counter
	gauges     map[string]*Gauge
	histograms map[string]*Histogram
}

// NewMetricsRegistry returns an empty registry.
func NewMetricsRegistry() *MetricsRegistry {
	return &MetricsRegistry{
		counters:   make(map[string]*Counter),
		gauges:     make(map[string]*Gauge),
		histograms: make(map[string]*Histogram),
	}
}

// Counter returns (creating if needed) the counter identified by name+labels.
func (r *MetricsRegistry) Counter(name string, labels map[string]string) *Counter {
	key := metricKey(name, labels)
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.counters[key]
	if !ok {
		c = &Counter{name: name, labels: labels}
		r.counters[key] = c
	}
	return c
}

// Gauge returns (creating if needed) the gauge identified by name+labels.
func (r *MetricsRegistry) Gauge(name string, labels map[string]string) *Gauge {
	key := metricKey(name, labels)
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok := r.gauges[key]
	if !ok {
		g = &Gauge{name: name, labels: labels}
		r.gauges[key] = g
	}
	return g
}

// Histogram returns (creating if needed) the histogram identified by
// name+labels. Buckets default to {0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5,
// 1, 2.5, 5, 10} seconds; pass custom bounds for other units.
func (r *MetricsRegistry) Histogram(name string, labels map[string]string) *Histogram {
	return r.HistogramWithBounds(name, labels, nil)
}

// HistogramWithBounds is Histogram with explicit bucket upper bounds. Bounds
// must be strictly ascending; the +Inf bucket is appended automatically.
func (r *MetricsRegistry) HistogramWithBounds(name string, labels map[string]string, bounds []float64) *Histogram {
	key := metricKey(name, labels)
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.histograms[key]
	if !ok {
		if len(bounds) == 0 {
			bounds = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
		}
		h = &Histogram{name: name, labels: labels, bounds: bounds, counts: make([]uint64, len(bounds)+1)}
		r.histograms[key] = h
	}
	return h
}

// Snapshot converts every registered metric into a MetricSample. The returned
// slice is freshly allocated so callers own it.
func (r *MetricsRegistry) Snapshot() []*pluginpb.MetricSample {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().UnixMilli()
	samples := make([]*pluginpb.MetricSample, 0, len(r.counters)+len(r.gauges)+len(r.histograms))
	for _, c := range r.counters {
		samples = append(samples, &pluginpb.MetricSample{
			Name:                c.name,
			Kind:                MetricKindCounter,
			Value:               c.value,
			Labels:              c.labels,
			TimestampUnixMillis: now,
		})
	}
	for _, g := range r.gauges {
		samples = append(samples, &pluginpb.MetricSample{
			Name:                g.name,
			Kind:                MetricKindGauge,
			Value:               g.value,
			Labels:              g.labels,
			TimestampUnixMillis: now,
		})
	}
	for _, h := range r.histograms {
		h.mu.Lock()
		counts := append([]uint64(nil), h.counts...)
		sum := h.sum
		count := h.count
		h.mu.Unlock()
		samples = append(samples, &pluginpb.MetricSample{
			Name:                h.name,
			Kind:                MetricKindHistogram,
			Value:               sum,
			Labels:              h.labels,
			TimestampUnixMillis: now,
			BucketBounds:        append([]float64(nil), h.bounds...),
			BucketCounts:        counts,
		})
		_ = count
	}
	return samples
}

// Counter is a monotonically increasing value.
type Counter struct {
	mu     sync.Mutex
	name   string
	labels map[string]string
	value  float64
}

// Add increments the counter by v (negative values are ignored — counters
// only go up).
func (c *Counter) Add(v float64) {
	if v < 0 {
		return
	}
	c.mu.Lock()
	c.value += v
	c.mu.Unlock()
}

// Gauge is a point-in-time value.
type Gauge struct {
	mu     sync.Mutex
	name   string
	labels map[string]string
	value  float64
}

// Set overwrites the gauge value.
func (g *Gauge) Set(v float64) {
	g.mu.Lock()
	g.value = v
	g.mu.Unlock()
}

// Histogram records a distribution of observations.
type Histogram struct {
	mu     sync.Mutex
	name   string
	labels map[string]string
	bounds []float64
	counts []uint64
	sum    float64
	count  uint64
}

// Observe records one value into the matching bucket.
func (h *Histogram) Observe(v float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	idx := len(h.counts) - 1 // +Inf bucket
	for i, b := range h.bounds {
		if v <= b {
			idx = i
			break
		}
	}
	h.counts[idx]++
	h.sum += v
	h.count++
}

func metricKey(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	key := name
	for k, v := range labels {
		key += "\x00" + k + "\x01" + v
	}
	return key
}
