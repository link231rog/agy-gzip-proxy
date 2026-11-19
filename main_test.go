package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func mustProxy(t *testing.T, upstreamURL string, minBytes int) *proxyHandler {
	t.Helper()
	u, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	h, err := newProxyHandler(config{
		upstreamURL: u,
		minBytes:    minBytes,
		gzipLevel:   gzip.DefaultCompression,
		logStats:    false,
	})
	if err != nil {
		t.Fatalf("newProxyHandler: %v", err)
	}
	return h
}

func TestGzipLargePostAndSSERoundtrip(t *testing.T) {
	payload := strings.Repeat(`{"role":"user","text":"hello agy context compression"}`+"\n", 80)

	var seenEncoding string
	var seenDecoded string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenEncoding = r.Header.Get("Content-Encoding")
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		if seenEncoding == "gzip" {
			zr, err := gzip.NewReader(bytes.NewReader(raw))
			if err != nil {
				t.Fatalf("gzip.NewReader: %v", err)
			}
			defer zr.Close()
			dec, err := io.ReadAll(zr)
			if err != nil {
				t.Fatalf("gunzip: %v", err)
			}
			seenDecoded = string(dec)
		} else {
			seenDecoded = string(raw)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: ok\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer upstream.Close()

	h := mustProxy(t, upstream.URL, 512)
	h.transport.TLSClientConfig = upstream.Client().Transport.(*http.Transport).TLSClientConfig
	proxySrv := httptest.NewServer(h)
	defer proxySrv.Close()

	resp, err := http.Post(proxySrv.URL+"/v1internal:streamGenerateContent?alt=sse", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("POST proxy: %v", err)
	}
	defer resp.Body.Close()
	bodyBytes, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if seenEncoding != "gzip" {
		t.Fatalf("expected upstream Content-Encoding=gzip, got %q", seenEncoding)
	}
	if seenDecoded != payload {
		t.Fatalf("decoded payload mismatch (len %d vs %d)", len(seenDecoded), len(payload))
	}
	if string(bodyBytes) != "data: ok\n\n" {
		t.Fatalf("unexpected response body %q", string(bodyBytes))
	}
	if h.stats.compressedRequests.Load() != 1 {
		t.Fatalf("expected 1 compressed request, got %d", h.stats.compressedRequests.Load())
	}
	if h.stats.totalSentBytes.Load() >= uint64(len(payload)) {
		t.Fatalf("expected compressed sent bytes < raw (%d >= %d)", h.stats.totalSentBytes.Load(), len(payload))
	}
}

func TestTinyBodySkipsGzip(t *testing.T) {
	var seenEncoding string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenEncoding = r.Header.Get("Content-Encoding")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	h := mustProxy(t, upstream.URL, 1024)
	h.transport.TLSClientConfig = upstream.Client().Transport.(*http.Transport).TLSClientConfig
	proxySrv := httptest.NewServer(h)
	defer proxySrv.Close()

	resp, err := http.Post(proxySrv.URL+"/v1internal:loadCodeAssist", "application/json", strings.NewReader(`{"project":"test"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if seenEncoding != "" {
		t.Fatalf("expected no Content-Encoding for tiny body, got %q", seenEncoding)
	}
	if h.stats.compressedRequests.Load() != 0 {
		t.Fatalf("expected 0 compressed requests, got %d", h.stats.compressedRequests.Load())
	}
}

func TestFallbackOn415(t *testing.T) {
	payload := strings.Repeat("compressible-text-", 100)
	var calls atomic.Int32

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if r.Header.Get("Content-Encoding") == "gzip" {
			w.WriteHeader(http.StatusUnsupportedMediaType)
			_, _ = w.Write([]byte("gzip rejected"))
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if string(raw) != payload {
			t.Fatalf("fallback call %d body mismatch", n)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("fallback-ok"))
	}))
	defer upstream.Close()

	h := mustProxy(t, upstream.URL, 256)
	h.transport.TLSClientConfig = upstream.Client().Transport.(*http.Transport).TLSClientConfig
	proxySrv := httptest.NewServer(h)
	defer proxySrv.Close()

	resp, err := http.Post(proxySrv.URL+"/v1internal:test", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK || string(out) != "fallback-ok" {
		t.Fatalf("unexpected fallback result: status=%d body=%q", resp.StatusCode, string(out))
	}
	if calls.Load() != 2 || h.stats.fallbackCount.Load() != 1 {
		t.Fatalf("expected 2 upstream calls and 1 fallback, got calls=%d fallback=%d", calls.Load(), h.stats.fallbackCount.Load())
	}
}

func TestResponseGzipPassthroughAndHealthz(t *testing.T) {
	var gzBuf bytes.Buffer
	zw := gzip.NewWriter(&gzBuf)
	_, _ = zw.Write([]byte(`{"status":"upstream-gz"}`))
	_ = zw.Close()

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(gzBuf.Bytes())
	}))
	defer upstream.Close()

	h := mustProxy(t, upstream.URL, 1024)
	h.transport.TLSClientConfig = upstream.Client().Transport.(*http.Transport).TLSClientConfig
	proxySrv := httptest.NewServer(h)
	defer proxySrv.Close()

	req, _ := http.NewRequest(http.MethodGet, proxySrv.URL+"/v1internal:check", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	// Disable auto-decompression on test client to verify wire Content-Encoding stays intact.
	tr := &http.Transport{DisableCompression: true}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("expected Content-Encoding=gzip preserved, got %q", resp.Header.Get("Content-Encoding"))
	}
	wireBytes, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(wireBytes, gzBuf.Bytes()) {
		t.Fatalf("gzipped response bytes altered")
	}

	hzResp, err := http.Get(proxySrv.URL + healthPath)
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	defer hzResp.Body.Close()
	var hz map[string]any
	if err := json.NewDecoder(hzResp.Body).Decode(&hz); err != nil {
		t.Fatalf("decode healthz: %v", err)
	}
	if hz["status"] != "ok" {
		t.Fatalf("unexpected healthz status: %v", hz)
	}
}
