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
- Product state is stored in SQLite: names, targets, dimensions, the resolved persistence kind (tmux, screen or none) and timestamps. The tmux/screen session name is derived deterministically from the session ID and is never stored. Schema changes ship as numbered migrations applied at startup (currently version 3).

The Go process detaches from persistent multiplexers during graceful shutdown. A user-initiated session deletion explicitly terminates only the corresponding multiplexer session and removes its recording. tmux and screen run with wmux-owned namespaces and minimal configuration, so user configuration, status bars, key bindings and unrelated sessions are isolated.

## Session lifecycle

A product session is a logical record in SQLite. Each time it is started it gets a new execution *generation*: creation starts generation 1 and every explicit restart increments it. The terminal manager never writes session rows itself; it reports state changes through a callback and SQLite applies them with a conditional `UPDATE` that requires the caller's generation to match, so a late callback from a superseded execution can neither resurrect a deleted row nor overwrite the state of its successor.

Only the first start of a freshly created generation may create a multiplexer session. Every later connection attempt, including automatic reconnects and the restore performed after a wmux restart, attaches to the existing tmux/screen session and never re-runs the configured command. If that session no longer exists the record becomes `exited`; the user decides whether to restart it.

Deleting a session first tries to terminate its backend through a dedicated control connection. When the backend is already exited nothing is contacted. When the host is unreachable the local record and recording are still removed and the API returns a warning that the remote multiplexer session may still be running, so an offline host never makes a session or its host undeletable. Restarting a session stops the current execution, bumps the generation and starts the next one; attached browsers receive `disconnect` with `reason: restarted` and re-attach to the new execution instead of being told the session ended.

Delete, restart and reconnect requests for the same session ID are serialized in the HTTP layer. Terminal input is delivered through a per-backend single-writer gate: a stalled write times out for that request only and never closes the shared backend.

Only one wmux process may hold a data directory at a time. A cross-process lock prevents two managers from attaching the same multiplexer and appending conflicting transcript sequences.

## WebSocket protocol

The endpoint is `/ws/sessions/{id}?since={sequence}` and uses the authenticated HTTP-only cookie.

Client binary input starts with byte `0x00`; all remaining bytes are terminal input. Text input is UTF-8 encoded, while xterm binary events preserve each raw 8-bit byte. The browser splits large input into messages that remain below the server's 128 KiB read limit. Server binary output starts with byte `0x01`, followed by an unsigned 64-bit big-endian sequence number and the terminal bytes.

Control messages use JSON:

```json
{"type":"resize","cols":120,"rows":32}
{"type":"take_control"}
```

The server sends `hello`, zero or more replay frames, then `replay_end` before any queued live frame. It also sends `state`, `writer`, `disconnect` and `error` messages; `hello` and `state` carry the backend status and the execution `generation`, and `state` is pushed immediately when the runtime state changes as well as on a periodic heartbeat. The browser keeps input disabled until every replay write has drained through xterm's parser and the reported status is `running`; this prevents historical device-status queries from generating replies in the current PTY, keeps keystrokes from racing a backend that is still connecting, and establishes the boundary required by any future side-effecting terminal protocol.

The server pings each connection periodically and drops it when no pong arrives, and it re-validates the login token on every heartbeat: after logout, a password change or token expiry the connection receives `disconnect` with `reason: unauthorized` and close code 1008, which also releases its write lease.

The first attachment receives the write lease. Another attachment may explicitly take it; both clients receive the writer change immediately and input from read-only attachments is ignored. When `since` predates retained output, `hello.sequence` reports the oldest available sequence and `hello.truncated` is true.

Attachment closure is explicit: a real process exit first flushes every output frame that is still queued for that connection, then sends `state: exited` whose `sequence` is the final delivered sequence, and closes normally; `disconnect` with `reason: server_shutdown` closes with code 1012, while `reason: evicted` and `reason: restarted` close with code 1013. Browsers reconnect for the latter three and never suggest restarting (and therefore killing) a persistent backend merely because the transport was interrupted. A session whose backend connection is down can be retried immediately through `POST /api/sessions/{id}/reconnect`, which only wakes the reconnect loop and never restarts the execution.

