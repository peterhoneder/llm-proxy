# llm-proxy

An OpenAI-compatible debugging proxy. Its one job: when a request fails, say
which side broke it — the client, the vendor, or the proxy itself.

Read `README.md` first for the reasoning. This file covers working on the code.

## The invariant everything rests on

The copy loop reads from the upstream and writes to the client, in that order:

- an error from `resp.Body.Read` is the **vendor's**
- an error from `w.Write` is the **client's**

Attribution is structural, not heuristic. Anything that blurs that ordering
breaks the product. Three supporting rules:

- **The upstream request owns its context.** It is `context.WithoutCancel` of
  the incoming request, cancelled only by us with a stamped cause. Deriving it
  from the incoming request inverts the model: a client hangup would cancel the
  upstream read and get blamed on the vendor. Go cancels a server request
  context with a bare `context.Canceled` and no cause, so our cause is the only
  reliable explanation.
- **A client watcher runs beside the copy loop.** A graceful FIN does not fail
  the next write, so a write error arrives a chunk or two late and, for the
  final chunk, never.
- **`finish_reason` proves completion, not `data: [DONE]`.** That sentinel is
  an OpenAI convention; vLLM, llama.cpp and several gateways never send it.

A confidently wrong verdict is worse than no verdict. When evidence is
unavailable — an undecodable body, a bodyless response — report that, never
guess.

## Layout

| Package | Responsibility |
|---|---|
| `internal/fault` | Side/Kind classification. The core judgement. |
| `internal/proxy` | Server, copy loop, retry, headers, transports. |
| `internal/analyze` | SSE framing and completion evidence. |
| `internal/record` | Per-request state. Mutex-guarded; snapshots are value copies. |
| `internal/obs` | Console rendering, redaction, OpenTelemetry. |
| `internal/ratelimit` | Format-tolerant rate-limit header parsing. |
| `internal/auth` | Static bearer tokens for the proxy's own listener. |
| `internal/config` | Config types, embedded defaults, validation. |
| `internal/testutil` | Scriptable fake vendor and raw HTTP client. |

Defaults live in `internal/config/default_config.yaml` and nowhere else.

## Testing

Run the whole suite, always, through the make target:

```sh
make test         # everything
make test-race    # everything, under -race
make check        # fmt + vet + lint + race tests
```

Tests drive a real proxy against a fake vendor over real TCP. The failures this
tool diagnoses only exist at the socket, so nothing below HTTP is mocked.

Two rules learned the hard way:

- **Assert on `Side`, and on a set of acceptable `Kind`s.** Whether a torn
  connection surfaces as `ECONNRESET` or a plain EOF depends on kernel timing
  and differs between macOS and Linux.
- **Test the wiring, not just the classifier.** A test that hand-builds a
  `fault.ReadState` cannot fail when the code that produces it is missing.
  `expect_done` was once fully decoded, defaulted, validated, documented and
  completely inert, and only an end-to-end test caught it.

`internal/proxy/attribution_test.go` is the core suite: each case reproduces
one real failure and asserts which side gets blamed.

`make demo-scenarios` prints every failure mode with no credentials or network,
which is how the console format gets reviewed by eye.
