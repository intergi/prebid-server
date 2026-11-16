package opentelemetry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prebid/prebid-server/v4/config"
	"github.com/prebid/prebid-server/v4/metrics"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	collectormetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"
)

func TestRecordsRequestCountersAndSuccessfulDurations(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	engine := newMetrics(provider, config.OpenTelemetryMetrics{AccountLimit: 10}, config.PrometheusMetrics{}, config.DisabledMetrics{}, nil)

	engine.RecordRequest(metrics.Labels{
		RType: metrics.ReqTypeORTB2Web, RequestStatus: metrics.RequestStatusOK,
		PubID: "publisher", CookieFlag: metrics.CookieFlagNo, RequestSize: 1024,
	})
	engine.RecordRequest(metrics.Labels{
		RType: metrics.ReqTypeORTB2Web, RequestStatus: metrics.RequestStatusErr,
		PubID: metrics.PublisherUnknown,
	})
	engine.RecordRequestTime(metrics.Labels{RType: metrics.ReqTypeORTB2Web, RequestStatus: metrics.RequestStatusOK}, 20*time.Millisecond)
	engine.RecordRequestTime(metrics.Labels{RType: metrics.ReqTypeORTB2Web, RequestStatus: metrics.RequestStatusErr}, 30*time.Millisecond)

	data := collectMetrics(t, reader)
	requests := findMetric(t, data, "requests")
	requestSum, ok := requests.Data.(metricdata.Sum[int64])
	require.True(t, ok)
	var requestCount int64
	for _, point := range requestSum.DataPoints {
		requestCount += point.Value
	}
	require.EqualValues(t, 2, requestCount)

	requestDuration := findMetric(t, data, "request_time_seconds")
	durationHistogram, ok := requestDuration.Data.(metricdata.Histogram[float64])
	require.True(t, ok)
	require.Len(t, durationHistogram.DataPoints, 1)
	require.EqualValues(t, 1, durationHistogram.DataPoints[0].Count)
	require.InDelta(t, 0.02, durationHistogram.DataPoints[0].Sum, 1e-9)
	require.Equal(t, []float64{0.05, 0.1, 0.15, 0.2, 0.25, 0.3, 0.4, 0.5, 0.75, 1}, durationHistogram.DataPoints[0].Bounds)
}

func TestAccountAttributesAreBoundedAndDisabledMetricsAreHonored(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	engine := newMetrics(provider, config.OpenTelemetryMetrics{AccountLimit: 1}, config.PrometheusMetrics{}, config.DisabledMetrics{
		AccountDebug:           true,
		AccountStoredResponses: true,
		AccountAdapterDetails:  true,
	}, nil)

	engine.RecordRequest(metrics.Labels{RType: metrics.ReqTypeORTB2Web, RequestStatus: metrics.RequestStatusOK, PubID: "one"})
	engine.RecordRequest(metrics.Labels{RType: metrics.ReqTypeORTB2Web, RequestStatus: metrics.RequestStatusOK, PubID: "two"})
	engine.RecordDebugRequest(true, "one")
	engine.RecordStoredResponse("one")
	engine.RecordBidValidationCreativeSizeError("bidder", "one")

	data := collectMetrics(t, reader)
	accountRequests := findMetric(t, data, "account_requests")
	accountSum, ok := accountRequests.Data.(metricdata.Sum[int64])
	require.True(t, ok)
	require.Len(t, accountSum.DataPoints, 2)
	accounts := map[string]int64{}
	for _, point := range accountSum.DataPoints {
		for _, attribute := range point.Attributes.ToSlice() {
			if attribute.Key == "account" {
				accounts[attribute.Value.AsString()] = point.Value
			}
		}
	}
	require.Equal(t, map[string]int64{"one": 1, "overflow": 1}, accounts)
	require.Empty(t, metricsByName(data, "account_debug_requests"))
	require.Empty(t, metricsByName(data, "account_stored_responses"))
	require.Empty(t, metricsByName(data, "account_response_validation_size_err"))
}

func TestModuleInitializationDoesNotRecordMeasurements(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })
	engine := newMetrics(provider, config.OpenTelemetryMetrics{}, config.PrometheusMetrics{}, config.DisabledMetrics{},
		map[string][]string{"test": {"entrypoint"}})

	require.Len(t, engine.counters, 7)
	require.Len(t, engine.histograms, 1)
	require.Empty(t, collectMetrics(t, reader).ScopeMetrics)

	engine.RecordModuleCalled(metrics.ModuleLabels{Module: "test", Stage: "entrypoint"}, 20*time.Millisecond)
	data := collectMetrics(t, reader)
	calls, ok := findMetric(t, data, "modules_test_called").Data.(metricdata.Sum[int64])
	require.True(t, ok)
	require.Len(t, calls.DataPoints, 1)
	require.EqualValues(t, 1, calls.DataPoints[0].Value)
	stage, ok := calls.DataPoints[0].Attributes.Value("stage")
	require.True(t, ok)
	require.Equal(t, "entrypoint", stage.AsString())
	duration, ok := findMetric(t, data, "modules_test_duration").Data.(metricdata.Histogram[float64])
	require.True(t, ok)
	require.Len(t, duration.DataPoints, 1)
	require.EqualValues(t, 1, duration.DataPoints[0].Count)
	require.InDelta(t, 0.02, duration.DataPoints[0].Sum, 1e-9)
	for _, suffix := range []string{"failed", "success_noops", "success_updates", "success_rejects", "execution_errors", "timeouts"} {
		require.Empty(t, metricsByName(data, "modules_test_"+suffix))
	}
}

func TestShutdownExportsPendingMetricsToOTLPHTTP(t *testing.T) {
	requests := make(chan []byte, 1)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/metrics" || r.Method != "POST" || r.Header.Get("Content-Type") != "application/x-protobuf" {
			t.Errorf("unexpected OTLP request: %s %s content-type=%q", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("failed to read OTLP payload: %v", err)
		}
		requests <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()

	engine, err := NewMetrics(config.OpenTelemetryMetrics{
		Endpoint: collector.URL, Protocol: "http/protobuf", MetricSendInterval: 3600,
		TimeoutMillis: 1000, AccountLimit: 10, ServiceName: "test",
	}, config.PrometheusMetrics{}, config.DisabledMetrics{}, nil)
	require.NoError(t, err)
	engine.RecordTMaxTimeout()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, engine.Shutdown(ctx))
	select {
	case payload := <-requests:
		require.NotEmpty(t, payload)
		var export collectormetricspb.ExportMetricsServiceRequest
		require.NoError(t, proto.Unmarshal(payload, &export))
		foundTimeoutCounter := false
		for _, resourceMetrics := range export.ResourceMetrics {
			for _, scopeMetrics := range resourceMetrics.ScopeMetrics {
				for _, metric := range scopeMetrics.Metrics {
					if metric.Name == "tmax_timeout" {
						foundTimeoutCounter = true
					}
				}
			}
		}
		require.True(t, foundTimeoutCounter)
	case <-ctx.Done():
		t.Fatal("collector did not receive the final export")
	}
}

func collectMetrics(t *testing.T, reader *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()
	var data metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &data))
	return data
}

func findMetric(t *testing.T, data metricdata.ResourceMetrics, name string) metricdata.Metrics {
	t.Helper()
	metrics := metricsByName(data, name)
	require.Len(t, metrics, 1)
	return metrics[0]
}

func metricsByName(data metricdata.ResourceMetrics, name string) []metricdata.Metrics {
	var found []metricdata.Metrics
	for _, scope := range data.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name == name {
				found = append(found, metric)
			}
		}
	}
	return found
}
