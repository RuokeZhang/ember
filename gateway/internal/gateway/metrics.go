package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type EndpointMetricsReader interface {
	ReadEndpointMetrics(context.Context, string, time.Duration, time.Duration) (*EndpointMetrics, error)
}

type EndpointMetrics struct {
	ObservedAt    time.Time             `json:"observedAt"`
	WindowSeconds int64                 `json:"windowSeconds"`
	StepSeconds   int64                 `json:"stepSeconds"`
	Current       EndpointMetricCurrent `json:"current"`
	Series        []EndpointMetricPoint `json:"series"`
}

type EndpointMetricCurrent struct {
	QueueDepth      float64 `json:"queueDepth"`
	RunningRequests float64 `json:"runningRequests"`
	Replicas        float64 `json:"replicas"`
	RequestsTotal   float64 `json:"requestsTotal"`
}

type EndpointMetricPoint struct {
	Timestamp       time.Time `json:"timestamp"`
	QueueDepth      float64   `json:"queueDepth"`
	RunningRequests float64   `json:"runningRequests"`
	Replicas        float64   `json:"replicas"`
}

type PrometheusReader struct {
	baseURL *url.URL
	client  *http.Client
	now     func() time.Time
}

type prometheusResponse struct {
	Status    string         `json:"status"`
	Data      prometheusData `json:"data"`
	ErrorType string         `json:"errorType"`
	Error     string         `json:"error"`
}

type prometheusData struct {
	ResultType string             `json:"resultType"`
	Result     []prometheusSeries `json:"result"`
}

type prometheusSeries struct {
	Values [][]json.RawMessage `json:"values"`
}

func NewPrometheusReader(rawURL string, client *http.Client) (*PrometheusReader, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	if err != nil {
		return nil, fmt.Errorf("parse Prometheus URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("Prometheus URL must use http or https")
	}
	if parsed.Host == "" {
		return nil, errors.New("Prometheus URL must include a host")
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &PrometheusReader{
		baseURL: parsed,
		client:  client,
		now:     func() time.Time { return time.Now().UTC() },
	}, nil
}

func (r *PrometheusReader) ReadEndpointMetrics(ctx context.Context, endpointUID string, window, step time.Duration) (*EndpointMetrics, error) {
	if window < time.Minute || window > time.Hour {
		return nil, errors.New("metrics window must be between 1 minute and 1 hour")
	}
	if step < 2*time.Second || step > 30*time.Second {
		return nil, errors.New("metrics step must be between 2 and 30 seconds")
	}
	if strings.Trim(endpointUID, "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ-") != "" {
		return nil, errors.New("endpoint UID contains invalid characters")
	}
	end := r.now().UTC()
	start := end.Add(-window)
	queries := map[string]string{
		"queue":    fmt.Sprintf(`sum(vllm:num_requests_waiting{endpoint_uid=%q}) or vector(0)`, endpointUID),
		"running":  fmt.Sprintf(`sum(vllm:num_requests_running{endpoint_uid=%q}) or vector(0)`, endpointUID),
		"replicas": fmt.Sprintf(`count(vllm:num_requests_waiting{endpoint_uid=%q}) or vector(0)`, endpointUID),
		"requests": fmt.Sprintf(`sum(ember_mock_requests_total{endpoint_uid=%q}) or vector(0)`, endpointUID),
	}
	results := make(map[string][]EndpointMetricPoint, len(queries))
	var resultMu sync.Mutex
	var wait sync.WaitGroup
	errs := make(chan error, len(queries))
	for name, query := range queries {
		name, query := name, query
		wait.Add(1)
		go func() {
			defer wait.Done()
			values, err := r.queryRange(ctx, query, start, end, step)
			if err != nil {
				errs <- fmt.Errorf("%s query: %w", name, err)
				return
			}
			resultMu.Lock()
			results[name] = values
			resultMu.Unlock()
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		return nil, err
	}

	points := mergeMetricPoints(results["queue"], results["running"], results["replicas"])
	current := EndpointMetricCurrent{
		QueueDepth:      lastMetricValue(results["queue"], "queue"),
		RunningRequests: lastMetricValue(results["running"], "running"),
		Replicas:        lastMetricValue(results["replicas"], "replicas"),
		RequestsTotal:   lastMetricValue(results["requests"], "requests"),
	}
	return &EndpointMetrics{
		ObservedAt:    end,
		WindowSeconds: int64(window.Seconds()),
		StepSeconds:   int64(step.Seconds()),
		Current:       current,
		Series:        points,
	}, nil
}

func (r *PrometheusReader) queryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]EndpointMetricPoint, error) {
	endpoint := *r.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/api/v1/query_range"
	values := endpoint.Query()
	values.Set("query", query)
	values.Set("start", strconv.FormatFloat(float64(start.UnixNano())/1e9, 'f', 3, 64))
	values.Set("end", strconv.FormatFloat(float64(end.UnixNano())/1e9, 'f', 3, 64))
	values.Set("step", strconv.FormatFloat(step.Seconds(), 'f', -1, 64))
	endpoint.RawQuery = values.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	response, err := r.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Prometheus returned %s", response.Status)
	}
	var payload prometheusResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, 2<<20))
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode Prometheus response: %w", err)
	}
	if payload.Status != "success" {
		return nil, fmt.Errorf("Prometheus query failed (%s): %s", payload.ErrorType, payload.Error)
	}
	if payload.Data.ResultType != "matrix" {
		return nil, fmt.Errorf("unexpected Prometheus result type %q", payload.Data.ResultType)
	}
	if len(payload.Data.Result) == 0 {
		return nil, nil
	}
	points := make([]EndpointMetricPoint, 0, len(payload.Data.Result[0].Values))
	for _, pair := range payload.Data.Result[0].Values {
		if len(pair) != 2 {
			continue
		}
		var timestamp float64
		if err := json.Unmarshal(pair[0], &timestamp); err != nil {
			return nil, fmt.Errorf("decode metric timestamp: %w", err)
		}
		var rawValue string
		if err := json.Unmarshal(pair[1], &rawValue); err != nil {
			return nil, fmt.Errorf("decode metric value: %w", err)
		}
		value, err := strconv.ParseFloat(rawValue, 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			value = 0
		}
		points = append(points, EndpointMetricPoint{
			Timestamp:  time.Unix(0, int64(timestamp*float64(time.Second))).UTC(),
			QueueDepth: value,
		})
	}
	return points, nil
}

func mergeMetricPoints(queue, running, replicas []EndpointMetricPoint) []EndpointMetricPoint {
	merged := map[int64]EndpointMetricPoint{}
	for _, item := range queue {
		key := item.Timestamp.UnixMilli()
		point := merged[key]
		point.Timestamp = item.Timestamp
		point.QueueDepth = item.QueueDepth
		merged[key] = point
	}
	for _, item := range running {
		key := item.Timestamp.UnixMilli()
		point := merged[key]
		point.Timestamp = item.Timestamp
		point.RunningRequests = item.QueueDepth
		merged[key] = point
	}
	for _, item := range replicas {
		key := item.Timestamp.UnixMilli()
		point := merged[key]
		point.Timestamp = item.Timestamp
		point.Replicas = item.QueueDepth
		merged[key] = point
	}
	points := make([]EndpointMetricPoint, 0, len(merged))
	for _, point := range merged {
		points = append(points, point)
	}
	sort.Slice(points, func(i, j int) bool { return points[i].Timestamp.Before(points[j].Timestamp) })
	return points
}

func lastMetricValue(points []EndpointMetricPoint, _ string) float64 {
	if len(points) == 0 {
		return 0
	}
	return points[len(points)-1].QueueDepth
}
