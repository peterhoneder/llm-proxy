# samples

Real failures caught in production, each with the console output verbatim and a
reading of what it meant.

These are reference material, not tests. The point is recognition: when the same
shape shows up again months later, the diagnosis should take seconds rather than
starting from scratch. They also serve as a record of whether the tool's verdict
was actually right — and, where it was not sharp enough, what would have been
better.

Each sample carries the log block, a line-by-line reading, and what to do about
it. Vendors are anonymised and identifiers redacted: the value is in the shape
of the failure, not in who produced it that day.

| Sample | Side | Kind |
|---|---|---|
| [upstream-html-error-mid-stream](upstream-html-error-mid-stream.md) | upstream | `upstream_truncated_body` — vendor wrote an HTML error page into a live SSE stream |
