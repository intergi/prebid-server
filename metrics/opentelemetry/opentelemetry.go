package opentelemetry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/prebid/prebid-server/v4/config"
	"github.com/prebid/prebid-server/v4/logger"
	"github.com/prebid/prebid-server/v4/metrics"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
	"github.com/prebid/prebid-server/v4/version"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	otelmetric "go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

const (
	accountLabel = "account"
	adapterLabel = "adapter"
	cookieLabel  = "cookie"
	statusLabel  = "status"
	successLabel = "success"
)

// Metrics implements the shared metrics interface using OTLP/HTTP.
type Metrics struct {
	provider *sdkmetric.MeterProvider
	meter    otelmetric.Meter
	ctx      context.Context
	prefix   string
	disabled config.DisabledMetrics

	instrumentMu sync.RWMutex
	accountMu    sync.Mutex
	counters     map[string]otelmetric.Int64Counter
	histograms   map[string]otelmetric.Float64Histogram
	accounts     map[string]struct{}
	accountLimit int
}

var _ metrics.MetricsEngine = (*Metrics)(nil)

// NewMetrics creates an OTLP/HTTP metrics backend. The exporter runs on a
// periodic reader, so recording never performs network I/O.
func NewMetrics(cfg config.OpenTelemetryMetrics, prometheusCfg config.PrometheusMetrics, disabled config.DisabledMetrics, moduleStageNames map[string][]string) (*Metrics, error) {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint != "" && !strings.Contains(endpoint, "://") {
		scheme := "http"
		if cfg.TLS.CAFile != "" || cfg.TLS.InsecureSkipVerify {
			scheme = "https"
		}
		endpoint = scheme + "://" + endpoint
	}

	options := make([]otlpmetrichttp.Option, 0, 4)
	if endpoint != "" {
		options = append(options, otlpmetrichttp.WithEndpointURL(endpoint))
	}
	if len(cfg.Headers) > 0 {
		options = append(options, otlpmetrichttp.WithHeaders(cfg.Headers))
	}
	if cfg.TLS.CAFile != "" || cfg.TLS.InsecureSkipVerify {
		tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: cfg.TLS.InsecureSkipVerify}
		if cfg.TLS.CAFile != "" {
			pem, err := os.ReadFile(cfg.TLS.CAFile)
			if err != nil {
				return nil, fmt.Errorf("read OpenTelemetry CA file: %w", err)
			}
			roots, err := x509.SystemCertPool()
			if err != nil {
				return nil, fmt.Errorf("load system certificate pool: %w", err)
			}
			if roots == nil {
				roots = x509.NewCertPool()
			}
			if !roots.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("OpenTelemetry CA file %q contains no valid certificates", cfg.TLS.CAFile)
			}
			tlsConfig.RootCAs = roots
		}
		options = append(options, otlpmetrichttp.WithTLSClientConfig(tlsConfig))
	}
	if cfg.TimeoutMillis > 0 {
		options = append(options, otlpmetrichttp.WithTimeout(time.Duration(cfg.TimeoutMillis)*time.Millisecond))
	}
	if cfg.Temporality == "delta" {
		options = append(options, otlpmetrichttp.WithTemporalitySelector(sdkmetric.DeltaTemporalitySelector))
	}

	exporter, err := otlpmetrichttp.New(context.Background(), options...)
	if err != nil {
		return nil, fmt.Errorf("initialize OpenTelemetry OTLP/HTTP exporter: %w", err)
	}

	res, err := newResource(cfg)
	if err != nil {
		return nil, fmt.Errorf("initialize OpenTelemetry resource: %w", err)
	}
	interval := time.Duration(cfg.MetricSendInterval) * time.Second
	timeout := time.Duration(cfg.TimeoutMillis) * time.Millisecond
	reader := sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(interval), sdkmetric.WithTimeout(timeout))
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithResource(res))
	if err := runtime.Start(runtime.WithMeterProvider(provider)); err != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if shutdownErr := provider.Shutdown(shutdownCtx); shutdownErr != nil {
			logger.Errorf("Failed to shut down OpenTelemetry metrics provider after runtime instrumentation initialization failed: %v", shutdownErr)
		}
		return nil, fmt.Errorf("initialize OpenTelemetry runtime metrics: %w", err)
	}

	return newMetrics(provider, cfg, prometheusCfg, disabled, moduleStageNames), nil
}

