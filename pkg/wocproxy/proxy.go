package wocproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	DefaultListenAddr      = "127.0.0.1:18222"
	DefaultRootURL         = "http://" + DefaultListenAddr
	DefaultUpstreamRootURL = "https://api.whatsonchain.com"
	ServiceName            = "woc-proxy"
	ServiceVersion         = "1.0.0"
)

// Config 描述透明代理运行参数。
// 设计约束：
// - 对外协议必须保持 WOC 原样，业务侧只配置 baseURL；
// - 频率控制只存在这一处，不再让调用方重复包装同样的方法面。
type Config struct {
	UpstreamRootURL string
	MinInterval     time.Duration
	Transport       http.RoundTripper
	// Logger 用于记录代理请求摘要与上游失败日志；为空时不输出日志。
	Logger *slog.Logger
}

type Proxy struct {
	upstreamRoot *url.URL
	client       *http.Client
	minInterval  time.Duration
	logger       *slog.Logger
}

func New(cfg Config) (*Proxy, error) {
	upstreamRaw := strings.TrimSpace(cfg.UpstreamRootURL)
	if upstreamRaw == "" {
		upstreamRaw = DefaultUpstreamRootURL
	}
	upstreamRoot, err := url.Parse(upstreamRaw)
	if err != nil {
		return nil, fmt.Errorf("parse upstream_root_url: %w", err)
	}
	if upstreamRoot.Scheme == "" || upstreamRoot.Host == "" {
		return nil, fmt.Errorf("upstream_root_url must include scheme and host")
	}

	transport := cfg.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	transport = newIntervalTransport(transport, cfg.MinInterval)

	return &Proxy{
		upstreamRoot: upstreamRoot,
		client:       &http.Client{Transport: transport},
		minInterval:  cfg.MinInterval,
		logger:       cfg.Logger,
	}, nil
}

func (p *Proxy) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", p.handleHealth)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p.handleProxy(w, r)
	})
	return p.withAccessLog(mux)
}

func (p *Proxy) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                true,
		"service":           ServiceName,
		"version":           ServiceVersion,
		"upstream_root_url": p.UpstreamRootURL(),
		"min_interval":      p.minInterval.String(),
	})
}

func (p *Proxy) handleProxy(w http.ResponseWriter, r *http.Request) {
	if p == nil || p.upstreamRoot == nil || p.client == nil {
		http.Error(w, "proxy is not initialized", http.StatusBadGateway)
		return
	}

	// 这里不用 ReverseProxy，避免它把 User-Agent 清空后触发上游 Cloudflare 403。
	target := *p.upstreamRoot
	target.Path = r.URL.Path
	target.RawQuery = r.URL.RawQuery

	req, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	req.Header = cloneHeadersWithoutHopByHop(r.Header)
	if strings.TrimSpace(req.Header.Get("User-Agent")) == "" {
		req.Header.Set("User-Agent", "bsv8-wocproxy/1.0")
	}
	if r.ContentLength >= 0 {
		req.ContentLength = r.ContentLength
	}

	resp, err := p.client.Do(req)
	if err != nil {
		p.logWarn("upstream request failed",
			slog.String("method", strings.TrimSpace(r.Method)),
			slog.String("path", r.URL.RequestURI()),
			slog.String("target_url", target.String()),
			slog.String("error", err.Error()),
		)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	copyHeadersWithoutHopByHop(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (p *Proxy) UpstreamRootURL() string {
	if p == nil || p.upstreamRoot == nil {
		return ""
	}
	return strings.TrimRight(p.upstreamRoot.String(), "/")
}

func (p *Proxy) withAccessLog(next http.Handler) http.Handler {
	if next == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "handler is not initialized", http.StatusInternalServerError)
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedAt := time.Now()
		rec := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rec, r)
		p.logInfo("request served",
			slog.String("method", strings.TrimSpace(r.Method)),
			slog.String("path", r.URL.RequestURI()),
			slog.Int("status", rec.statusCode),
			slog.Int64("duration_ms", time.Since(startedAt).Milliseconds()),
			slog.String("remote_addr", strings.TrimSpace(r.RemoteAddr)),
		)
	})
}

func NormalizeRootURL(rootURL string) string {
	root := strings.TrimRight(strings.TrimSpace(rootURL), "/")
	if root == "" {
		return DefaultRootURL
	}
	return root
}

func BaseURLForNetwork(rootURL string, network string) string {
	root := NormalizeRootURL(rootURL)
	switch strings.ToLower(strings.TrimSpace(network)) {
	case "main", "mainnet":
		return root + "/v1/bsv/main"
	default:
		return root + "/v1/bsv/test"
	}
}

func DefaultBaseURLForNetwork(network string) string {
	return BaseURLForNetwork(DefaultRootURL, network)
}

func (p *Proxy) logInfo(msg string, attrs ...slog.Attr) {
	if p == nil || p.logger == nil {
		return
	}
	p.logger.LogAttrs(context.Background(), slog.LevelInfo, msg, attrs...)
}

func (p *Proxy) logWarn(msg string, attrs ...slog.Attr) {
	if p == nil || p.logger == nil {
		return
	}
	p.logger.LogAttrs(context.Background(), slog.LevelWarn, msg, attrs...)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

type statusRecorder struct {
	http.ResponseWriter
	statusCode int
}

func (r *statusRecorder) WriteHeader(statusCode int) {
	r.statusCode = statusCode
	r.ResponseWriter.WriteHeader(statusCode)
}

func cloneHeadersWithoutHopByHop(src http.Header) http.Header {
	dst := make(http.Header, len(src))
	copyHeadersWithoutHopByHop(dst, src)
	return dst
}

func copyHeadersWithoutHopByHop(dst http.Header, src http.Header) {
	for key, values := range src {
		if isHopByHopHeader(key) {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func isHopByHopHeader(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "connection", "proxy-connection", "keep-alive", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

type intervalTransport struct {
	base     http.RoundTripper
	interval time.Duration

	mu          sync.Mutex
	nextAllowed time.Time
}

func newIntervalTransport(base http.RoundTripper, interval time.Duration) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	if interval <= 0 {
		return base
	}
	return &intervalTransport{base: base, interval: interval}
}

func (t *intervalTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("request is nil")
	}
	if err := t.wait(req.Context()); err != nil {
		return nil, err
	}
	return t.base.RoundTrip(req)
}

func (t *intervalTransport) wait(ctx context.Context) error {
	waitUntil := t.reserveSlot()
	waitDur := time.Until(waitUntil)
	if waitDur <= 0 {
		return nil
	}
	timer := time.NewTimer(waitDur)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (t *intervalTransport) reserveSlot() time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	slot := now
	if !t.nextAllowed.IsZero() && t.nextAllowed.After(now) {
		slot = t.nextAllowed
	}
	t.nextAllowed = slot.Add(t.interval)
	return slot
}
