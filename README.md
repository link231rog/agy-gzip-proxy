# agy-gzip-proxy

<div align="center">

[![Go](https://img.shields.io/badge/Go-1.21%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![Dependencies](https://img.shields.io/badge/dependencies-zero-brightgreen.svg)]()
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Status: Proof of Concept](https://img.shields.io/badge/status-proof--of--concept%20%2F%20use%20at%20own%20risk-orange.svg)]()

</div>

> [!CAUTION]
> **Terms of Service & Account Risk Warning / 服务条款与封号风险提示**
>
> Google's [Antigravity Additional Terms of Service (Section 6)](https://antigravity.google/terms) and [FAQ](https://antigravity.google/docs/faq/) prohibit accessing the service through third-party software, wrappers, or proxies that intercept or alter official client requests/credentials. Routing Antigravity through local reverse proxies has triggered automated `403 PERMISSION_DENIED` (`TOS_VIOLATION`) suspensions (with a strict one-time appeal policy; repeat violations result in permanent loss of Antigravity / Gemini CLI access).
>
> **Do not run this proxy against a primary personal Google subscription account (AI Pro / Ultra).** This repository is published purely as a technical proof-of-concept demonstrating that `daily-cloudcode-pa.googleapis.com` natively accepts `Content-Encoding: gzip` request bodies—and why upstream CLI clients should implement request compression natively rather than users deploying local MITM/reverse proxies.
>
> **风险提示**：[Google Antigravity 附加服务条款](https://antigravity.google/terms)第 6 条与官方 FAQ 明确限制通过第三方包装器或代理转发 Antigravity 端点；2026 年 11 月已出现本地反代触发 `403 TOS_VIOLATION` 封号案例（且每个账号仅限申诉解封一次）。**请勿在主力个人订阅账号上长期挂用本反代。** 本仓库仅作为“后端原生支持 `Content-Encoding: gzip`、建议官方在客户端内原生开启请求压缩”的技术验证归档。

A lightweight, zero-dependency Go reverse proxy that compresses outbound Google Antigravity CLI (`agy`) request bodies with `Content-Encoding: gzip` via `CLOUD_CODE_URL`, cutting full-context upload traffic by **~64%** without modifying the `agy` binary.

> **中文简介**：`agy` 在每次工具调用和对话轮次中都会将完整上下文以**未压缩明文 JSON**（`identity`）重传给后端（即便命中 Prompt Caching，省的也只是服务端 Token 计费而非上行流量）。实测表明 Google 后端 `daily-cloudcode-pa.googleapis.com` 原生支持 `Content-Encoding: gzip` 请求体；`agy-gzip-proxy` 通过纯 Go 标准库在本地透明压缩并复用 HTTP/2 连接池，可将 `agy` 文本/代码会话的真实外网上传流量削减约 **64%**，且支持 `400/415` 自动回退明文与 macOS `LaunchAgent` 后台保活。

---

## Why This Exists

1. **Full-context replay is uncompressed by default**: Native `agy` sends `/v1internal:streamGenerateContent?alt=sse` payloads with no `Content-Encoding` (`identity`). In long sessions or multi-window workflows, every single step re-uploads hundreds of KB to several MB of JSON history.
2. **Prompt caching does not reduce upload bytes**: Even when `cache_read_tokens > 0`, the client still uploads the full request body over your network / proxy.
3. **Upstream natively accepts gzip requests**: Pointing `CLOUD_CODE_URL` at a local compressor that sets `Content-Encoding: gzip` works seamlessly with live `daily-cloudcode-pa.googleapis.com` endpoints.

### Measured Live Results (`agy 1.5.12` -> `daily-cloudcode-pa.googleapis.com`)

| Workload | Raw Request | Gzipped Wire Size | Saved | Status |
| :--- | ---: | ---: | ---: | :---: |
| Single-turn prompt (`streamGenerateContent?alt=sse`) | `52,877 B` | `19,107 B` (`36.1%`) | **63.9%** | `200` |
| Multi-turn tool call — Step 1 | `49,572 B` | `17,914 B` (`36.1%`) | **63.9%** | `200` |
| Multi-turn tool call — Step 2 (after file read) | `50,547 B` | `18,348 B` (`36.3%`) | **63.7%** | `200` |
| Live `LaunchAgent` Go run via `~/.zshrc` wrapper | `49,271 B` | `17,953 B` (`36.4%`) | **63.6%** | `200` (`2.84s`) |

*(Note: Pure text/JSON payloads typically compress to ~30%–40% of original size; inline Base64 images already compressed as PNG/JPEG compress by ~35%–40%.)*

---

## How It Works

```text
+-----+   http://127.0.0.1:18319    +----------------+   HTTPS + HTTP/2 pool    +------------------------------------+
| agy | --------------------------> | agy-gzip-proxy | -----------------------> | daily-cloudcode-pa.googleapis.com  |
+-----+   (uncompressed JSON)       +----------------+   Content-Encoding: gzip +------------------------------------+
                                           |
                                           +-- on HTTP 400/415: automatic 1x retry uncompressed
                                           +-- SSE chunks flushed immediately to client
```

### Key Features

- **Zero external dependencies**: Pure Go standard library (`net/http`, `compress/gzip`).
- **HTTP/2 & connection pooling**: Reuses persistent TLS connections across concurrent `agy` sessions instead of paying a fresh TLS handshake per request.
- **Automatic uncompressed fallback**: If upstream ever returns `400` or `415` for a compressed request, retries once with the original `identity` body.
- **True SSE streaming & response passthrough**: Flushes upstream chunks immediately without buffering, and disables Go's transparent response decompression (`DisableCompression: true`) so `Content-Encoding: gzip` responses pass through untouched.
- **Privacy-safe telemetry**: Logs only HTTP method, path, byte counts, ratio, status, and latency; never logs `Authorization` headers, tokens, or payloads.

---

## Quick Start

### Option A: macOS (`LaunchAgent` auto-start, recommended)

```bash
git clone https://github.com/link231rog/agy-gzip-proxy.git
cd agy-gzip-proxy
./install.sh
```

`install.sh` builds the binary to `~/.local/bin/agy-gzip-proxy`, creates and loads `~/Library/LaunchAgents/io.github.agy-gzip-proxy.plist` on `127.0.0.1:18319` (`KeepAlive` + `RunAtLoad`), and verifies `/_agy_gzip_proxy/healthz`.

Next, add a safe health-checked wrapper to `~/.zshrc` (if `agy-gzip-proxy` is ever stopped, `agy` automatically falls back to direct connection):

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

Reload your shell (`source ~/.zshrc`). Existing already-running `agy` processes keep their original environment; newly launched `agy` sessions automatically route through the proxy.

### Option B: Direct Go Install / Linux / Foreground Run

```bash
go install github.com/link231rog/agy-gzip-proxy@latest
agy-gzip-proxy -listen 127.0.0.1:18319
```

Then run `agy` with:

```bash
CLOUD_CODE_URL="http://127.0.0.1:18319" agy
```

---

## Monitor & Verify

Check cumulative compression stats:

```bash
curl -s http://127.0.0.1:18319/_agy_gzip_proxy/healthz
```

Example response:

```json
{
  "compressed_requests": 1,
  "fallback_count": 0,
  "raw_bytes": 49748,
  "requests": 16,
  "saved_bytes": 31318,
  "sent_bytes": 18430,
  "sent_ratio": 0.37046715445847067,
  "status": "ok",
  "upstream": "https://daily-cloudcode-pa.googleapis.com",
  "uptime_sec": 44
}
```

Inspect live compression logs (macOS LaunchAgent):

```bash
tail -f ~/Library/Logs/agy-gzip-proxy.log
```

---

## Configuration

All options can be supplied via CLI flags or environment variables:

| Flag | Environment Variable | Default | Description |
| :--- | :--- | :--- | :--- |
| `-listen` | `AGY_GZIP_LISTEN` | `127.0.0.1:18319` | Local listen address |
| `-upstream` | `AGY_GZIP_UPSTREAM` | `https://daily-cloudcode-pa.googleapis.com` | Upstream Cloud Code base URL |
| `-proxy` | `AGY_GZIP_UPSTREAM_PROXY` | `""` (`http.ProxyFromEnvironment`) | Optional explicit upstream proxy URL (e.g. `http://127.0.0.1:7897` if not using TUN mode) |
| `-min-bytes` | `AGY_GZIP_MIN_BYTES` | `1024` | Skip gzip for bodies smaller than this threshold |
| `-level` | `AGY_GZIP_LEVEL` | `-1` (`gzip.DefaultCompression`) | Gzip compression level (`-1..9`) |
| `-log-stats` | `AGY_GZIP_LOG_STATS` | `true` | Log per-request byte savings |

---

## Limitations

- **Reduces wire upload size, not context replay semantics**: `agy` still serializes full history on each turn; use `/compact` or start fresh sessions for long-running tasks, and avoid leaving large images in multi-turn histories.
- **Only affects `CLOUD_CODE_URL` traffic**: Other endpoints outside `CLOUD_CODE_URL` (such as telemetry or auxiliary endpoints) are unaffected.
- **128 MiB body cap**: Requests above `128 MiB` are rejected to bound proxy memory (`agy` itself caps inline attachments far below this).

---

## Development & Uninstall

Run the test suite:

```bash
go test -v ./...
```

Uninstall the macOS LaunchAgent and binary:

```bash
./uninstall.sh
```

Then remove the `# >>> agy-gzip-proxy >>>` block from `~/.zshrc`.

---

## License

[MIT](LICENSE)