func newMetrics(provider *sdkmetric.MeterProvider, cfg config.OpenTelemetryMetrics, prometheusCfg config.PrometheusMetrics, disabled config.DisabledMetrics, moduleStageNames map[string][]string) *Metrics {
	prefix := ""
	if prometheusCfg.Namespace != "" {
		prefix += prometheusCfg.Namespace + "_"
	}
	if prometheusCfg.Subsystem != "" {
		prefix += prometheusCfg.Subsystem + "_"
	}
	m := &Metrics{
		provider:     provider,
		meter:        provider.Meter("github.com/prebid/prebid-server/v4/metrics/opentelemetry"),
		ctx:          context.Background(),
		prefix:       prefix,
		disabled:     disabled,
		counters:     make(map[string]otelmetric.Int64Counter),
		histograms:   make(map[string]otelmetric.Float64Histogram),
		accounts:     make(map[string]struct{}),
		accountLimit: cfg.AccountLimit,
	}
	for _, name := range []string{
		"connections_closed", "connections_error", "connections_opened", "tmax_timeout",
		"cookie_sync_requests", "setuid_requests", "impressions_requests", "requests",
		"debug_requests", "requests_without_cookie", "stored_impressions_cache_performance",
		"stored_request_cache_performance", "account_cache_performance", "stored_account_errors",
		"stored_amp_errors", "stored_category_errors", "stored_request_errors", "stored_video_errors",
		"timeout_notification", "privacy_ccpa", "privacy_coppa", "privacy_tcf", "privacy_lmt",
		"stored_response_errors", "stored_responses", "gvl_requests", "live_gvl_fetch", "adapter_bids",
		"adapter_errors", "adapter_panics", "adapter_requests", "adapter_connection_created",
		"adapter_connection_reused", "adapter_connection_dial_errors", "adapter_response_validation_size_err",
		"adapter_response_validation_size_warn", "adapter_response_validation_secure_err",
		"adapter_response_validation_secure_warn", "adapter_throttled", "syncer_requests", "syncer_sets",
		"account_requests", "account_debug_requests", "account_stored_responses",
		"account_response_validation_size_err", "account_response_validation_size_warn",
		"account_response_validation_secure_err", "account_response_validation_secure_warn",
		"ads_cert_requests", "adapter_buyeruids_scrubbed", "adapter_gdpr_requests_blocked",
	} {
		m.getCounter(name)
	}
	for _, name := range []string{
		"request_size_bytes", "request_time_seconds", "request_queue_time", "prebidcache_write_time_seconds",
		"stored_account_fetch_time_seconds", "stored_amp_fetch_time_seconds", "stored_category_fetch_time_seconds",
		"stored_request_fetch_time_seconds", "stored_video_fetch_time_seconds", "stored_response_fetch_time_seconds",
		"dns_lookup_time", "tls_handshake_time", "bidder_server_response_time_seconds", "adapter_prices",
		"adapter_connection_wait", "adapter_connection_dial_time", "adapter_request_time_seconds", "overhead_time_seconds",
		"ads_cert_sign_time",
	} {
		m.histogram(name)
	}
	for module := range moduleStageNames {
		for _, suffix := range []string{"called", "failed", "success_noops", "success_updates", "success_rejects", "execution_errors", "timeouts"} {
			m.counter("modules_"+module+"_"+suffix, nil)
		}
		m.histogram("modules_" + module + "_duration")
	}
	return m
}

func newResource(cfg config.OpenTelemetryMetrics) (*resource.Resource, error) {
	attrs := make([]attribute.KeyValue, 0, len(cfg.ResourceAttributes)+3)
	serviceName := cfg.ServiceName
	if serviceName == "" {
		serviceName = "prebid-server"
	}
	serviceVersion := cfg.ServiceVersion
	if serviceVersion == "" {
		serviceVersion = version.Ver
	}
	instanceID := cfg.InstanceID
	if instanceID == "" {
		var err error
		instanceID, err = os.Hostname()
		if err != nil {
			return nil, fmt.Errorf("determine instance identity: %w", err)
		}
	}
	attrs = append(attrs,
		attribute.String("service.name", serviceName),
		attribute.String("service.version", serviceVersion),
		attribute.String("service.instance.id", instanceID),
	)
	for key, value := range cfg.ResourceAttributes {
		attrs = append(attrs, attribute.String(key, value))
	}
	return resource.New(context.Background(), resource.WithFromEnv(), resource.WithAttributes(attrs...))
}

