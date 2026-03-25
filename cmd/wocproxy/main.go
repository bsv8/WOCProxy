package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/bsv8/WOCProxy/pkg/wocproxy"
)

func main() {
	var (
		listenAddr      = flag.String("listen", envOr("WOC_PROXY_LISTEN", wocproxy.DefaultListenAddr), "listen address")
		upstreamRootURL = flag.String("upstream", envOr("WOC_PROXY_UPSTREAM_URL", wocproxy.DefaultUpstreamRootURL), "upstream root url")
		intervalRaw     = flag.String("interval", envOr("WOC_PROXY_MIN_INTERVAL", "1s"), "minimal interval between upstream requests")
	)
	flag.Parse()

	interval, err := time.ParseDuration(strings.TrimSpace(*intervalRaw))
	if err != nil || interval < 0 {
		fmt.Fprintf(os.Stderr, "invalid interval: %q\n", *intervalRaw)
		os.Exit(2)
	}

	proxy, err := wocproxy.New(wocproxy.Config{
		UpstreamRootURL: strings.TrimSpace(*upstreamRootURL),
		MinInterval:     interval,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "build proxy failed: %v\n", err)
		os.Exit(1)
	}

	httpSrv := &http.Server{
		Addr:              strings.TrimSpace(*listenAddr),
		Handler:           proxy.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	fmt.Printf("wocproxy listening on %s, upstream=%s, interval=%s\n", httpSrv.Addr, proxy.UpstreamRootURL(), interval.String())
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "wocproxy server failed: %v\n", err)
		os.Exit(1)
	}
}

func envOr(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}
