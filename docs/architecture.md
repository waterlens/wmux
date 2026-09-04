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

Client binary input starts with byte `0x00`; all remaining bytes are terminal input. Text input is UTF-8 encoded, while xterm binary events preserve each raw 8-bit byte. The browser splits large input into messages that remain below the server's 128 KiB read limit. Server binary output starts with byte `0x01`, followed by an unsigned 64-bit big-endian sequence number and the terminal bytes.

Control messages use JSON:

```json
{"type":"resize","cols":120,"rows":32}
{"type":"take_control"}
```

The server sends `hello`, zero or more replay frames, then `replay_end` before any queued live frame. It also sends `state`, `writer`, `disconnect` and `error` messages. The browser keeps input disabled until every replay write has drained through xterm's parser; this prevents historical device-status queries from generating replies in the current PTY and establishes the boundary required by any future side-effecting terminal protocol.

The first attachment receives the write lease. Another attachment may explicitly take it; both clients receive the writer change immediately and input from read-only attachments is ignored. When `since` predates retained output, `hello.sequence` reports the oldest available sequence and `hello.truncated` is true.

Attachment closure is explicit: a real process exit sends `state: exited` and closes normally; `disconnect` with `reason: server_shutdown` closes with code 1012, while `reason: evicted` closes with code 1013. Browsers reconnect for the latter two and never suggest restarting (and therefore killing) a persistent backend merely because the transport was interrupted.

## Terminal compatibility and side effects

The browser waits for its bundled terminal fonts before opening xterm, uses JetBrains Mono with italic faces plus a Nerd-symbol fallback, and activates xterm's Unicode 11 width tables. Application-cursor and modifier state is respected by the mobile special-key row. The application UI stays light, while the terminal theme is left unset so ANSI colors continue to come from xterm and programs running inside it.

The isolated local and SSH tmux servers enable mouse handling and advertise native OSC 8 hyperlink support when the installed tmux version accepts that feature. Arbitrary tmux passthrough remains disabled. OSC 52 clipboard writes, remote file transfer, desktop notifications, terminal graphics and Kitty keyboard extensions are intentionally unsupported until wmux has an explicit permission, active-tab, multi-client and resource-limit policy for each side effect.

## SSH trust

Creating a host does not silently trust it. wmux first probes the public host key and shows its SHA-256 fingerprint. Trusting it stores that fingerprint. Every authenticated connection compares the presented key to the stored value; a changed key is rejected until the user probes and explicitly trusts the new fingerprint.

## OpenSSH config discovery

The authenticated discovery endpoint reads the config of the operating-system account running wmux, or the explicit path selected with `WMUX_SSH_CONFIG`. Account-home lookup, `%d`, `~` and relative `Include` paths use the account database rather than the process `HOME` environment. It expands active `Include` files in lexical order and applies case-sensitive literal `Host` aliases, wildcard/negated patterns and OpenSSH's first-value rules. Discovery is read-only: it does not mutate SQLite, read `IdentityFile` contents, contact a host or trust a fingerprint. `IdentityFile` is exposed only as a boolean so local key paths never enter the browser response.

Includes remain lazy syntax-tree nodes: resolution opens them only when the current `Host` or safely supported `Match all` block is active. Other `Match` conditions are fail-closed and `Match exec` is never executed. Candidate enumeration follows global, universal `Host *` and `Match all` includes; an alias declared only inside any other conditional include is intentionally not enumerated.

Import accepts only an alias, reloads the config at the mutation boundary and stores the resolved alias, address, port and username with SSH-agent authentication. The duplicate check and insert are serialized because SQLite intentionally has no unique connection-tuple constraint. `ProxyJump` and `ProxyCommand` candidates remain visible but disabled until the Go SSH transport can reproduce those connection semantics. Fingerprint probing and trust continue through the normal explicit host workflow after import.

Container deployments should mount only the config text and required `Include` fragments read-only. Private keys, `known_hosts` and the rest of `~/.ssh` are outside this discovery boundary.

## HTTP security

- Passwords use a versioned scrypt hash with a random salt.
- Login tokens are random values; SQLite stores only their SHA-256 hashes.
- Credentials use AES-256-GCM with a locally generated master key.
- Mutating requests and WebSocket upgrades require the configured or inferred same origin.
- Without a configured public URL, browser writes are accepted only through literal IP addresses or `localhost`, preventing an attacker-controlled rebinding hostname from authorizing first-time setup.
- Authentication cookies are HTTP-only, SameSite Strict and Secure when configured for HTTPS.
- Login failures are rate-limited by client address.
- Responses disable framing, MIME sniffing and unnecessary browser permissions.
