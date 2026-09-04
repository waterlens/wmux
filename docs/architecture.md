# wmux architecture

## Components

```text
Browser / PWA
  ├─ JSON API ───────────────┐
  └─ binary WebSocket ───┐   │
                         ▼   ▼
                    Go HTTP server
                     ├─ authentication
                     ├─ host/session API
                     └─ terminal manager
                         ├─ local PTY → tmux / screen / shell
                         ├─ Go SSH client → remote tmux / screen / shell
                         └─ sequence transcript

                    SQLite + encrypted credentials + JSONL recordings
```

The browser never receives stored SSH credentials. It asks the terminal manager to attach a session identified by an opaque ID. Each running session owns one backend PTY or SSH channel and fans output out to attached WebSockets.

## Persistence layers

Persistence is intentionally split into three independent concerns:

- Process persistence is provided by tmux or screen on the machine where the shell runs.
- Output continuity is provided by monotonically increasing sequence numbers and a bounded JSONL transcript.
- Product state is stored in SQLite: names, targets, dimensions, backend identifiers and timestamps.

The Go process detaches from persistent multiplexers during graceful shutdown. A user-initiated session deletion explicitly terminates only the corresponding multiplexer session and removes its recording. tmux and screen run with wmux-owned namespaces and minimal configuration, so user configuration, status bars, key bindings and unrelated sessions are isolated.

Only one wmux process may hold a data directory at a time. A cross-process lock prevents two managers from attaching the same multiplexer and appending conflicting transcript sequences.

## WebSocket protocol

The endpoint is `/ws/sessions/{id}?since={sequence}` and uses the authenticated HTTP-only cookie.

Client binary input starts with byte `0x00`; all remaining bytes are terminal input. Server binary output starts with byte `0x01`, followed by an unsigned 64-bit big-endian sequence number and the terminal bytes.

Control messages use JSON:

```json
{"type":"resize","cols":120,"rows":32}
{"type":"take_control"}
```

The server sends `hello`, `state`, `writer`, `disconnect` and `error` messages. The first attachment receives the write lease. Another attachment may explicitly take it; both clients receive the writer change immediately and input from read-only attachments is ignored. When `since` predates retained output, `hello.sequence` reports the oldest available sequence and `hello.truncated` is true.

Attachment closure is explicit: a real process exit sends `state: exited` and closes normally; `disconnect` with `reason: server_shutdown` closes with code 1012, while `reason: evicted` closes with code 1013. Browsers reconnect for the latter two and never suggest restarting (and therefore killing) a persistent backend merely because the transport was interrupted.

## SSH trust

Creating a host does not silently trust it. wmux first probes the public host key and shows its SHA-256 fingerprint. Trusting it stores that fingerprint. Every authenticated connection compares the presented key to the stored value; a changed key is rejected until the user probes and explicitly trusts the new fingerprint.

## HTTP security

- Passwords use a versioned scrypt hash with a random salt.
- Login tokens are random values; SQLite stores only their SHA-256 hashes.
- Credentials use AES-256-GCM with a locally generated master key.
- Mutating requests and WebSocket upgrades require the configured or inferred same origin.
- Without a configured public URL, browser writes are accepted only through literal IP addresses or `localhost`, preventing an attacker-controlled rebinding hostname from authorizing first-time setup.
- Authentication cookies are HTTP-only, SameSite Strict and Secure when configured for HTTPS.
- Login failures are rate-limited by client address.
- Responses disable framing, MIME sniffing and unnecessary browser permissions.