func (m *Metrics) Shutdown(ctx context.Context) error {
	return m.provider.Shutdown(ctx)
}

func (m *Metrics) metricName(name string) string {
	return m.prefix + name
}

func (m *Metrics) counter(name string, attrs []attribute.KeyValue) {
	counter := m.getCounter(name)
	if counter != nil {
		counter.Add(m.ctx, 1, otelmetric.WithAttributes(attrs...))
	}
}

func (m *Metrics) getCounter(name string) otelmetric.Int64Counter {
	m.instrumentMu.RLock()
	counter, ok := m.counters[name]
	m.instrumentMu.RUnlock()
	if ok {
		return counter
	}
	m.instrumentMu.Lock()
	defer m.instrumentMu.Unlock()
	if counter, ok = m.counters[name]; ok {
		return counter
	}
	counter, err := m.meter.Int64Counter(m.metricName(name))
	if err != nil {
		logger.Errorf("Failed to create OpenTelemetry counter %s: %v", m.metricName(name), err)
		return nil
	}
	m.counters[name] = counter
	return counter
}

func (m *Metrics) add(name string, value int64, attrs []attribute.KeyValue) {
	counter := m.getCounter(name)
	if counter != nil {
		counter.Add(m.ctx, value, otelmetric.WithAttributes(attrs...))
	}
}

func (m *Metrics) histogram(name string) otelmetric.Float64Histogram {
	m.instrumentMu.RLock()
	if instrument, ok := m.histograms[name]; ok {
		m.instrumentMu.RUnlock()
		return instrument
	}
	m.instrumentMu.RUnlock()
	m.instrumentMu.Lock()
	defer m.instrumentMu.Unlock()
	if instrument, ok := m.histograms[name]; ok {
		return instrument
	}
	options := []otelmetric.Float64HistogramOption{}
	if strings.HasSuffix(name, "_time_seconds") || strings.HasSuffix(name, "_time") || strings.HasSuffix(name, "_wait") || strings.HasSuffix(name, "_duration") {
		options = append(options, otelmetric.WithUnit("s"))
	} else if strings.HasSuffix(name, "_bytes") {
		options = append(options, otelmetric.WithUnit("By"))
	}
	options = append(options, otelmetric.WithExplicitBucketBoundaries(histogramBoundaries(name)...))
	instrument, err := m.meter.Float64Histogram(m.metricName(name), options...)
	if err != nil {
		logger.Errorf("Failed to create OpenTelemetry histogram %s: %v", m.metricName(name), err)
		return nil
	}
	m.histograms[name] = instrument
	return instrument
}

func (m *Metrics) observe(name string, value float64, attrs []attribute.KeyValue) {
	histogram := m.histogram(name)
	if histogram != nil {
		histogram.Record(m.ctx, value, otelmetric.WithAttributes(attrs...))
	}
}

func histogramBoundaries(name string) []float64 {
	switch name {
	case "adapter_prices":
		return []float64{0.01, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 25, 50, 100, 250, 500, 1000, 4000}
	case "request_size_bytes":
		return []float64{100, 500, 750, 1000, 2000, 4000, 7000, 10000, 15000, 20000, 50000, 75000}
	case "prebidcache_write_time_seconds":
		return []float64{0.001, 0.002, 0.005, 0.01, 0.025, 0.05, 0.1, 0.2, 0.3, 0.4, 0.5, 1}
	case "request_queue_time":
		return []float64{0, 1, 5, 30, 60, 120, 180, 240, 300}
	case "overhead_time_seconds":
		return []float64{0.05, 0.06, 0.07, 0.08, 0.09, 0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1}
	case "adapter_connection_dial_time":
		return []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 15, 30}
	default:
		return []float64{0.05, 0.1, 0.15, 0.2, 0.25, 0.3, 0.4, 0.5, 0.75, 1}
	}
}

