package gateway

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPrometheusReaderMergesEndpointSeries(t *testing.T) {
	var mu sync.Mutex
	queries := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("query")
		mu.Lock()
		queries = append(queries, query)
		mu.Unlock()
		value := "0"
		switch {
		case strings.Contains(query, "num_requests_waiting") && strings.Contains(query, "count("):
			value = "2"
		case strings.Contains(query, "num_requests_waiting"):
			value = "4"
		case strings.Contains(query, "num_requests_running"):
			value = "1"
		case strings.Contains(query, "ember_mock_requests_total"):
			value = "12"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"status":"success","data":{"resultType":"matrix","result":[{"values":[[1786867200,"%s"],[1786867205,"%s"]]}]}}`, value, value)
	}))
	defer server.Close()

	reader, err := NewPrometheusReader(server.URL, server.Client())
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}
	reader.now = func() time.Time { return time.Date(2026, 8, 16, 8, 0, 5, 0, time.UTC) }
	metrics, err := reader.ReadEndpointMetrics(t.Context(), "abc-123", 10*time.Minute, 5*time.Second)
	if err != nil {
		t.Fatalf("read metrics: %v", err)
	}
	if metrics.Current.QueueDepth != 4 || metrics.Current.RunningRequests != 1 || metrics.Current.Replicas != 2 || metrics.Current.RequestsTotal != 12 {
		t.Fatalf("unexpected current values: %#v", metrics.Current)
	}
	if len(metrics.Series) != 2 || metrics.Series[1].QueueDepth != 4 || metrics.Series[1].RunningRequests != 1 || metrics.Series[1].Replicas != 2 {
		t.Fatalf("unexpected merged series: %#v", metrics.Series)
	}
	if len(queries) != 4 {
		t.Fatalf("expected four bounded queries, got %d", len(queries))
	}
	for _, query := range queries {
		if !strings.Contains(query, `endpoint_uid="abc-123"`) {
			t.Fatalf("query was not endpoint scoped: %s", query)
		}
	}
}
