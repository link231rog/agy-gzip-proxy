# agy-gzip-proxy

A lightweight, zero-dependency Go reverse proxy that compresses outbound Google Antigravity CLI (`agy`) request bodies with `Content-Encoding: gzip` via `CLOUD_CODE_URL`.

## Why

Stateless LLM CLI agents re-upload the entire conversation history (system prompt, tool declarations, prior tool outputs, and inline media) on every turn, and native `agy` sends `v1internal:streamGenerateContent` payloads uncompressed (`identity`). Even when server-side prompt caching hits (`cache_read_tokens > 0`), the client still re-uploads the full JSON body over the network.

Empirically against `daily-cloudcode-pa.googleapis.com`, the endpoint natively accepts `Content-Encoding: gzip` request bodies, cutting text/code upload bandwidth by **~64%** (typically `2.7×–3.5×` smaller wire payload) with no binary modification.

## Features

- **Zero dependencies**: pure Go standard library (`net/http`, `compress/gzip`).
- **Connection pooling & HTTP/2**: reuses persistent TLS/HTTP2 connections across concurrent `agy` sessions.
- **Automatic fallback**: if upstream ever returns `400` or `415` on a compressed request, retries once uncompressed.
- **True SSE streaming**: flushes chunks immediately (`FlushInterval`-free direct read loop) and preserves upstream `Content-Encoding` on responses (`DisableCompression: true`).
- **Privacy-safe telemetry**: logs only path, raw/sent byte counts, ratio, status, and duration; never logs auth headers, tokens, or payloads.

## Quick Start (macOS LaunchAgent)

```bash
git clone <your-repo-url> agy-gzip-proxy
cd agy-gzip-proxy
./install.sh
```

Add a safe health-checked wrapper to `~/.zshrc` (falls back seamlessly to direct connection if the proxy is stopped):

```zsh
# >>> agy-gzip-proxy >>>
unalias agy 2>/dev/null || true
agy() {
  if [ -z "${CLOUD_CODE_URL:-}" ] && nc -z 127.0.0.1 18319 2>/dev/null; then
    CLOUD_CODE_URL="http://127.0.0.1:18319" command agy "$@"
  else
    command agy "$@"
  fi
}
# <<< agy-gzip-proxy <<<
```

*(If you previously aliased `agy` with extra flags like `--dangerously-skip-permissions`, include them after `command agy`.)*

## Verify & Monitor

```bash
# Check proxy status and cumulative saved bytes
curl -s http://127.0.0.1:18319/_agy_gzip_proxy/healthz

# View live compression logs
tail -f ~/Library/Logs/agy-gzip-proxy.log
```

## Configuration

All options can be passed via CLI flags or environment variables:

| Flag | Env Var | Default | Description |
| :--- | :--- | :--- | :--- |
| `-listen` | `AGY_GZIP_LISTEN` | `127.0.0.1:18319` | Local bind address |
| `-upstream` | `AGY_GZIP_UPSTREAM` | `https://daily-cloudcode-pa.googleapis.com` | Upstream Cloud Code endpoint |
| `-proxy` | `AGY_GZIP_UPSTREAM_PROXY` | `""` (uses `HTTPS_PROXY` / system TUN) | Optional explicit upstream proxy URL |
| `-min-bytes` | `AGY_GZIP_MIN_BYTES` | `1024` | Skip gzip below this payload size |
| `-level` | `AGY_GZIP_LEVEL` | `-1` (`gzip.DefaultCompression`) | Gzip level (`-1..9`) |
| `-log-stats` | `AGY_GZIP_LOG_STATS` | `true` | Log per-request byte savings |

## Testing & Uninstall

```bash
go test -v ./...
./uninstall.sh
```
