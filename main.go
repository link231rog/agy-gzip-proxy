// agy-gzip-proxy is a lightweight local reverse proxy for Antigravity CLI (agy)
// that gzip-compresses outbound request bodies (via CLOUD_CODE_URL) to cut
// full-context replay upload traffic without modifying the agy binary.
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	defaultListen   = "127.0.0.1:18319"
	defaultUpstream = "https://daily-cloudcode-pa.googleapis.com"
	healthPath      = "/_agy_gzip_proxy/healthz"
	maxBodyBytes    = 128 << 20 // 128 MiB safety cap
)

var hopByHopHeaders = map[string]struct{}{
	"connection":          {},
	"proxy-connection":    {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"te":                  {},
	"trailer":             {},
	"transfer-encoding":   {},
	"upgrade":             {},
}

type config struct {
	listenAddr    string
	upstreamURL   *url.URL
	upstreamProxy string
	minBytes      int
	gzipLevel     int
	logStats      bool
}

type stats struct {
	startedAt          time.Time
	totalRequests      atomic.Uint64
	compressedRequests atomic.Uint64
	fallbackCount      atomic.Uint64
	totalRawBytes      atomic.Uint64
	totalSentBytes     atomic.Uint64
}

type proxyHandler struct {
	cfg       config
	transport *http.Transport
	stats     *stats
}

func envOrDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envIntOrDefault(key string, fallback int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envBoolOrDefault(key string, fallback bool) bool {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}

func newTransport(upstreamProxy string) (*http.Transport, error) {
	proxyFunc := http.ProxyFromEnvironment
	if upstreamProxy != "" {
		pu, err := url.Parse(upstreamProxy)
		if err != nil {
			return nil, fmt.Errorf("invalid upstream proxy URL %q: %w", upstreamProxy, err)
		}
		proxyFunc = http.ProxyURL(pu)
	}

	return &http.Transport{
		Proxy: proxyFunc,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// Keep response Content-Encoding untouched for the downstream client.
		DisableCompression: true,
	}, nil
}

func newProxyHandler(cfg config) (*proxyHandler, error) {
	tr, err := newTransport(cfg.upstreamProxy)
	if err != nil {
		return nil, err
	}
	return &proxyHandler{
		cfg:       cfg,
		transport: tr,
		stats:     &stats{startedAt: time.Now()},
	}, nil
}

func (h *proxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == healthPath && r.Method == http.MethodGet {
		h.serveHealth(w)
		return
	}

	start := time.Now()
	h.stats.totalRequests.Add(1)

	rawBody, err := readRequestBody(r)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to read request body: %v", err), http.StatusBadRequest)
		return
	}

	inEnc := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding")))
	sendBody := rawBody
	compressed := false

	if shouldCompress(r.Method, inEnc, len(rawBody), h.cfg.minBytes) {
		if gz, err := compressGzip(rawBody, h.cfg.gzipLevel); err == nil && len(gz) < len(rawBody) {
			sendBody = gz
			compressed = true
		}
	}

	resp, err := h.forwardOnce(r, sendBody, compressed)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		http.Error(w, fmt.Sprintf("upstream error: %v", err), http.StatusBadGateway)
		return
	}

	// Safety fallback: if upstream rejects gzipped request body, retry once uncompressed.
	if compressed && (resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnsupportedMediaType) {
		_ = resp.Body.Close()
		h.stats.fallbackCount.Add(1)
		compressed = false
		sendBody = rawBody
		resp, err = h.forwardOnce(r, sendBody, false)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			http.Error(w, fmt.Sprintf("upstream fallback error: %v", err), http.StatusBadGateway)
			return
		}
	}
	defer resp.Body.Close()

	h.stats.totalRawBytes.Add(uint64(len(rawBody)))
	h.stats.totalSentBytes.Add(uint64(len(sendBody)))
	if compressed {
		h.stats.compressedRequests.Add(1)
	}

	if h.cfg.logStats && (compressed || len(rawBody) >= h.cfg.minBytes || resp.StatusCode >= 400) {
		ratio := 100.0
		if len(rawBody) > 0 {
			ratio = float64(len(sendBody)) * 100.0 / float64(len(rawBody))
		}
		log.Printf(
			"%s %s raw=%dB sent=%dB (%.1f%%) gzip=%t status=%d %s",
			r.Method, r.URL.RequestURI(), len(rawBody), len(sendBody), ratio, compressed, resp.StatusCode, time.Since(start).Round(time.Millisecond),
		)
	}

	copyResponse(w, resp)
}

