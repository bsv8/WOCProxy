package wocproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	DefaultListenAddr      = "127.0.0.1:18222"
	DefaultRootURL         = "http://" + DefaultListenAddr
	DefaultUpstreamRootURL = "https://api.whatsonchain.com"
	ServiceName            = "woc-proxy"
	ServiceVersion         = "1.0.0"
	DefaultMaxRetries      = 3
	DefaultRetryBaseDelay  = 1500 * time.Millisecond
	DefaultRetryMaxDelay   = 12 * time.Second
)

// Config 描述透明代理运行参数。
// 设计约束：
// - 对外协议必须保持 WOC 原样，业务侧只配置 baseURL；
// - 频率控制只存在这一处，不再让调用方重复包装同样的方法面。
type Config struct {
	UpstreamRootURL string
	MinInterval     time.Duration
	Transport       http.RoundTripper
	MaxRetries      int
	RetryBaseDelay  time.Duration
	RetryMaxDelay   time.Duration
	// Logger 用于记录代理请求摘要与上游失败日志；为空时不输出日志。
	Logger *slog.Logger
}

type Proxy struct {
	upstreamRoot *url.URL
	client       *http.Client
	minInterval  time.Duration
	maxRetries   int
	retryBase    time.Duration
	retryMax     time.Duration
	rateGate     *intervalTransport
	logger       *slog.Logger
	reqSeq       atomic.Uint64
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
	rateGate := newIntervalTransport(transport, cfg.MinInterval)
	maxRetries := cfg.MaxRetries
	if maxRetries <= 0 {
		maxRetries = DefaultMaxRetries
	}
	retryBase := cfg.RetryBaseDelay
	if retryBase <= 0 {
		retryBase = DefaultRetryBaseDelay
	}
	retryMax := cfg.RetryMaxDelay
	if retryMax <= 0 {
		retryMax = DefaultRetryMaxDelay
	}
	if retryMax < retryBase {
		retryMax = retryBase
	}

	return &Proxy{
		upstreamRoot: upstreamRoot,
		client:       &http.Client{Transport: rateGate},
		minInterval:  cfg.MinInterval,
		maxRetries:   maxRetries,
		retryBase:    retryBase,
		retryMax:     retryMax,
		rateGate:     rateGate,
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
	requestID := p.nextRequestID()
	requestEnteredAt := time.Now()
	p.logInfo("proxy request accepted",
		slog.String("request_id", requestID),
		slog.String("method", strings.TrimSpace(r.Method)),
		slog.String("path", r.URL.RequestURI()),
		slog.String("target_url", target.String()),
		slog.String("remote_addr", strings.TrimSpace(r.RemoteAddr)),
	)

	bodyBytes, err := readRequestBody(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer func() {
		if r.Body != nil {
			_ = r.Body.Close()
		}
	}()

	var resp *http.Response
	var attemptErr error
	attempts := maxInt(p.maxRetries+1, 1)
	for attempt := 1; attempt <= attempts; attempt++ {
		req, buildErr := p.buildUpstreamRequest(r.Context(), r, target.String(), bodyBytes, requestID, requestEnteredAt, attempt)
		if buildErr != nil {
			http.Error(w, buildErr.Error(), http.StatusBadGateway)
			return
		}
		resp, attemptErr = p.client.Do(req)
		if attemptErr == nil {
			if !shouldRetryStatus(resp.StatusCode) || attempt == attempts {
				break
			}
			retryDelay := p.retryDelayForResponse(attempt, resp)
			p.logWarn("upstream temporary response, retry scheduled",
				slog.String("request_id", requestID),
				slog.String("method", strings.TrimSpace(r.Method)),
				slog.String("path", r.URL.RequestURI()),
				slog.String("target_url", target.String()),
				slog.Int("status", resp.StatusCode),
				slog.Int("attempt", attempt),
				slog.Int64("retry_after_ms", retryDelay.Milliseconds()),
			)
			if !sleepWithRatePenalty(r.Context(), p.rateGate, retryDelay) {
				_ = resp.Body.Close()
				http.Error(w, r.Context().Err().Error(), http.StatusGatewayTimeout)
				return
			}
			_ = resp.Body.Close()
			resp = nil
			continue
		}
		if !shouldRetryError(attemptErr) || attempt == attempts {
			break
		}
		retryDelay := p.retryDelayForError(attempt)
		p.logWarn("upstream temporary error, retry scheduled",
			slog.String("request_id", requestID),
			slog.String("method", strings.TrimSpace(r.Method)),
			slog.String("path", r.URL.RequestURI()),
			slog.String("target_url", target.String()),
			slog.String("error", attemptErr.Error()),
			slog.Int("attempt", attempt),
			slog.Int64("retry_after_ms", retryDelay.Milliseconds()),
		)
		if !sleepWithRatePenalty(r.Context(), p.rateGate, retryDelay) {
			http.Error(w, r.Context().Err().Error(), http.StatusGatewayTimeout)
			return
		}
	}
	if attemptErr != nil {
		p.logWarn("upstream request failed",
			slog.String("request_id", requestID),
			slog.String("method", strings.TrimSpace(r.Method)),
			slog.String("path", r.URL.RequestURI()),
			slog.String("target_url", target.String()),
			slog.String("error", attemptErr.Error()),
		)
		http.Error(w, attemptErr.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	copyHeadersWithoutHopByHop(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (p *Proxy) buildUpstreamRequest(ctx context.Context, src *http.Request, rawURL string, body []byte, requestID string, requestEnteredAt time.Time, attempt int) (*http.Request, error) {
	traceCtx := context.WithValue(ctx, roundTripTraceContextKey{}, roundTripTrace{
		RequestID:        strings.TrimSpace(requestID),
		RequestMethod:    strings.TrimSpace(src.Method),
		RequestPath:      src.URL.RequestURI(),
		TargetURL:        strings.TrimSpace(rawURL),
		RequestEnteredAt: requestEnteredAt,
		Attempt:          attempt,
		Logger:           p.logger,
	})
	req, err := http.NewRequestWithContext(traceCtx, src.Method, rawURL, bytesReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = cloneHeadersWithoutHopByHop(src.Header)
	if strings.TrimSpace(req.Header.Get("User-Agent")) == "" {
		req.Header.Set("User-Agent", "bsv8-wocproxy/1.0")
	}
	if len(body) > 0 {
		req.ContentLength = int64(len(body))
	} else if src.ContentLength >= 0 {
		req.ContentLength = src.ContentLength
	}
	return req, nil
}

func (p *Proxy) retryDelayForResponse(attempt int, resp *http.Response) time.Duration {
	if d, ok := retryAfterDelay(resp); ok {
		return clampRetryDelay(d, p.retryBase, p.retryMax)
	}
	return p.retryDelayForError(attempt)
}

func (p *Proxy) retryDelayForError(attempt int) time.Duration {
	return exponentialRetryDelay(attempt, p.retryBase, p.retryMax)
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

func (p *Proxy) nextRequestID() string {
	if p == nil {
		return ""
	}
	seq := p.reqSeq.Add(1)
	return fmt.Sprintf("wocp-%d", seq)
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

func newIntervalTransport(base http.RoundTripper, interval time.Duration) *intervalTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &intervalTransport{base: base, interval: interval}
}

func (t *intervalTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("request is nil")
	}
	trace := traceFromContext(req.Context())
	queueStartedAt := time.Now()
	if err := t.wait(req.Context()); err != nil {
		if trace.Enabled() {
			trace.Logger.LogAttrs(context.Background(), slog.LevelWarn, "upstream dispatch canceled before send",
				slog.String("request_id", trace.RequestID),
				slog.String("method", trace.RequestMethod),
				slog.String("path", trace.RequestPath),
				slog.String("target_url", trace.TargetURL),
				slog.Int("attempt", trace.Attempt),
				slog.Int64("queue_wait_ms", time.Since(queueStartedAt).Milliseconds()),
				slog.String("error", err.Error()),
			)
		}
		return nil, err
	}
	dispatchAt := time.Now()
	if trace.Enabled() {
		trace.Logger.LogAttrs(context.Background(), slog.LevelInfo, "upstream dispatch started",
			slog.String("request_id", trace.RequestID),
			slog.String("method", trace.RequestMethod),
			slog.String("path", trace.RequestPath),
			slog.String("target_url", trace.TargetURL),
			slog.Int("attempt", trace.Attempt),
			slog.String("request_entered_at", trace.RequestEnteredAt.UTC().Format(time.RFC3339Nano)),
			slog.String("dispatch_at", dispatchAt.UTC().Format(time.RFC3339Nano)),
			slog.Int64("queue_wait_ms", dispatchAt.Sub(queueStartedAt).Milliseconds()),
			slog.Int64("request_age_ms", dispatchAt.Sub(trace.RequestEnteredAt).Milliseconds()),
		)
	}
	resp, err := t.base.RoundTrip(req)
	if trace.Enabled() {
		attrs := []slog.Attr{
			slog.String("request_id", trace.RequestID),
			slog.String("method", trace.RequestMethod),
			slog.String("path", trace.RequestPath),
			slog.String("target_url", trace.TargetURL),
			slog.Int("attempt", trace.Attempt),
			slog.Int64("upstream_duration_ms", time.Since(dispatchAt).Milliseconds()),
		}
		if err != nil {
			attrs = append(attrs, slog.String("error", err.Error()))
			trace.Logger.LogAttrs(context.Background(), slog.LevelWarn, "upstream dispatch failed", attrs...)
		} else {
			attrs = append(attrs, slog.Int("status", resp.StatusCode))
			trace.Logger.LogAttrs(context.Background(), slog.LevelInfo, "upstream dispatch completed", attrs...)
		}
	}
	return resp, err
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
	if t.interval > 0 {
		t.nextAllowed = slot.Add(t.interval)
	} else {
		t.nextAllowed = slot
	}
	return slot
}

func (t *intervalTransport) Penalize(wait time.Duration) {
	if t == nil || wait <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	until := time.Now().Add(wait)
	if until.After(t.nextAllowed) {
		t.nextAllowed = until
	}
}

func readRequestBody(r *http.Request) ([]byte, error) {
	if r == nil || r.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	return body, nil
}

func bytesReader(body []byte) io.Reader {
	if len(body) == 0 {
		return nil
	}
	return bytes.NewReader(body)
}

func shouldRetryStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func shouldRetryError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return true
}

func retryAfterDelay(resp *http.Response) (time.Duration, bool) {
	if resp == nil {
		return 0, false
	}
	raw := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if raw == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(raw); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second, true
	}
	if ts, err := http.ParseTime(raw); err == nil {
		d := time.Until(ts)
		if d > 0 {
			return d, true
		}
	}
	return 0, false
}

func exponentialRetryDelay(attempt int, base time.Duration, maxDelay time.Duration) time.Duration {
	if base <= 0 {
		base = DefaultRetryBaseDelay
	}
	if maxDelay <= 0 {
		maxDelay = DefaultRetryMaxDelay
	}
	if maxDelay < base {
		maxDelay = base
	}
	if attempt < 1 {
		attempt = 1
	}
	exp := math.Pow(2, float64(attempt-1))
	delay := time.Duration(float64(base) * exp)
	return clampRetryDelay(delay, base, maxDelay)
}

func clampRetryDelay(delay, base, maxDelay time.Duration) time.Duration {
	if delay <= 0 {
		delay = base
	}
	if delay < base {
		delay = base
	}
	if maxDelay > 0 && delay > maxDelay {
		delay = maxDelay
	}
	return delay
}

func sleepWithRatePenalty(ctx context.Context, gate *intervalTransport, delay time.Duration) bool {
	if delay <= 0 {
		return true
	}
	if gate != nil {
		gate.Penalize(delay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func maxInt(a int, b int) int {
	if a > b {
		return a
	}
	return b
}

type roundTripTraceContextKey struct{}

type roundTripTrace struct {
	RequestID        string
	RequestMethod    string
	RequestPath      string
	TargetURL        string
	RequestEnteredAt time.Time
	Attempt          int
	Logger           *slog.Logger
}

func (t roundTripTrace) Enabled() bool {
	return t.Logger != nil
}

func traceFromContext(ctx context.Context) roundTripTrace {
	if ctx == nil {
		return roundTripTrace{}
	}
	trace, _ := ctx.Value(roundTripTraceContextKey{}).(roundTripTrace)
	return trace
}