func str(key, value string) attribute.KeyValue {
	return attribute.String(key, value)
}

func status(success bool) string {
	if success {
		return "ok"
	}
	return "failed"
}

func adapterName(name openrtb_ext.BidderName) string {
	return strings.ToLower(string(name))
}

func (m *Metrics) accountAttribute(id string) attribute.KeyValue {
	if id == metrics.PublisherUnknown {
		return str(accountLabel, metrics.PublisherUnknown)
	}
	m.accountMu.Lock()
	defer m.accountMu.Unlock()
	if _, ok := m.accounts[id]; ok {
		return str(accountLabel, id)
	}
	if len(m.accounts) < m.accountLimit {
		m.accounts[id] = struct{}{}
		return str(accountLabel, id)
	}
	return str(accountLabel, "overflow")
}

func (m *Metrics) RecordConnectionAccept(success bool) {
	if success {
		m.counter("connections_opened", nil)
	} else {
		m.counter("connections_error", []attribute.KeyValue{str("connection_error", "accept")})
	}
}

func (m *Metrics) RecordTMaxTimeout() { m.counter("tmax_timeout", nil) }

func (m *Metrics) RecordConnectionClose(success bool) {
	if success {
		m.counter("connections_closed", nil)
	} else {
		m.counter("connections_error", []attribute.KeyValue{str("connection_error", "close")})
	}
}

func (m *Metrics) RecordRequest(labels metrics.Labels) {
	m.counter("requests", []attribute.KeyValue{str("request_type", string(labels.RType)), str("request_status", string(labels.RequestStatus))})
	if labels.RequestSize > 0 && labels.RType != metrics.ReqTypeAMP {
		m.observe("request_size_bytes", float64(labels.RequestSize), []attribute.KeyValue{str("request_size", string(metrics.GetEndpointFromRequestType(labels.RType)))})
	}
	if labels.CookieFlag == metrics.CookieFlagNo {
		m.counter("requests_without_cookie", []attribute.KeyValue{str("request_type", string(labels.RType))})
	}
	if labels.PubID != metrics.PublisherUnknown {
		m.counter("account_requests", []attribute.KeyValue{m.accountAttribute(labels.PubID)})
	}
}

func (m *Metrics) RecordImps(labels metrics.ImpLabels) {
	m.counter("impressions_requests", []attribute.KeyValue{
		attribute.Bool("banner", labels.BannerImps), attribute.Bool("video", labels.VideoImps),
		attribute.Bool("audio", labels.AudioImps), attribute.Bool("native", labels.NativeImps),
	})
}

func (m *Metrics) RecordRequestTime(labels metrics.Labels, length time.Duration) {
	if labels.RequestStatus == metrics.RequestStatusOK {
		m.observe("request_time_seconds", length.Seconds(), []attribute.KeyValue{str("request_type", string(labels.RType))})
	}
}

func (m *Metrics) RecordOverheadTime(overhead metrics.OverheadType, length time.Duration) {
	m.observe("overhead_time_seconds", length.Seconds(), []attribute.KeyValue{str("overhead_type", overhead.String())})
}

func (m *Metrics) RecordAdapterRequest(labels metrics.AdapterLabels) {
	m.counter("adapter_requests", []attribute.KeyValue{
		str(adapterLabel, adapterName(labels.Adapter)), str(cookieLabel, string(labels.CookieFlag)),
		attribute.Bool("has_bids", labels.AdapterBids == metrics.AdapterBidPresent),
	})
	for adapterError := range labels.AdapterErrors {
		m.counter("adapter_errors", []attribute.KeyValue{str(adapterLabel, adapterName(labels.Adapter)), str("adapter_error", string(adapterError))})
	}
}

