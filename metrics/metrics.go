package metrics

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/pprof"
	"strings"
	"time"

	"github.com/rcrowley/go-metrics"
	log "github.com/sirupsen/logrus"
)

type skipperMetrics map[string]interface{}

// Options for initializing metrics collection.
type Options struct {
	// Network address where the current metrics values
	// can be pulled from. If not set, the collection of
	// the metrics is disabled.
	Listener string

	// Common prefix for the keys of the different
	// collected metrics.
	Prefix string

	// If set, garbage collector metrics are collected
	// in addition to the http traffic metrics.
	EnableDebugGcMetrics bool

	// If set, Go runtime metrics are collected in
	// addition to the http traffic metrics.
	EnableRuntimeMetrics bool

	// If set, detailed total response time metrics will be collected
	// for each route, additionally grouped by status and method.
	EnableServeRouteMetrics bool

	// If set, detailed total response time metrics will be collected
	// for each host, additionally grouped by status and method.
	EnableServeHostMetrics bool

	// If set, detailed response time metrics will be collected
	// for each backend host
	EnableBackendHostMetrics bool

	// EnableProfile exposes profiling information on /pprof of the
	// metrics listener.
	EnableProfile bool
}

const (
	KeyRouteLookup      = "routelookup"
	KeyRouteFailure     = "routefailure"
	KeyFilterRequest    = "filter.%s.request"
	KeyFiltersRequest   = "allfilters.request.%s"
	KeyProxyBackend     = "backend.%s"
	KeyProxyBackendHost = "backendhost.%s"
	KeyFilterResponse   = "filter.%s.response"
	KeyFiltersResponse  = "allfilters.response.%s"
	KeyResponse         = "response.%d.%s.skipper.%s"
	KeyServeRoute       = "serveroute.%s.%s.%d"
	KeyServeHost        = "servehost.%s.%s.%d"

	KeyErrorsBackend   = "errors.backend.%s"
	KeyErrorsStreaming = "errors.streaming.%s"

	statsRefreshDuration = time.Duration(5 * time.Second)

	defaultReservoirSize = 1024
)

type Metrics struct {
	reg           metrics.Registry
	createTimer   func() metrics.Timer
	createCounter func() metrics.Counter
	options       Options
}

var (
	Default *Metrics
	Void    *Metrics
)

func New(o Options) *Metrics {
	m := &Metrics{}
	m.reg = metrics.NewRegistry()
	m.createTimer = createTimer
	m.createCounter = metrics.NewCounter
	m.options = o

	if o.EnableDebugGcMetrics {
		metrics.RegisterDebugGCStats(m.reg)
		go metrics.CaptureDebugGCStats(m.reg, statsRefreshDuration)
	}

	if o.EnableRuntimeMetrics {
		metrics.RegisterRuntimeMemStats(m.reg)
		go metrics.CaptureRuntimeMemStats(m.reg, statsRefreshDuration)
	}

	return m
}

func NewVoid() *Metrics {
	m := &Metrics{}
	m.reg = metrics.NewRegistry()
	m.createTimer = func() metrics.Timer { return metrics.NilTimer{} }
	m.createCounter = func() metrics.Counter { return metrics.NilCounter{} }
	return m
}

func init() {
	Void = NewVoid()
	Default = Void
}

// Initializes the collection of metrics.
func Init(o Options) {
	if o.Listener == "" {
		log.Infoln("Metrics are disabled")
		return
	}

	Default = New(o)

	handler := &metricsHandler{registry: Default.reg, options: o}
	if o.EnableProfile {
		mux := http.NewServeMux()
		mux.Handle("/debug/pprof/", http.HandlerFunc(pprof.Index))
		mux.Handle("/debug/pprof/cmdline", http.HandlerFunc(pprof.Cmdline))
		mux.Handle("/debug/pprof/profile", http.HandlerFunc(pprof.Profile))
		mux.Handle("/debug/pprof/symbol", http.HandlerFunc(pprof.Symbol))
		mux.Handle("/debug/pprof/trace", http.HandlerFunc(pprof.Trace))
		handler.profile = mux
	}

	log.Infof("metrics listener on %s/metrics", o.Listener)
	go http.ListenAndServe(o.Listener, handler)
}

func createTimer() metrics.Timer {
	return metrics.NewCustomTimer(metrics.NewHistogram(metrics.NewUniformSample(defaultReservoirSize)), metrics.NewMeter())
}

func (m *Metrics) getTimer(key string) metrics.Timer {
	return m.reg.GetOrRegister(key, m.createTimer).(metrics.Timer)
}

func (m *Metrics) updateTimer(key string, d time.Duration) {
	if t := m.getTimer(key); t != nil {
		t.Update(d)
	}
}

func (m *Metrics) measureSince(key string, start time.Time) {
	d := time.Since(start)
	go m.updateTimer(key, d)
}

func (m *Metrics) MeasureRouteLookup(start time.Time) {
	m.measureSince(KeyRouteLookup, start)
}

