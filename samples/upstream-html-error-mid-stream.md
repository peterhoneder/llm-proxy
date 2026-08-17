# Vendor injected an HTML error page into a live SSE stream

**Verdict:** upstream fault. The vendor's backend crashed mid-generation and its
web framework rendered an HTML error page into a response that had already
committed to `200 OK` and `Content-Type: text/event-stream`.

The vendor is anonymised here. What matters is the shape of the failure, which
any backend can produce; naming one provider over a single unreproduced
incident would not help anyone reading this.

**Why it matters:** an OpenAI client sees a 200, no exception, and a short
answer. Nothing downstream flags it. This is the failure llm-proxy exists to
make visible.

---

## The log

```
18:33:58.754 INF → vendor-a  POST /vendor-a/v1/chat/completions  chat moonshotai/Kimi-K3  stream  msgs=38  req=r-00014
18:34:23.002 ERR ← vendor-a  UPSTREAM TRUNCATED  200 OK  24.25s  ttfb=3.54s  45 chunks  req=r-00014
                 request     POST /vendor-a/v1/chat/completions  chat moonshotai/Kimi-K3  stream  msgs=38  body=148.7 kB  max_tokens=32768
                 fault       side=upstream  kind=upstream_truncated_body  op=read
                 cause       the stream ended mid-frame with 226 unterminated bytes, after 45 events and 17.6 KiB
                 error       unexpected EOF
                 timing      last upstream byte 284µs ago · last downstream write 227µs ago
                 stream      45 events  · 6 reads parsed  · 6 delivered  · 17.7 kB read / 17.7 kB written  · max gap 10.35s after event 31  · no [DONE]
                 partial     226 bytes unterminated: `<div class="ml-4 text-lg text-gray-500 uppercase tracking-wider">                         Server Error                  …`
                 conn        reused=false  · peer=<redacted>:443  · tls1.3  · ttfb=3.33s
                 ratelimit   requests      59/60 left
                 clock-skew  1.30s between this host and the vendor
                 response    Access-Control-Allow-Headers: Content-Type, Authorization, X-Requested-With
                             Access-Control-Allow-Methods: GET, POST, OPTIONS, PUT, DELETE
                             Access-Control-Allow-Origin: *
                             Cache-Control: no-cache, private
                             Connection: keep-alive
                             Content-Type: text/event-stream; charset=utf-8
                             Date: Mon, 17 Aug 2026 16:34:01 GMT
                             Server: nginx
                             X-Content-Type-Options: nosniff
                             X-Frame-Options: SAMEORIGIN
                             X-Ratelimit-Limit: 60
                             X-Ratelimit-Remaining: 59
                             X-Request-Id: <redacted>
                             X-Xss-Protection: 1; mode=block
                 verdict     the vendor cut the response mid-frame. Your tool will see an incomplete reply.
```

---

## The smoking gun

The `partial` line:

```
<div class="ml-4 text-lg text-gray-500 uppercase tracking-wider">  Server Error
```

The vendor streamed 45 SSE events correctly, went quiet for 10 seconds, then
wrote **HTML into the middle of the event stream** and dropped the connection.
The HTML is a web framework's default error page, rendered into a response that
had already committed to streaming.

## Reading the rest

| Field | What it tells you |
|---|---|
| `max gap 10.35s after event 31` | The stream ran fine for 31 events, then stalled 10s, then died. That is a backend hitting trouble and timing out internally — not a network blip. |
| `timing: last upstream byte 284µs ago` | The connection died *while data was flowing*, microseconds before the failure. With `stream_idle` unlimited, this rules out anything on our side giving up. |
| `error: unexpected EOF` | The HTTP chunked framing itself was cut, not just the SSE content. The connection was severed mid-write. |
| `conn: reused=false` | A fresh connection, so this is not a stale keep-alive race — the benign explanation is ruled out. |
| `ratelimit: 59/60 left` | Not throttled. |
| `17.7 kB read / 17.7 kB written` | Everything the vendor sent was forwarded. Nothing was lost in the proxy. |
| `ttfb=3.54s` vs `24.25s` total | Headers came back quickly; the failure was well into generation. |

## What to do

Report it to the vendor. Most return a correlation id in a response header
(`X-Request-Id` here) — quote it along with the UTC timestamp from `Date:`, as
that is what they need to find the request in their own logs.

Then check whether it reproduces. 38 messages, a 148.7 kB body and
`max_tokens=32768` is a heavy request, and backends commonly fall over on large
contexts specifically. Retrying with a shorter context distinguishes "this
vendor is flaky" from "this vendor cannot handle this request size".

## Notes on the tool's own output

- **The headline could be sharper.** The analyzer detects vendor errors
  delivered as SSE frames (`data: {"error":…}`) and reports
  `upstream_error_in_stream`. This was raw HTML, so it fell through to generic
  truncation detection. The verdict is correct and the evidence is in
  `partial`, but a frame starting with `<` in a `text/event-stream` response is
  never legitimate and could be named as such.
- **`clock-skew 1.30s` is mostly measurement artifact.** It is computed as
  *(local time when the headers were parsed) − (the vendor's `Date` header)*,
  and the vendor stamps `Date` when it starts generating. With `ttfb=3.54s` and
  HTTP dates having one-second resolution, the residual is close to noise. It
  only matters when a vendor sends `Retry-After` as an HTTP date.
