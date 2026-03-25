package main

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bsv8/WOCProxy/pkg/wocproxy"
)

func main() {
	var (
		listenAddr      = flag.String("listen", envOr("WOC_PROXY_LISTEN", wocproxy.DefaultListenAddr), "listen address")
		upstreamRootURL = flag.String("upstream", envOr("WOC_PROXY_UPSTREAM_URL", wocproxy.DefaultUpstreamRootURL), "upstream root url")
		intervalRaw     = flag.String("interval", envOr("WOC_PROXY_MIN_INTERVAL", "1s"), "minimal interval between upstream requests")
		logFilePath     = flag.String("log-file", envOr("WOC_PROXY_LOG_FILE", defaultLogFilePath()), "log file path")
	)
	flag.Parse()

	logger, closeLog, err := buildLogger(strings.TrimSpace(*logFilePath))
	if err != nil {
		fmt.Fprintf(os.Stderr, "init logger failed: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		if closeLog != nil {
			_ = closeLog()
		}
	}()

	interval, err := time.ParseDuration(strings.TrimSpace(*intervalRaw))
	if err != nil || interval < 0 {
		logger.Error("invalid interval", "interval", strings.TrimSpace(*intervalRaw))
		os.Exit(2)
	}

	proxy, err := wocproxy.New(wocproxy.Config{
		UpstreamRootURL: strings.TrimSpace(*upstreamRootURL),
		MinInterval:     interval,
		Logger:          logger,
	})
	if err != nil {
		logger.Error("build proxy failed", "error", err.Error())
		os.Exit(1)
	}

	httpSrv := &http.Server{
		Addr:              strings.TrimSpace(*listenAddr),
		Handler:           proxy.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	logger.Info("wocproxy listening",
		"listen_addr", httpSrv.Addr,
		"upstream_root_url", proxy.UpstreamRootURL(),
		"min_interval", interval.String(),
		"log_file", strings.TrimSpace(*logFilePath),
	)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("wocproxy server failed", "error", err.Error())
		os.Exit(1)
	}
}

func envOr(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

func defaultLogFilePath() string {
	return filepath.Join(".vault", "wocproxy.log")
}

func buildLogger(logFilePath string) (*slog.Logger, func() error, error) {
	logFilePath = strings.TrimSpace(logFilePath)
	if logFilePath == "" {
		logFilePath = defaultLogFilePath()
	}
	if err := os.MkdirAll(filepath.Dir(logFilePath), 0o755); err != nil {
		return nil, nil, err
	}
	file, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, err
	}
	writer := io.MultiWriter(os.Stdout, file)
	handler := slog.NewTextHandler(writer, &slog.HandlerOptions{Level: slog.LevelInfo})
	return slog.New(handler), file.Close, nil
}
