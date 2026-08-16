// Package observability owns the Prometheus registry and the metric set
// described in §35, plus the health/readiness probe contract from §36.
package observability

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Metrics is the process-wide metric set. It is created once in main and
// threaded through the services that record into it.
type Metrics struct {
	Registry *prometheus.Registry

	HTTPRequests     *prometheus.CounterVec
	HTTPDuration     *prometheus.HistogramVec
	HTTPInFlight     prometheus.Gauge
	HTTPResponseSize *prometheus.HistogramVec

	WSConnections   *prometheus.GaugeVec
	WSEventsSent    *prometheus.CounterVec
	WSEventsRecv    *prometheus.CounterVec
	WSDeliveryDelay prometheus.Histogram
	WSSendErrors    *prometheus.CounterVec

	MessagesSent       *prometheus.CounterVec
	MessageSendLatency prometheus.Histogram

	DBQueryDuration *prometheus.HistogramVec
	DBErrors        *prometheus.CounterVec

	QueuePublished *prometheus.CounterVec
	QueueConsumed  *prometheus.CounterVec
	QueueFailures  *prometheus.CounterVec

	MediaUploads    *prometheus.CounterVec
	MediaBytes      *prometheus.CounterVec
	MediaProcessing *prometheus.HistogramVec

	PushDeliveries *prometheus.CounterVec
	RateLimitHits  *prometheus.CounterVec
	AuthAttempts   *prometheus.CounterVec
}

// New builds the metric set and registers it, including Go runtime and process
// collectors so CPU/RAM appear on the same scrape (§35).
func New(serviceName string) *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	m := &Metrics{
		Registry: reg,

		HTTPRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sobh_http_requests_total",
			Help: "HTTP requests by route, method and status class.",
		}, []string{"route", "method", "status"}),

		HTTPDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "sobh_http_request_duration_seconds",
			Help:    "HTTP request latency. Buckets straddle the 200ms p95 target (§79).",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.2, 0.3, 0.5, 1, 2.5, 5, 10},
		}, []string{"route", "method"}),

		HTTPInFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "sobh_http_in_flight_requests",
			Help: "HTTP requests currently being served.",
		}),

		HTTPResponseSize: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "sobh_http_response_bytes",
			Help:    "HTTP response body size.",
			Buckets: prometheus.ExponentialBuckets(128, 4, 8),
		}, []string{"route"}),

		WSConnections: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sobh_ws_connections",
			Help: "Live WebSocket connections by node.",
		}, []string{"node"}),

		WSEventsSent: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sobh_ws_events_sent_total",
			Help: "WebSocket events pushed to clients by type.",
		}, []string{"event"}),

		WSEventsRecv: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sobh_ws_events_received_total",
			Help: "WebSocket frames received from clients by type.",
		}, []string{"event"}),

		WSDeliveryDelay: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "sobh_ws_delivery_delay_seconds",
			Help:    "Time from message persistence to client delivery (target <500ms, §79).",
			Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.2, 0.3, 0.5, 1, 2, 5},
		}),

		WSSendErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sobh_ws_send_errors_total",
			Help: "Failed WebSocket writes by reason.",
		}, []string{"reason"}),

		MessagesSent: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sobh_messages_sent_total",
			Help: "Messages accepted by chat type and message type.",
		}, []string{"chat_type", "message_type"}),

		MessageSendLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "sobh_message_send_seconds",
			Help:    "Server-side send-to-acknowledgement latency (target <300ms, §79).",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.2, 0.3, 0.5, 1, 2},
		}),

		DBQueryDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "sobh_db_query_seconds",
			Help:    "Database query latency by operation.",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 5},
		}, []string{"operation"}),

		DBErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sobh_db_errors_total",
			Help: "Database errors by operation.",
		}, []string{"operation"}),

		QueuePublished: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sobh_queue_published_total",
			Help: "Messages published to the queue by subject.",
		}, []string{"subject"}),

		QueueConsumed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sobh_queue_consumed_total",
			Help: "Messages consumed from the queue by subject.",
		}, []string{"subject"}),

		QueueFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sobh_queue_failures_total",
			Help: "Queue handler failures by subject.",
		}, []string{"subject"}),

		MediaUploads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sobh_media_uploads_total",
			Help: "Completed media uploads by kind and outcome.",
		}, []string{"kind", "outcome"}),

		MediaBytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sobh_media_bytes_total",
			Help: "Bytes stored by media kind.",
		}, []string{"kind"}),

		MediaProcessing: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "sobh_media_processing_seconds",
			Help:    "Worker time spent producing derived media variants.",
			Buckets: prometheus.ExponentialBuckets(0.1, 2, 10),
		}, []string{"kind"}),

		PushDeliveries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sobh_push_deliveries_total",
			Help: "Push notification attempts by provider and outcome.",
		}, []string{"provider", "outcome"}),

		RateLimitHits: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sobh_rate_limit_hits_total",
			Help: "Requests rejected by a rate limiter, by limiter name.",
		}, []string{"limiter"}),

		AuthAttempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sobh_auth_attempts_total",
			Help: "Authentication attempts by stage and outcome.",
		}, []string{"stage", "outcome"}),
	}

	reg.MustRegister(
		m.HTTPRequests, m.HTTPDuration, m.HTTPInFlight, m.HTTPResponseSize,
		m.WSConnections, m.WSEventsSent, m.WSEventsRecv, m.WSDeliveryDelay, m.WSSendErrors,
		m.MessagesSent, m.MessageSendLatency,
		m.DBQueryDuration, m.DBErrors,
		m.QueuePublished, m.QueueConsumed, m.QueueFailures,
		m.MediaUploads, m.MediaBytes, m.MediaProcessing,
		m.PushDeliveries, m.RateLimitHits, m.AuthAttempts,
	)

	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "sobh_build_info",
		Help: "Build information for the running service.",
	}, []string{"service"})
	reg.MustRegister(buildInfo)
	buildInfo.WithLabelValues(serviceName).Set(1)

	return m
}
