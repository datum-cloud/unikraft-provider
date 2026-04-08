package activator

import "github.com/prometheus/client_golang/prometheus"

var (
	coldStartDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "ufo_activator_cold_start_duration_seconds",
			Help:    "Duration of cold start from first request to successful proxy, in seconds.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"function_namespace", "function_name"},
	)

	requestQueueDepth = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "ufo_activator_request_queue_depth",
			Help: "Current number of requests buffered per function.",
		},
		[]string{"function_namespace", "function_name"},
	)

	timeoutTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ufo_activator_timeout_total",
			Help: "Total number of requests that timed out waiting for a cold-start.",
		},
		[]string{"function_namespace", "function_name"},
	)

	queueFullTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ufo_activator_queue_full_total",
			Help: "Total number of requests rejected because the per-function queue was full.",
		},
		[]string{"function_namespace", "function_name"},
	)
)

// RegisterMetrics registers all activator metrics with the given registerer.
func RegisterMetrics(reg prometheus.Registerer) error {
	for _, c := range []prometheus.Collector{
		coldStartDuration,
		requestQueueDepth,
		timeoutTotal,
		queueFullTotal,
	} {
		if err := reg.Register(c); err != nil {
			return err
		}
	}
	return nil
}