func (m *Metrics) RecordAdapterConnections(name openrtb_ext.BidderName, reused bool, wait time.Duration) {
	if m.disabled.AdapterConnectionMetrics {
		return
	}
	if reused {
		m.counter("adapter_connection_reused", []attribute.KeyValue{str(adapterLabel, adapterName(name))})
	} else {
		m.counter("adapter_connection_created", []attribute.KeyValue{str(adapterLabel, adapterName(name))})
	}
	m.observe("adapter_connection_wait", wait.Seconds(), []attribute.KeyValue{str(adapterLabel, adapterName(name))})
}

func (m *Metrics) RecordDNSTime(duration time.Duration) {
	m.observe("dns_lookup_time", duration.Seconds(), nil)
}

func (m *Metrics) RecordTLSHandshakeTime(duration time.Duration) {
	m.observe("tls_handshake_time", duration.Seconds(), nil)
}

func (m *Metrics) RecordBidderServerResponseTime(duration time.Duration) {
	m.observe("bidder_server_response_time_seconds", duration.Seconds(), nil)
}

func (m *Metrics) RecordAdapterPanic(labels metrics.AdapterLabels) {
	m.counter("adapter_panics", []attribute.KeyValue{str(adapterLabel, adapterName(labels.Adapter))})
}

func (m *Metrics) RecordAdapterBidReceived(labels metrics.AdapterLabels, _ openrtb_ext.BidType, hasAdm bool) {
	delivery := "nurl"
	if hasAdm {
		delivery = "adm"
	}
	m.counter("adapter_bids", []attribute.KeyValue{str(adapterLabel, adapterName(labels.Adapter)), str("delivery", delivery)})
}

func (m *Metrics) RecordAdapterPrice(labels metrics.AdapterLabels, cpm float64) {
	m.observe("adapter_prices", cpm, []attribute.KeyValue{str(adapterLabel, adapterName(labels.Adapter))})
}

func (m *Metrics) RecordAdapterTime(labels metrics.AdapterLabels, duration time.Duration) {
	if len(labels.AdapterErrors) == 0 {
		m.observe("adapter_request_time_seconds", duration.Seconds(), []attribute.KeyValue{str(adapterLabel, adapterName(labels.Adapter))})
	}
}

func (m *Metrics) RecordCookieSync(syncStatus metrics.CookieSyncStatus) {
	m.counter("cookie_sync_requests", []attribute.KeyValue{str(statusLabel, string(syncStatus))})
}

func (m *Metrics) RecordSyncerRequest(key string, syncStatus metrics.SyncerCookieSyncStatus) {
	m.counter("syncer_requests", []attribute.KeyValue{str("syncer", key), str(statusLabel, string(syncStatus))})
}

func (m *Metrics) RecordSetUid(setStatus metrics.SetUidStatus) {
	m.counter("setuid_requests", []attribute.KeyValue{str(statusLabel, string(setStatus))})
}

func (m *Metrics) RecordSyncerSet(key string, setStatus metrics.SyncerSetUidStatus) {
	m.counter("syncer_sets", []attribute.KeyValue{str("syncer", key), str(statusLabel, string(setStatus))})
}

func (m *Metrics) RecordStoredReqCacheResult(result metrics.CacheResult, inc int) {
	m.add("stored_request_cache_performance", int64(inc), []attribute.KeyValue{str("cache_result", string(result))})
}

func (m *Metrics) RecordStoredImpCacheResult(result metrics.CacheResult, inc int) {
	m.add("stored_impressions_cache_performance", int64(inc), []attribute.KeyValue{str("cache_result", string(result))})
}

func (m *Metrics) RecordAccountCacheResult(result metrics.CacheResult, inc int) {
	m.add("account_cache_performance", int64(inc), []attribute.KeyValue{str("cache_result", string(result))})
}

func (m *Metrics) RecordStoredDataFetchTime(labels metrics.StoredDataLabels, duration time.Duration) {
	name := map[metrics.StoredDataType]string{
		metrics.AccountDataType:  "stored_account_fetch_time_seconds",
		metrics.AMPDataType:      "stored_amp_fetch_time_seconds",
		metrics.CategoryDataType: "stored_category_fetch_time_seconds",
		metrics.RequestDataType:  "stored_request_fetch_time_seconds",
		metrics.VideoDataType:    "stored_video_fetch_time_seconds",
		metrics.ResponseDataType: "stored_response_fetch_time_seconds",
	}[labels.DataType]
	if name != "" {
		m.observe(name, duration.Seconds(), []attribute.KeyValue{str("stored_data_fetch_type", string(labels.DataFetchType))})
	}
}

