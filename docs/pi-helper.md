# Pi compose helper

`runtime/cli.mjs` is a one-request JSON-lines helper. It reads exactly one UTF-8
JSON value from stdin (at most 64 KiB) and writes exactly one JSON result line.
The input is complete at EOF; stalled stdin exhausts the total budget and returns
`timeout`. It accepts no command-line input, discovers no models, and never falls
back to a model.

## Request

```json
{"version":1,"operation":"compose","model":{"provider":"openai-codex","id":"gpt-5.6-luna","thinking":"low"},"input":{"user":"...","assistant":"..."},"label":{"current":"","refresh":true}}
```

All keys are required and exact. `version` is `1`; `operation` is `compose`;
`thinking` is one of `minimal`, `low`, `medium`, `high`, `xhigh`, or `max`.
Provider and model IDs are nonempty, control-free strings of at most 256 UTF-8
bytes. Combined input is at most 8192 bytes and the current label at most 120
bytes. Empty input fields are permitted.

The whole operation, including stdin, lazy SDK loading, runtime initialization,
and completion, has one 15-second budget. After request validation it uses
`ModelRuntime.create({ signal, allowModelNetwork: false })`, resolves only the
requested provider/ID, and makes one `completeSimple` request with 256 tokens, no
retries, and the requested reasoning level. Pi remains the credential authority.
The helper does not load sessions, tools, extensions, context files, or auth
files itself.

## Results

Success is one line no larger than 4096 bytes:

```json
{"version":1,"ok":true,"body":"Plain outcome","label":"Three word label"}
```

Bodies must contain non-whitespace plain text with no controls or Markdown and
no more than 180 Unicode code points. When `refresh` is false, `label` is omitted.
When it is true, a new label has 3–4 whitespace words and at most 60 bytes;
`KEEP` retains a nonempty current label and is omitted from the result. If Pi
returns documented phased text metadata, only `final_answer` text is parsed;
commentary and thinking content are never treated as the result.

Failures are safe, non-diagnostic lines with one of `invalid_request`,
`unavailable_model`, `generation_failed`, `timeout`, or `invalid_output`.
Malformed, unreadable, or oversized input is `invalid_request`; a stalled input
or exhausted total deadline is `timeout`. No request text, credentials, provider
errors, reasoning, stack traces, or raw model output are returned.

The production entrypoint waits for the Node stdout write callback so the one
line is flushed, and then exits explicitly. This releases unfinished stdin and
provider handles after the result is delivered.

## Packaging workaround

Pi coding-agent 0.85.0 publicly imported `@earendil-works/pi-server` while
omitting it from its published manifest; 0.85.1 made the server and client
commands source-only and no longer imports it. This package still declares the
exact `0.85.1` server package, pinned in lockstep with the other Pi packages,
but nothing in the helper or the coding-agent build imports it any more. It does
not start a server or use Pi internal runtime APIs. The Nix runtime exposes this entry as
`steward-pi-helper`, an absolute Node 24 wrapper; its package smoke coverage is
documented in [Nix packaging](packaging.md).
