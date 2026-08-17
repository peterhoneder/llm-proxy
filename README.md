# llm-proxy

When an LLM request fails, you usually can't tell whose fault it was.

Your coding agent stops mid-answer. Was the model cut off? Did the vendor drop
the connection? Did your own tool hang up? The client library reports "stream
ended" and nothing else. So you retry, it works, and you learn nothing.

llm-proxy sits between your tool and the LLM API and answers that one question:

```
ERR ← scaleway  UPSTREAM TRUNCATED  200 OK  8.02s  87 chunks  req=r-00044
     fault      side=upstream  kind=upstream_truncated_body  op=read
     cause      the upstream closed the connection mid-body after 87 events and 22.9 kB
     partial    19 bytes unterminated: `data: {"id":"chatcmp`
     conn       reused=false · alpn=http/1.1 · peer=51.159.13.90:443 · tls1.3 · ttfb=480ms
     verdict    the vendor cut the response mid-frame. Your tool will see an incomplete reply.
```

Note the `200 OK`. The vendor said everything was fine.

## Why it exists

Three problems, all invisible from inside a client library:

**You can't tell the client from the server.** A dropped stream looks identical
from both directions. llm-proxy watches both ends of the connection, so it can
say which one stopped first.

**Failures hide behind success.** A stream that dies mid-answer still carries a
`200`. A response cut off by `max_tokens` looks like a short reply. A vendor
that crashes mid-stream may write an HTML error page into the middle of your
SSE stream, and your client will treat it as content.

**Errors get swallowed.** Client libraries normalise vendor errors into their
own exception types. Rate-limit headers, request ids, the vendor's actual
message: all gone by the time you see it. llm-proxy forwards bytes unaltered
and prints what the vendor really said.

## What you get

- **A verdict on every failure.** Every fault report names a side — client,
  vendor, or the proxy itself — and says why in plain English.
- **Untouched passthrough.** Bytes go through byte-for-byte. No retries, no
  rewriting, no normalising, unless you ask for them.
- **Connection-level detail.** DNS, TCP, TLS, connection reuse, time to first
  byte, gaps between chunks.
- **The vendor's own words.** Error bodies, status codes and rate-limit headers
  verbatim, including headers it doesn't document.
- **Several backends, one process.** Each route gets a URL prefix and its own
  API key.
- **OpenTelemetry**, if you want it. Off unless you configure an endpoint.

## Install

Needs Go 1.26.5 or later.

```sh
git clone https://github.com/peterhoneder/llm-proxy && cd llm-proxy
make build
```

## Use

```sh
cp llm-proxy.example.yaml llm-proxy.yaml
$EDITOR llm-proxy.yaml
export SCALEWAY_API_KEY=...        # whatever your routes reference
make run
```

Point your tool at the route instead of the vendor:

```sh
export OPENAI_BASE_URL=http://127.0.0.1:14701/scaleway/v1
```

That's it. Requests now flow through the proxy and every one gets a line.

To see the output without setting anything up — no credentials, no network:

```sh
make demo-scenarios
```

## Configure

```yaml
listen: "127.0.0.1:14701"

routes:
  - name: scaleway
    upstream: https://api.scaleway.ai      # no /v1 — your client adds that
    api_key_env: SCALEWAY_API_KEY

  - name: mistral
    upstream: https://api.mistral.ai
    api_key_env: MISTRAL_API_KEY
```

Each route becomes a URL prefix, so `/scaleway/v1/...` goes to Scaleway and
`/mistral/v1/...` goes to Mistral. Your API keys stay in environment variables
and never appear in the config file or the logs.

Run `llm-proxy -check` to validate the file and print what it resolved to.

Flags: `-config`, `-listen`, `-log-level`, `-full-trace`, `-json`, `-no-color`,
`-check`, `-version`. Environment overrides: `LLM_PROXY_CONFIG`,
`LLM_PROXY_LISTEN`, `LLM_PROXY_LOG_LEVEL`, `LLM_PROXY_FULL_TRACE`,
`LLM_PROXY_TOKENS`, `NO_COLOR`.

Everything else has a default. The settings worth knowing:

| Setting | Default | |
|---|---|---|
| `log.full_trace` | off | Every header, body and SSE frame with arrival times. Keys stay redacted. |
| `routes[].retry` | off | Retry 429s and 5xx, honouring `Retry-After`. Off means transparent. |
| `routes[].timeouts.*` | no limit | See [Waiting](#waiting). |
| `auth.tokens` | none | See [Exposing it](#exposing-it). `auth.enabled: false` ignores tokens and the environment entirely. |

## Waiting

**The proxy never gives up before the vendor does.** A reasoning model can take
ten minutes to its first token. A proxy that quits at two minutes doesn't
observe the failure, it *becomes* the failure, and hides which side would
really have quit first.

So there are no deadlines after the connection is established. While you wait,
it tells you it's waiting:

```
WRN still waiting for the upstream's first byte  route=scaleway request=r-00042 waited=8m30s
```

If you do want a deadline, set one:

```yaml
timeouts:
  response_header: 5m    # no status line within 5 minutes -> give up
  stream_idle: 2m        # 2 minutes of mid-stream silence -> give up
```

A deadline the proxy enforces still blames the vendor for going silent, and the
report names the setting that fired, so you can tell it apart from a real
vendor failure.

## Exposing it

With no tokens configured the proxy is open, which is fine on `127.0.0.1`. If
you expose it — Tailscale Funnel, a tunnel, a LAN — require a token:

```sh
export LLM_PROXY_TOKENS=$(openssl rand -hex 32)
```

Clients send it where they'd normally put an API key:

```sh
export OPENAI_API_KEY=$LLM_PROXY_TOKENS     # the proxy's token, not the vendor's
```

The proxy checks it, strips it, and substitutes the route's real key. Your
vendor key never leaves the machine.

Two things to know:

**Local traffic is not exempt.** Tailscale Funnel forwards to a local port, so
requests from the internet arrive looking like `127.0.0.1`. Trusting local
traffic would trust exactly what you're guarding against.

**It fails closed.** If a token points at an environment variable that isn't
set, the proxy refuses to start rather than coming up unprotected.

This is authentication, not authorization: every valid token can use every
route.

## What it reports

| | |
|---|---|
| Clean request | One line: status, duration, TTFB, chunks, tokens, finish reason |
| Client hung up | `side=client`, when it went away, and that the vendor was fine |
| Vendor cut the stream | `side=upstream`, with the unterminated bytes as proof |
| Vendor went silent | `side=upstream`, naming the deadline that fired |
| Stale keep-alive | Flagged as a connection race, not an outage |
| Error inside a `200` stream | The vendor's error, quoted |
| Rate limited | Every bucket, its reset format, and the retry arithmetic |
| Context window exceeded | The vendor's own message, and which rule matched it |
| `finish_reason=length` | A warning: the protocol was fine, the answer is cut off |
| Proxy shut down | `side=proxy`, explicitly not the vendor's fault |

Real examples with explanations are in [`samples/`](samples/).

## How it works

The proxy writes its own copy loop instead of using `httputil.ReverseProxy`,
because that's what makes attribution structural rather than guesswork:

```go
n, rerr := resp.Body.Read(buf)   // an error here is the VENDOR's
...
nw, werr := w.Write(buf[:n])     // an error here is the CLIENT's
```

Read first, write second. Three details make it hold up:

**The upstream request has its own context**, detached from the incoming
request, cancelled only with a recorded reason. The obvious approach — derive
it from the incoming request — inverts everything: a client hangup would cancel
the upstream read and get blamed on the vendor.

**A watcher runs alongside the copy loop.** A client closing gracefully doesn't
fail the proxy's next write, so waiting for a write error notices one or two
chunks late, and for the final chunk, never. Watching the request context
catches it immediately, including a client that leaves while the proxy is
blocked reading a silent vendor.

**`finish_reason` is the proof a stream finished, not `data: [DONE]`.** That
sentinel is an OpenAI convention; vLLM, llama.cpp and several gateways never
send it. Treating its absence as truncation would flag every healthy request
against those backends.

## Endpoints

Everything under a route prefix is proxied, not just chat completions: your
tool probably calls `/v1/models` on startup. Only `POST .../chat/completions`
gets body parsing and stream analysis.

The proxy serves two paths of its own:

- `GET /_proxy/healthz` — open, so uptime checks need no token
- `GET /_proxy/routes` — the route table; requires a token when auth is on

## Development

```sh
make test         # everything
make test-race    # everything, with the race detector
make check        # fmt + vet + lint + race tests
make demo-scenarios
make hooks        # pre-commit secret scan, needs gitleaks
```

Tests run a real proxy against a scriptable fake vendor over real TCP. Nothing
below the HTTP layer is mocked, because the failures this tool diagnoses only
exist at the socket. `internal/proxy/attribution_test.go` is the core: each
test reproduces one real failure and asserts which side gets blamed.

## Limits

- Chat Completions only. Not the Responses API.
- No load balancing, failover or model routing. One route, one backend.
- Authentication is a shared token, not per-user access control.
- Token counts come from the vendor. Nothing is counted locally.
- HTTP/1.1 to the vendor by default: its failures are unambiguous, where
  HTTP/2 reports the same thing as a stream error or GOAWAY. Enable h2 per
  route if you need it.

## License

Apache License 2.0. See [LICENSE](LICENSE).
