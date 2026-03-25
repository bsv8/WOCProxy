package wocproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBaseURLForNetwork(t *testing.T) {
	if got, want := BaseURLForNetwork(" http://127.0.0.1:18222/ ", "test"), "http://127.0.0.1:18222/v1/bsv/test"; got != want {
		t.Fatalf("BaseURLForNetwork(test)=%q, want %q", got, want)
	}
	if got, want := BaseURLForNetwork("http://127.0.0.1:18222", "mainnet"), "http://127.0.0.1:18222/v1/bsv/main"; got != want {
		t.Fatalf("BaseURLForNetwork(mainnet)=%q, want %q", got, want)
	}
}

func TestHandlerHealthz(t *testing.T) {
	proxy, err := New(Config{})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	srv := httptest.NewServer(proxy.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz error: %v", err)
	}
	defer resp.Body.Close()

	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode healthz response error: %v", err)
	}
	if resp.StatusCode != http.StatusOK || out["service"] != ServiceName {
		t.Fatalf("unexpected healthz response: status=%d body=%+v", resp.StatusCode, out)
	}
}

func TestProxySerializesUpstreamRequests(t *testing.T) {
	var mu sync.Mutex
	calls := make([]time.Time, 0, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, time.Now())
		mu.Unlock()
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	proxy, err := New(Config{
		UpstreamRootURL: upstream.URL,
		MinInterval:     40 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	srv := httptest.NewServer(proxy.Handler())
	defer srv.Close()

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	for range 2 {
		go func() {
			defer wg.Done()
			<-start
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/v1/bsv/test/chain/info", nil)
			if err != nil {
				t.Errorf("new request error: %v", err)
				return
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("proxy request error: %v", err)
				return
			}
			_ = resp.Body.Close()
		}()
	}
	close(start)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("unexpected upstream call count: got=%d want=2", len(calls))
	}
	if diff := calls[1].Sub(calls[0]); diff < 35*time.Millisecond {
		t.Fatalf("unexpected upstream interval: %s", diff)
	}
}

func TestProxySetsStableUserAgentForUpstream(t *testing.T) {
	gotUA := ""
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = strings.TrimSpace(r.Header.Get("User-Agent"))
		_, _ = w.Write([]byte(`{"blocks":123}`))
	}))
	defer upstream.Close()

	proxy, err := New(Config{UpstreamRootURL: upstream.URL})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	srv := httptest.NewServer(proxy.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/bsv/test/chain/info")
	if err != nil {
		t.Fatalf("GET proxied chain info error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected proxy response status: got=%d want=%d", resp.StatusCode, http.StatusOK)
	}
	if gotUA == "" {
		t.Fatalf("upstream user-agent is empty")
	}
}

func TestProxyLogsAccessSummaryAndUpstreamFailure(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	proxy, err := New(Config{
		UpstreamRootURL: "http://127.0.0.1:1",
		Logger:          logger,
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	srv := httptest.NewServer(proxy.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/bsv/test/chain/info")
	if err != nil {
		t.Fatalf("GET proxied chain info error: %v", err)
	}
	_ = resp.Body.Close()

	out := logBuf.String()
	if !strings.Contains(out, "request served") {
		t.Fatalf("expected access log, got: %s", out)
	}
	if !strings.Contains(out, "upstream request failed") {
		t.Fatalf("expected upstream failure log, got: %s", out)
	}
	if !strings.Contains(out, "path=/v1/bsv/test/chain/info") {
		t.Fatalf("expected path field in logs, got: %s", out)
	}
}