func (m *Metrics) MeasureFilterRequest(filterName string, start time.Time) {
	m.measureSince(fmt.Sprintf(KeyFilterRequest, filterName), start)
}

func (m *Metrics) MeasureAllFiltersRequest(routeId string, start time.Time) {
	m.measureSince(fmt.Sprintf(KeyFiltersRequest, routeId), start)
}

func (m *Metrics) MeasureBackend(routeId string, start time.Time) {
	m.measureSince(fmt.Sprintf(KeyProxyBackend, routeId), start)
}

func (m *Metrics) MeasureBackendHost(routeBackendHost string, start time.Time) {
	if m.options.EnableBackendHostMetrics {
		m.measureSince(fmt.Sprintf(KeyProxyBackendHost, hostForKey(routeBackendHost)), start)
	}
}

func (m *Metrics) MeasureFilterResponse(filterName string, start time.Time) {
	m.measureSince(fmt.Sprintf(KeyFilterResponse, filterName), start)
}

func (m *Metrics) MeasureAllFiltersResponse(routeId string, start time.Time) {
	m.measureSince(fmt.Sprintf(KeyFiltersResponse, routeId), start)
}

func (m *Metrics) MeasureResponse(code int, method string, routeId string, start time.Time) {
	method = measuredMethod(method)
	m.measureSince(fmt.Sprintf(KeyResponse, code, method, routeId), start)
}

func hostForKey(h string) string {
	h = strings.Replace(h, ".", "_", -1)
	h = strings.Replace(h, ":", "__", -1)
	return h
}

func measuredMethod(m string) string {
	switch m {
	case "OPTIONS",
		"GET",
		"HEAD",
		"POST",
		"PUT",
		"DELETE",
		"TRACE",
		"CONNECT":
		return m
	default:
		return "_unknownmethod_"
	}
}

func (m *Metrics) MeasureServe(routeId, host, method string, code int, start time.Time) {
	method = measuredMethod(method)

	if m.options.EnableServeRouteMetrics {
		m.measureSince(fmt.Sprintf(KeyServeRoute, routeId, method, code), start)
	}

	if m.options.EnableServeHostMetrics {
		m.measureSince(fmt.Sprintf(KeyServeHost, hostForKey(host), method, code), start)
	}
}

func (m *Metrics) getCounter(key string) metrics.Counter {
	return m.reg.GetOrRegister(key, m.createCounter).(metrics.Counter)
}

func (m *Metrics) incCounter(key string) {
	go func() {
		if c := m.getCounter(key); c != nil {
			c.Inc(1)
		}
	}()
}

func (m *Metrics) IncRoutingFailures() {
	m.incCounter(KeyRouteFailure)
}

func (m *Metrics) IncErrorsBackend(routeId string) {
	m.incCounter(fmt.Sprintf(KeyErrorsBackend, routeId))
}

func (m *Metrics) IncErrorsStreaming(routeId string) {
	m.incCounter(fmt.Sprintf(KeyErrorsStreaming, routeId))
}

// This listener is used to expose the collected metrics.
func (sm skipperMetrics) MarshalJSON() ([]byte, error) {
	data := make(map[string]map[string]interface{})
	for name, metric := range sm {
		values := make(map[string]interface{})
		var metricsFamily string
		switch m := metric.(type) {
		case metrics.Gauge:
			metricsFamily = "gauges"
			values["value"] = m.Value()
		case metrics.Histogram:
			metricsFamily = "histograms"
			h := m.Snapshot()
			ps := h.Percentiles([]float64{0.5, 0.75, 0.95, 0.99, 0.999})
			values["count"] = h.Count()
			values["min"] = h.Min()
			values["max"] = h.Max()
			values["mean"] = h.Mean()
			values["stddev"] = h.StdDev()
			values["median"] = ps[0]
			values["75%"] = ps[1]
			values["95%"] = ps[2]
			values["99%"] = ps[3]
			values["99.9%"] = ps[4]
		case metrics.Timer:
			metricsFamily = "timers"
			t := m.Snapshot()
			ps := t.Percentiles([]float64{0.5, 0.75, 0.95, 0.99, 0.999})
			values["count"] = t.Count()
			values["min"] = t.Min()
			values["max"] = t.Max()
			values["mean"] = t.Mean()
			values["stddev"] = t.StdDev()
			values["median"] = ps[0]
			values["75%"] = ps[1]
			values["95%"] = ps[2]
			values["99%"] = ps[3]
			values["99.9%"] = ps[4]
			values["1m.rate"] = t.Rate1()
			values["5m.rate"] = t.Rate5()
			values["15m.rate"] = t.Rate15()
			values["mean.rate"] = t.RateMean()
		case metrics.Counter:
			metricsFamily = "counters"
			t := m.Snapshot()
			values["count"] = t.Count()
		default:
			metricsFamily = "unknown"
			values["error"] = fmt.Sprintf("unknown metrics type %T", m)
		}
		if data[metricsFamily] == nil {
			data[metricsFamily] = make(map[string]interface{})
		}
		data[metricsFamily][name] = values
	}

	return json.Marshal(data)
}
