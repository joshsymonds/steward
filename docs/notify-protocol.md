# notify control protocol

`steward notify` and `steward notifyd` communicate over a private Unix socket.
The protocol transfers an immutable event snapshot, not native hook JSON. It is
local coordination for low-latency notification delivery, not a durable queue
or a delivery receipt.

## Framing

A request is exactly one UTF-8 JSON object followed by `\n`. The complete line,
including the newline, is at most 64 KiB. The daemon begins decoding when the
newline arrives and does not wait for EOF. A connection that never completes a
line expires at the daemon read deadline.

The request has exactly these keys:

```json
{
  "event": {
    "version": 1,
    "harness": "pi",
    "session_id": "session-id",
    "kind": "completion",
    "source_event": "TurnComplete",
    "cwd": "/work/project",
    "completion_id": "turn-id",
    "user": "latest user text",
    "assistant": "latest assistant text",
    "notification_type": "",
    "message": "",
    "agent_id": "",
    "agent_type": "",
    "goal_active": false
  },
  "workspace": "session:window",
  "dry_run": false
}
```

`dry_run` is the only optional key. All event keys and `workspace` are required,
even when their values are empty. Every scalar must have its declared JSON
type and be non-null. Unknown, duplicate, conflicting, or case-aliased keys are
rejected. So are extra JSON values on the same line, wrong JSON types, invalid
UTF-8, an unpaired UTF-16 surrogate escape, unsupported versions, invalid event
shapes, and unfinished or oversized lines. There is intentionally no
compatibility with the former `hook_input` wire shape.

`event.version` versions the request. `harness` is `claude-code` or `pi`;
`kind` is `completion`, `input`, `cleanup`, or `ignored`. Source adapters
prepare this value once. In particular, a Claude Stop scans its transcript once
to capture goal state, reliable completion identity, and the latest user and
assistant text. The daemon and inline fallback use the same prepared snapshot
and never rescan the transcript.

User and assistant text share an 8192-byte UTF-8 budget with 4096-byte
reservations that donate unused space to the other side; truncation keeps
rune-safe tails. An input message is normalized plain text of at most 160
bytes. Supplied session, completion, and agent identifiers are at most 256
bytes. Workspace is at most 256 bytes, cwd at most 4096 bytes, and source event
and notification type at most 128 bytes. Metadata rejects control characters.
An absent session or reliable completion ID is accepted as degraded input but
never shares an empty deduplication key; a completion without a complete
identity pair also skips model composition.

## Acknowledgements

The daemon validates the complete request and atomically claims an eligible
completion before replying. An acknowledgement is exactly one UTF-8 JSON line,
at most 256 bytes including its newline, and has one of these forms:

```json
{"version":1,"status":"accepted"}
{"version":1,"status":"duplicate"}
{"version":1,"status":"rejected"}
```

`accepted` means the daemon admitted the event and will process it under its
bounded graceful-shutdown tracker. It writes this acknowledgement before model
composition or ntfy delivery. `duplicate` means the same reliable completion is
already in flight or previously succeeded; the duplicate causes no model or
send work. `rejected` means the request was not admitted.

The client gives dial, write, and acknowledgement read one combined 250 ms
budget, shortened by any earlier caller deadline. Only `accepted` and
`duplicate` transfer responsibility to the daemon. Rejection, malformed,
truncated, oversized or unknown acknowledgements, disconnects, and timeouts all
cause exactly one model-free inline fallback using the same prepared snapshot.
There is no busy retry.

## In-memory completion claims

Only non-dry, structurally eligible completions with non-empty reliable identity
are claimed. The key is exactly harness, session ID, kind, and completion ID;
message text is not part of it. In-flight and successful claims deduplicate.
Different completion IDs, harnesses, or sessions remain independent.

Claims expire 24 hours after their first claim. Duplicate observations do not
refresh TTL or ordering. The daemon retains at most 10,000 claims and evicts the
oldest first; its ordering storage is bounded by the same limit. A successful
fallback send after helper failure retains the claim. A final Sender failure
releases only that job's ownership token so a later source event can retry.
SessionEnd does not clear claims.

Claims are non-durable and every daemon restart starts empty. This is deliberate:
there is no persistence, durable outbox, or cross-process delivery guarantee.

## Failure semantics

The protocol makes outages bounded and observable. It provides no delivery guarantee:

- If the socket is unavailable, the hook performs one inline fallback.
- If an accepted acknowledgement is lost, the client falls back while the
  daemon may also finish the accepted work, so duplicate delivery is possible.
- If notifyd crashes after `accepted` was observed but before delivery, the
  client does not fall back and that notification can be lost.
- After restart, the empty claim store can admit an event delivered before the
  crash, so a later source retry can duplicate an earlier successful send.
- notifyd shutdown cancels composition and waits only a bounded interval for
  tracked handlers to finish deterministic fallback delivery.

Pi composition is daemon-only and attempted at most once per accepted reliable
completion. It cannot alter eligibility or urgency. Helper failure uses captured
assistant text; Sender retains its existing bounded HTTP retry behavior. Logs
contain fixed status/error categories plus validated source metadata and IDs,
never native payloads, prompt bodies, credentials, or raw errors.