## Terminal compatibility and side effects

The browser waits for its bundled terminal fonts before opening xterm, defaults to JetBrains Mono with italic faces plus a Nerd-symbol fallback, and activates xterm's Unicode 11 width tables. Seven further monospace families (Fira Code, Cascadia Code, Source Code Pro, Roboto Mono, IBM Plex Mono, Ubuntu Mono and the platform stack) are selectable per browser; each ships as its own chunk that is only fetched when chosen, and switching re-measures the open terminal. The terminal either fits the pane or keeps a fixed column count: in the fixed mode the font shrinks from the preferred size (down to a floor) until the columns fit, the screen is centred in the pane, and a phone that still cannot fit them scrolls sideways instead of clipping, so every device attached to a tmux session negotiates the same width. Application-cursor and modifier state is respected by the mobile special-key row. While the foreground program tracks the mouse (tmux with `mouse on`, vim, less), a vertical touch drag is converted into wheel reports, one per five rows to match tmux's default copy-mode step, with a short fling after release; without mouse tracking xterm's own touch scrolling applies. The application UI stays light, while the terminal theme is left unset so ANSI colors continue to come from xterm and programs running inside it.

The isolated local and SSH tmux servers enable mouse handling, set `escape-time` to 10 ms so that Esc followed by another key reaches editors as two keys rather than a Meta sequence, forward focus events, advertise 24-bit colour to the programs inside (`terminal-overrides` `Tc` plus `COLORTERM=truecolor`), and advertise native OSC 8 hyperlink support when the installed tmux version accepts that feature. Arbitrary tmux passthrough remains disabled. Every session receives its own `WMUX_SESSION_ID` through tmux's `update-environment`, which works on every tmux release and does not leak the first session's value into later ones. All shell snippets executed on an SSH host run under `/bin/sh -c`, so a fish, csh or other non-POSIX login shell is supported. OSC 52 clipboard writes, remote file transfer, desktop notifications, terminal graphics and Kitty keyboard extensions are intentionally unsupported until wmux has an explicit permission, active-tab, multi-client and resource-limit policy for each side effect.

## SSH trust

Creating a host does not silently trust it. wmux first probes the public host key and shows its SHA-256 fingerprint. Trusting it stores that fingerprint. Every authenticated connection compares the presented key to the stored value; a changed key is rejected until the user probes and explicitly trusts the new fingerprint.

## OpenSSH config discovery

The authenticated discovery endpoint reads the config of the operating-system account running wmux, or the explicit path selected with `WMUX_SSH_CONFIG`. Account-home lookup, `%d`, `~` and relative `Include` paths use the account database rather than the process `HOME` environment. It expands active `Include` files in lexical order and applies case-sensitive literal `Host` aliases, wildcard/negated patterns and OpenSSH's first-value rules. Discovery is read-only: it does not mutate SQLite, read `IdentityFile` contents, contact a host or trust a fingerprint. `IdentityFile` is exposed only as a boolean so local key paths never enter the browser response. `HostName` expands only `%h`; `User` accepts neither `%` tokens nor `${ENV}` expansion, so a config value can never pull server-side data into a candidate. The system-wide `/etc/ssh/ssh_config` is deliberately not read: discovery covers exactly the account config or the configured file and its `Include` fragments.

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
- Three wrong passwords in a row lock `/api/login` for everyone for an hour. The lock is global because there is a single account, it lives in memory (a restart clears it), and it never touches sessions that are already signed in, which are validated by token.
- Responses disable framing, MIME sniffing and unnecessary browser permissions.
- Every request produces one `request` log line with method, path, status, bytes and duration; 4xx logs at Info and 5xx at Warn, and `/api/health` is silent unless it fails.
- The API error codes the browser matches on are enumerated in `internal/api/response.go`.
