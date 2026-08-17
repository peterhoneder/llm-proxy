# Security

llm-proxy holds your LLM vendor API keys and sees every prompt and response
that passes through it. Two things matter.

## Don't expose it unauthenticated

The default binds to `127.0.0.1`, which is safe. If you put it anywhere else —
a tunnel, a LAN, Tailscale Funnel — require a token:

```sh
export LLM_PROXY_TOKENS=$(openssl rand -hex 32)
```

Anyone who can reach an unauthenticated proxy can spend your API credits. The
proxy warns at startup if it is listening on a non-loopback address with no
tokens set.

Note there is no exemption for local traffic, deliberately: tunnels forward to
a local port, so requests from the internet arrive looking like `127.0.0.1`.

## Keys and prompts in logs

API keys are never printed. They are redacted everywhere — console, full-trace
output and OpenTelemetry attributes — and shown only as a fingerprint
(`sk-pr…9f2c (len=51, sha256:1a2b3c4d)`), which is enough to tell two keys
apart without revealing either.

`log.full_trace` prints request and response bodies, which means your prompts.
Keys stay redacted, but be careful where that output goes.

Keep your keys in environment variables, not in `llm-proxy.yaml`. The shipped
`.gitignore` excludes `llm-proxy.yaml`, `.env` and `.envrc` for that reason.

## Reporting

Open an issue. If it is sensitive, say so without details and we'll find a
private channel.