func (m *Metrics) RecordStoredDataError(labels metrics.StoredDataLabels) {
	name := map[metrics.StoredDataType]string{
		metrics.AccountDataType:  "stored_account_errors",
		metrics.AMPDataType:      "stored_amp_errors",
		metrics.CategoryDataType: "stored_category_errors",
		metrics.RequestDataType:  "stored_request_errors",
		metrics.VideoDataType:    "stored_video_errors",
		metrics.ResponseDataType: "stored_response_errors",
	}[labels.DataType]
	if name != "" {
		m.counter(name, []attribute.KeyValue{str("stored_data_error", string(labels.Error))})
	}
}

func (m *Metrics) RecordPrebidCacheRequestTime(success bool, duration time.Duration) {
	m.observe("prebidcache_write_time_seconds", duration.Seconds(), []attribute.KeyValue{attribute.Bool(successLabel, success)})
}

func (m *Metrics) RecordRequestQueueTime(success bool, requestType metrics.RequestType, duration time.Duration) {
	result := "requestRejectedLabel"
	if success {
		result = "requestAcceptedLabel"
	}
	m.observe("request_queue_time", duration.Seconds(), []attribute.KeyValue{
		str("request_type", string(requestType)), str("request_status", result),
	})
}

func (m *Metrics) RecordTimeoutNotice(success bool) {
	m.counter("timeout_notification", []attribute.KeyValue{str(successLabel, status(success))})
}

func (m *Metrics) RecordRequestPrivacy(privacy metrics.PrivacyLabels) {
	if privacy.CCPAProvided {
		m.counter("privacy_ccpa", []attribute.KeyValue{str("source", "request"), attribute.Bool("opt_out", privacy.CCPAEnforced)})
	}
	if privacy.COPPAEnforced {
		m.counter("privacy_coppa", []attribute.KeyValue{str("source", "request")})
	}
	if privacy.GDPREnforced {
		m.counter("privacy_tcf", []attribute.KeyValue{str("version", string(privacy.GDPRTCFVersion)), str("source", "request")})
	}
	if privacy.LMTEnforced {
		m.counter("privacy_lmt", []attribute.KeyValue{str("source", "request")})
	}
}

func (m *Metrics) RecordAdapterBuyerUIDScrubbed(name openrtb_ext.BidderName) {
	if !m.disabled.AdapterBuyerUIDScrubbed {
		m.counter("adapter_buyeruids_scrubbed", []attribute.KeyValue{str(adapterLabel, adapterName(name))})
	}
}

func (m *Metrics) RecordAdapterGDPRRequestBlocked(name openrtb_ext.BidderName) {
	if !m.disabled.AdapterGDPRRequestBlocked {
		m.counter("adapter_gdpr_requests_blocked", []attribute.KeyValue{str(adapterLabel, adapterName(name))})
	}
}

func (m *Metrics) RecordDebugRequest(debugEnabled bool, pubID string) {
	if debugEnabled {
		m.counter("debug_requests", nil)
		if !m.disabled.AccountDebug && pubID != metrics.PublisherUnknown {
			m.counter("account_debug_requests", []attribute.KeyValue{m.accountAttribute(pubID)})
		}
	}
}

func (m *Metrics) RecordStoredResponse(pubID string) {
	m.counter("stored_responses", nil)
	if !m.disabled.AccountStoredResponses && pubID != metrics.PublisherUnknown {
		m.counter("account_stored_responses", []attribute.KeyValue{m.accountAttribute(pubID)})
	}
}

func (m *Metrics) RecordGvlListRequest() { m.counter("gvl_requests", nil) }

func (m *Metrics) RecordLiveGVLFetch(success bool) {
	m.counter("live_gvl_fetch", []attribute.KeyValue{str(successLabel, status(success))})
}

func (m *Metrics) RecordAdsCertReq(success bool) {
	m.counter("ads_cert_requests", []attribute.KeyValue{str(successLabel, status(success))})
}