func (h *proxyHandler) serveHealth(w http.ResponseWriter) {
	raw := h.stats.totalRawBytes.Load()
	sent := h.stats.totalSentBytes.Load()
	var saved uint64
	if raw > sent {
		saved = raw - sent
	}
	ratio := 1.0
	if raw > 0 {
		ratio = float64(sent) / float64(raw)
	}
	payload := map[string]any{
		"status":              "ok",
		"uptime_sec":          int64(time.Since(h.stats.startedAt).Seconds()),
		"upstream":            h.cfg.upstreamURL.String(),
		"requests":            h.stats.totalRequests.Load(),
		"compressed_requests": h.stats.compressedRequests.Load(),
		"fallback_count":      h.stats.fallbackCount.Load(),
		"raw_bytes":           raw,
		"sent_bytes":          sent,
		"saved_bytes":         saved,
		"sent_ratio":          ratio,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

func readRequestBody(r *http.Request) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	defer r.Body.Close()
	data, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBodyBytes {
		return nil, fmt.Errorf("request body exceeds %d bytes", maxBodyBytes)
	}
	return data, nil
}

func shouldCompress(method, contentEncoding string, size, minBytes int) bool {
	if size < minBytes {
		return false
	}
	if contentEncoding != "" && contentEncoding != "identity" {
		return false
	}
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		return true
	default:
		return false
	}
}

func compressGzip(data []byte, level int) ([]byte, error) {
	var buf bytes.Buffer
	buf.Grow(len(data) / 2)
	zw, err := gzip.NewWriterLevel(&buf, level)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(data); err != nil {
		_ = zw.Close()
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (h *proxyHandler) forwardOnce(r *http.Request, body []byte, isGzip bool) (*http.Response, error) {
	target := *r.URL
	target.Scheme = h.cfg.upstreamURL.Scheme
	target.Host = h.cfg.upstreamURL.Host
	target.Path = singleJoiningSlash(h.cfg.upstreamURL.Path, r.URL.Path)

	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}

	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bodyReader)
	if err != nil {
		return nil, err
	}

	copyHeadersFiltered(upReq.Header, r.Header)
	upReq.Host = h.cfg.upstreamURL.Host

	if isGzip {
		upReq.Header.Set("Content-Encoding", "gzip")
	} else if r.Header.Get("Content-Encoding") == "" {
		upReq.Header.Del("Content-Encoding")
	}

	if len(body) > 0 || r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch {
		upReq.ContentLength = int64(len(body))
	}

	return h.transport.RoundTrip(upReq)
}

func copyHeadersFiltered(dst, src http.Header) {
	connTokens := parseConnectionTokens(src.Values("Connection"))
	for k, vals := range src {
		kl := strings.ToLower(k)
		if _, skip := hopByHopHeaders[kl]; skip {
			continue
		}
		if _, skip := connTokens[kl]; skip {
			continue
		}
		if kl == "host" || kl == "content-length" {
			continue
		}
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
}

func parseConnectionTokens(values []string) map[string]struct{} {
	tokens := make(map[string]struct{})
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			if tok := strings.ToLower(strings.TrimSpace(part)); tok != "" {
				tokens[tok] = struct{}{}
			}
		}
	}
	return tokens
}

func copyResponse(w http.ResponseWriter, resp *http.Response) {
	connTokens := parseConnectionTokens(resp.Header.Values("Connection"))
	dst := w.Header()
	for k, vals := range resp.Header {
		kl := strings.ToLower(k)
		if _, skip := hopByHopHeaders[kl]; skip {
			continue
		}
		if _, skip := connTokens[kl]; skip {
			continue
		}
		for _, v := range vals {
			dst.Add(k, v)
		}
	}

	w.WriteHeader(resp.StatusCode)

	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func singleJoiningSlash(a, b string) string {
	if a == "" || a == "/" {
		if b == "" {
			return "/"
		}
		return b
	}
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	default:
		return a + b
	}
}

func main() {
	var cfg config
	var upstreamStr string

	flag.StringVar(&cfg.listenAddr, "listen", envOrDefault("AGY_GZIP_LISTEN", defaultListen), "listen address")
	flag.StringVar(&upstreamStr, "upstream", envOrDefault("AGY_GZIP_UPSTREAM", defaultUpstream), "upstream base URL")
	flag.StringVar(&cfg.upstreamProxy, "proxy", envOrDefault("AGY_GZIP_UPSTREAM_PROXY", ""), "optional upstream proxy URL")
	flag.IntVar(&cfg.minBytes, "min-bytes", envIntOrDefault("AGY_GZIP_MIN_BYTES", 1024), "minimum request body bytes to gzip")
	flag.IntVar(&cfg.gzipLevel, "level", envIntOrDefault("AGY_GZIP_LEVEL", gzip.DefaultCompression), "gzip compression level (-1..9)")
	flag.BoolVar(&cfg.logStats, "log-stats", envBoolOrDefault("AGY_GZIP_LOG_STATS", true), "log per-request compression stats")
	flag.Parse()

	u, err := url.Parse(upstreamStr)
	if err != nil || u.Scheme == "" || u.Host == "" {
		log.Fatalf("invalid upstream URL %q: %v", upstreamStr, err)
	}
	cfg.upstreamURL = u

	handler, err := newProxyHandler(cfg)
	if err != nil {
		log.Fatalf("init proxy: %v", err)
	}

	srv := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	log.Printf("agy-gzip-proxy listening on http://%s -> %s (min=%dB, level=%d)", cfg.listenAddr, cfg.upstreamURL, cfg.minBytes, cfg.gzipLevel)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}