func (m *Metrics) RecordAdsCertSignTime(duration time.Duration) {
	m.observe("ads_cert_sign_time", duration.Seconds(), nil)
}

func (m *Metrics) recordBidValidation(adapter openrtb_ext.BidderName, account, adapterMetric, accountMetric string) {
	m.counter(adapterMetric, []attribute.KeyValue{str(adapterLabel, adapterName(adapter)), str(successLabel, successLabel)})
	if !m.disabled.AccountAdapterDetails && account != metrics.PublisherUnknown {
		m.counter(accountMetric, []attribute.KeyValue{m.accountAttribute(account), str(successLabel, successLabel)})
	}
}

func (m *Metrics) RecordBidValidationCreativeSizeError(adapter openrtb_ext.BidderName, account string) {
	m.recordBidValidation(adapter, account, "adapter_response_validation_size_err", "account_response_validation_size_err")
}

func (m *Metrics) RecordBidValidationCreativeSizeWarn(adapter openrtb_ext.BidderName, account string) {
	m.recordBidValidation(adapter, account, "adapter_response_validation_size_warn", "account_response_validation_size_warn")
}

func (m *Metrics) RecordBidValidationSecureMarkupError(adapter openrtb_ext.BidderName, account string) {
	m.recordBidValidation(adapter, account, "adapter_response_validation_secure_err", "account_response_validation_secure_err")
}

func (m *Metrics) RecordBidValidationSecureMarkupWarn(adapter openrtb_ext.BidderName, account string) {
	m.recordBidValidation(adapter, account, "adapter_response_validation_secure_warn", "account_response_validation_secure_warn")
}

func moduleAttrs(labels metrics.ModuleLabels) []attribute.KeyValue {
	return []attribute.KeyValue{str("stage", labels.Stage)}
}

func (m *Metrics) RecordModuleCalled(labels metrics.ModuleLabels, duration time.Duration) {
	m.counter("modules_"+labels.Module+"_called", moduleAttrs(labels))
	m.observe("modules_"+labels.Module+"_duration", duration.Seconds(), moduleAttrs(labels))
}

func (m *Metrics) RecordModuleFailed(labels metrics.ModuleLabels) {
	m.counter("modules_"+labels.Module+"_failed", moduleAttrs(labels))
}

func (m *Metrics) RecordModuleSuccessNooped(labels metrics.ModuleLabels) {
	m.counter("modules_"+labels.Module+"_success_noops", moduleAttrs(labels))
}

func (m *Metrics) RecordModuleSuccessUpdated(labels metrics.ModuleLabels) {
	m.counter("modules_"+labels.Module+"_success_updates", moduleAttrs(labels))
}

func (m *Metrics) RecordModuleSuccessRejected(labels metrics.ModuleLabels) {
	m.counter("modules_"+labels.Module+"_success_rejects", moduleAttrs(labels))
}

func (m *Metrics) RecordModuleExecutionError(labels metrics.ModuleLabels) {
	m.counter("modules_"+labels.Module+"_execution_errors", moduleAttrs(labels))
}

func (m *Metrics) RecordModuleTimeout(labels metrics.ModuleLabels) {
	m.counter("modules_"+labels.Module+"_timeouts", moduleAttrs(labels))
}

func (m *Metrics) RecordAdapterThrottled(name openrtb_ext.BidderName) {
	m.counter("adapter_throttled", []attribute.KeyValue{str(adapterLabel, adapterName(name))})
}

func (m *Metrics) RecordAdapterConnectionDialError(name openrtb_ext.BidderName) {
	if !m.disabled.AdapterConnectionMetrics && !m.disabled.AdapterConnectionDialMetrics {
		m.counter("adapter_connection_dial_errors", []attribute.KeyValue{str(adapterLabel, adapterName(name))})
	}
}

func (m *Metrics) RecordAdapterConnectionDialTime(name openrtb_ext.BidderName, duration time.Duration) {
	if !m.disabled.AdapterConnectionMetrics && !m.disabled.AdapterConnectionDialMetrics {
		m.observe("adapter_connection_dial_time", duration.Seconds(), []attribute.KeyValue{str(adapterLabel, adapterName(name))})
	}
}
