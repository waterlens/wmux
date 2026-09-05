# wmux 独立代码审查（2026-09-04）

审查对象：`main` 分支 `34de777`（fix(ui): compact dialogs and harden modal interactions）。

## 范围与方法

- 通读了全部 Go 源码（`cmd/`、`internal/` 共 14.5k 行，含测试）、全部前端源码（`client/src`）、CI、Dockerfile、compose、systemd/Caddy 示例与 README/architecture 文档。
- 本地运行：`go vet ./...`、`go test ./...`、`pnpm lint`、`pnpm typecheck`、`pnpm test:unit`，全部通过（Go 12 个包，vitest 11 个文件 50 个用例）。
- 重新执行 `vite build` 到临时目录并与提交的 `internal/webui/dist` 逐字节比对：完全一致，内嵌 UI 与源码同步。
- 对三个可疑点做了实验验证（tmux 环境传递、fish 登录 shell、不可达主机删除），结果见下文。
- 按要求，撰写本文前未阅读 `docs/reviews/2026-09-04-lifecycle-design-review.md`。

## 总体评价

代码质量高于同类个人项目的常见水平：安全模型完整且自洽（Origin + Sec-Fetch-Site + SameSite=Strict、DNS rebinding 防护、scrypt、token 只存哈希、AES-GCM 加 magic 前缀、主密钥原子创建、数据目录权限收紧、SSH 指纹 TOFU 且变更拒绝、sshconfig 只读且 fail-closed）。终端生命周期的关键边界（replay/live 分界、ReplayBarrier、单写多读 lease、terminate 使用独立控制连接、saveMu/resizeMu 序列化、transcript 尾部崩溃恢复）都有对应测试。

主要风险不在安全，而在三处"跑出实验室后才会碰到"的场景：SSH 远端脚本对登录 shell 的假设、本机 tmux 的环境变量传递、以及主机永久不可达时的生命周期死角。

## 发现

### 高

**H1. SSH 远端 attach/terminate 脚本假设登录 shell 是 POSIX sh，fish/csh 主机无法使用**

`internal/terminal/ssh.go:107` 用 `sess.Start(command)` 执行 `remoteAttachCommand` 生成的脚本（`ssh.go:365-437`），sshd 会通过远端用户的登录 shell `-c` 运行它。脚本使用了 `if ! …; then …; fi`、`"${SHELL:-/bin/sh}"`、`var="…"` 等 POSIX 语法；`remoteTerminateCommand`（`ssh.go:503-518`）和 `remoteScreenSetup`（`ssh.go:453-475`）同样如此。

实验（本机 fish 4.x）：

```
fish -c 'if ! true; then echo created; fi'                      # exit 127: Missing end to balance this if statement
fish -c 'exec "${SHELL:-/bin/sh}" -l'                            # exit 127: ${ is not a valid variable in fish
fish -c 'wmux_screen_root="${XDG_CACHE_HOME:-$HOME/.cache}/x"'   # exit 127: Unsupported use of '='
```

后果：登录 shell 为 fish/tcsh 的主机，三种持久化模式的会话都会立即以 `exited` 结束（`sess.Wait` 返回 ExitError，`Reconnectable` 判定为不可重连），且 terminate 也无法杀掉远端会话。`resolveRemote` 的探测 `command -v tmux >/dev/null 2>&1` 恰好在 fish 下能跑，所以错误只在 attach 阶段暴露。

建议：把整段脚本包进 `exec /bin/sh -c '<script>'`（用现有 `shellQuote`），三处（attach、terminate、probe）统一；补一个 `TestRemoteCommandsAreShellAgnostic` 类型的单测，用 `fish -c`/`sh -c` 各跑一遍生成的脚本（CI 可 `apt-get install fish`）。

**H2. 本机 tmux 会话拿不到自己的环境变量（`WMUX_SESSION_ID` 等只对第一个会话生效）**

`internal/terminal/local.go:104-124` 只在 `new-session` 的 `cmd.Env` 里传入 `localEnvironment(spec.Env)`（`local.go:116`），attach 时不传（`local.go:52`）。tmux 的会话环境来自 server 的全局环境，而全局环境只在 server 首次启动时从客户端复制；后续 `new-session` 不会把客户端环境带进去。

实验（tmux 3.7c，与 wmux 相同的 `-L`/`-f /dev/null` 参数）：

```
WMUX_SESSION_ID=first  tmux -L t new-session -d -s s1 'echo $WMUX_SESSION_ID > env-s1'
WMUX_SESSION_ID=second tmux -L t new-session -d -s s2 'echo $WMUX_SESSION_ID > env-s2'
s1 sees: first
s2 sees: first
```

后果：`internal/app/runtime_repository.go:180` 注入的 `WMUX_SESSION_ID` 在第二个及之后的本机 tmux 会话里是错的；将来任何通过 `spec.Env` 传递的变量都会有同样问题。远端路径不受影响，因为 `remoteAttachCommand` 在会话命令里显式 `export`。

建议：与远端做法对齐，把 `export KEY=VALUE; exec "$SHELL" -l` 作为 `new-session` 的命令；或使用 tmux ≥ 3.2 的 `new-session -e KEY=VALUE`（Docker 镜像的 bookworm tmux 3.3a 支持）。screen 路径（`ensureLocalScreen`，`local.go:161-200`）用 `cmd.Env` 启动守护进程，每个会话独立，不受影响。

**H3. 主机永久不可达时，SSH 会话和主机都无法删除**

`internal/api/sessions.go:216-233` 的 `stopTerminalSession` 先 `DiscardContext`，仅当运行时状态是 `exited`/`error` 时才允许丢弃（`internal/terminal/manager.go:1009`）；否则走 `Terminate`，而 `terminate` 会新建一条 SSH 控制连接去 kill 远端 tmux（`manager.go:943` → `ssh.go:477-501`）。连接被拒/超时不是 permanent error（`ssh.go:182-184`），会话停在 `disconnected`，无客户端时 `waitForRetry` 永久等待（`manager.go:701-716`）。

用现有 API fixture 验证（临时测试，已删除）：把一条 `running` 的 tmux SSH 会话指向 `127.0.0.1:1` 后 `Restore`：

```
runtime state after restore against unreachable host: disconnected
DELETE /api/sessions/{id} -> 502   (terminal_stop_failed)
DELETE /api/hosts/{id}    -> 409   (host_in_use)
session row still present after delete attempt
```

现有测试 `TestDeleteDormantPersistentSSHSessionsWithoutContactingUnreachableHost` 只覆盖 `exited`/`error` 两种休眠状态，没有覆盖 `reconnecting`。UI 侧该会话会一直显示"正在重连"转圈，`state` 消息里的 `Message` 只在 `error` 状态才被前端展示（`TerminalView.tsx:351`），用户既看不到原因也删不掉。

建议：提供显式的"丢弃（不联系主机）"路径，例如 `DELETE ?force=1` 或在 `DiscardContext` 允许 `disconnected` 状态；UI 在 terminate 失败时提示"主机不可达，是否仅删除本地记录（远端 tmux 会话会残留）"。

### 中

**M1. 输入写入超时会直接关闭后端连接；对 `none` 持久化等于杀掉 shell**

`internal/api/websocket.go:217-219` 给每次 `WriteContext` 10 秒超时；`localBackend.WriteContext`（`local.go:395-406`）超时后 `b.Close()` 会 `Process.Kill()` 并关闭 PTY，`sshBackend.WriteContext`（`ssh.go:537-548`）则关闭整个 `ssh.Client`。前台程序不读 stdin 时 PTY 输入队列（几 KB）很快填满，粘贴较长文本就会触发：tmux/screen 会话经历一次重连并丢掉剩余输入；`none` 会话的 shell 被 SIGKILL，会话结束。`TestWriteContextCancelsBlockedBackendWrite` 证明这是刻意设计，但代价落在最常见的"粘贴时程序没在读"场景上。

建议：对后端写入施加背压（写入阻塞时暂停读取 WebSocket，让浏览器侧排队）而不是关闭后端；至少对 `PersistenceNone` 不以 Close 作为取消手段，或把超时提高并向前端回报"输入被阻塞"。

**M2. WebSocket 没有心跳和读超时，幽灵连接会长期持有输入权**

`websocket.go` 未调用 `Ping`，`connection.Read` 无 deadline；`http.Server` 的 `IdleTimeout` 对 hijack 后的连接无效。移动端切后台或网络掉线时 TCP 不会立刻断开，服务端每 2 秒的 `state` 小消息只会进入内核发送缓冲，写超时不会触发；直到 TCP keepalive/重传放弃（分钟级），旧连接才被清理。期间新打开的页面拿不到 writer，需要手动"接管"。`Attachment` 也不会因此释放。

建议：服务端每 15–30 秒 `connection.Ping(ctx)`（coder/websocket 内置处理 pong），失败即关闭并 detach；或给 `Read` 加滚动 deadline。

**M3. xterm 使用 DOM 渲染器，大量输出与移动端性能差**

`TerminalView.tsx:138-164` 只加载了 fit/unicode11/web-links；`package.json` 没有 `@xterm/addon-webgl` 或 `addon-canvas`。xterm 5+ 默认 DOM 渲染，加上 `allowTransparency: true` 与 `minimumContrastRatio: 4.5`，`cat` 大文件、`yes`、`htop` 高刷新时明显掉帧；配合服务端 `ClientBuffer: 512` 的驱逐策略，慢客户端会反复被踢（1013）并重连回放。

建议：加载 `@xterm/addon-webgl`，`onContextLoss` 时回退到 DOM；评估是否真的需要 `allowTransparency`。

**M4. 隔离 tmux 服务器不提供 truecolor**

`configureLocalTmux`（`local.go:126-154`）与远端脚本都未设置 RGB 特性，`localEnvironment`（`local.go:360-375`）只设 `TERM=xterm-256color` 不设 `COLORTERM`。实验（tmux 3.7c，通过 pty 附着）：

```
无 COLORTERM:            features=bpaste,ccolour,clipboard,cstyle,focus,title
COLORTERM=truecolor:     features=bpaste,ccolour,clipboard,cstyle,focus,RGB,title
```

xterm.js 本身支持 24-bit 色，但 tmux 内的 neovim/bat/delta 等会降级到 256 色。建议：`set-option -as terminal-features ",xterm-256color:RGB"`（本地与远端都加），或在附着客户端环境里设 `COLORTERM=truecolor`。

**M5. `PATCH /api/sessions/{id}` 的 cols/rows 与运行时不同步且会被覆盖**

`internal/api/sessions.go:124-140` 直接写库，不经过 `Manager`，既不 resize 后端，也不更新 `runtimeSession.cols/rows`；下一次 `save()`（`manager.go:807-825`，`record()` 取运行时尺寸）经 `SaveRuntimeSession` 的 `ON CONFLICT … cols = excluded.cols`（`store/sessions.go:300-304`）会把它覆盖回去。前端只用该接口改名（`api.ts:176`）。建议删掉 cols/rows 字段，或改为通过 `Manager` 应用后再落库。

**M6. 终端历史明文落盘，与凭据加密的威胁模型不一致**

`internal/transcript/store.go:232-289` 以 base64 JSONL 写入所有输出，最多每会话 16 MiB（`cmd/wmux/main.go:88-92`）。目录 0700 能挡住其他用户，但备份、快照、云盘同步会把 `cat ~/.ssh/id_rsa`、`env`、`aws configure` 之类的输出带走，而同一目录里的 SSH 密码/私钥却是 AES-GCM 加密的。README 只说"有上限的终端历史文件"。建议至少在文档里明确这一点；更好的做法是用主密钥对 segment 加密，或提供"不录制/仅内存回放"的会话选项。

**M7. attach 时在会话互斥锁内从磁盘回放**

`internal/terminal/attachment.go:31-56` 持有 `s.mu` 期间调用 `s.log.Replay`（最多 8192 帧/16 MiB，逐行 JSON+base64 解码），此时 `publish`（`manager.go:652-699`）被阻塞，PTY 读循环停顿，前台程序会在 PTY 缓冲填满后被卡住。个人场景通常是百毫秒级，但新设备打开长会话时其它设备会看到明显卡顿。建议：在锁外读快照、锁内只做"记录 newest 并注册订阅者"，用 sequence 去重；或引入内存环形缓冲。

### 低

- **L1. 登录用户名计时侧信道**：`internal/api/auth.go:80-84` 用户名不存在时直接返回，不做 scrypt，与存在时相差约 100 ms。单用户系统影响很小，可用固定的假哈希校验拉平。
- **L2. `Manager.Refresh` 无调用方**（`manager.go:225-232`），只有 `RefreshHost` 被 `hosts.go` 使用。同时前端 `error` 状态卡片提供"立即重连"（`TerminalView.tsx:653-661`）只是重连 WebSocket，对永久启动错误无效，应改为"重启会话"或同时提供两者。
- **L3. `hello.truncated` 提示措辞**：`ReplayLimit` 截断时（`attachment.go:44-48`）也提示"较早的终端输出已被清理"（`TerminalView.tsx:343-345`），但历史并未被删除，只是回放上限；长会话每次冷打开都会弹一次。
- **L4. `recoverRequests` 吞掉 `http.ErrAbortHandler`**（`middleware.go:28-38`），并在 hijack 后仍尝试写响应；建议 `if value == http.ErrAbortHandler { panic(value) }`。
- **L5. 未认证的 `/api/health` 与 `/api/status` 暴露 version/commit**（`server.go:110-116`、`auth.go:19-32`）。health 可只返回 `ok`。
- **L6. CSP `connect-src 'self' ws: wss:`**（`middleware.go:23`）允许任意 ws 目标；现代浏览器 `'self'` 已覆盖同源 ws/wss。
- **L7. 未匹配的 `/api/*` 路径回退到 SPA `index.html` 并返回 200 HTML**（`server.go:102`、`webui/handler.go:39-41`）；API 客户端拿到 HTML 而不是 404 JSON。
- **L8. 非 flock 平台的锁文件崩溃后残留**（`config/lock_fallback.go`），不是当前支持目标，文档提一句即可。
- **L9. `reconnecting` 状态没有任何错误信息**：`publicTerminalMessage` 的文案只在前端 `error` 状态显示（`TerminalView.tsx:351`），主机掉线时用户只看到无限转圈（与 H3 相关）。
- **L10. `minimumContrastRatio: 4.5`** 会改写程序配色，与 README"终端画布保留程序自己的 ANSI 配色"的表述不完全一致。
- **L11. `issueLogin(w, ctx)`** 参数顺序不符合 Go 惯例（context 应在前），`golangci-lint` 的 `revive` 会报。

## 做得好的地方（值得保持）

- `originAllowed` 在无 PublicURL 时只接受字面 IP/localhost，配合 `Sec-Fetch-Site` 与 SameSite=Strict，`/api/setup` 的 DNS rebinding 路径被堵死；测试 `TestOriginAllowedRejectsDNSRebindingFallback` 明确固化了这一点。
- 主密钥用临时文件 + `os.Link` 发布，避免并发首启覆盖；数据目录、DB、lock 文件都主动 chmod；symlink 一律拒绝。
- `sshconfig` 包：Include 深度/环路检查、`Match` 除 `all` 外 fail-closed、`%` token 白名单、`User` 禁止 `${}` 展开、错误信息不进 API/日志；`ProxyJump/ProxyCommand` 不导入。
- `terminate` 先用独立控制连接做破坏性操作，成功后才取消 run loop（`manager.go:936-973`），失败时保持数据连接完好；`saveMu` 保证旧的 active 快照不会复活已终止会话（`TestTerminationCannotBeOverwrittenByOlderActiveSave`）。
- 前端 `ReplayBarrier` 把"历史里的 DSR 查询在当前 PTY 触发回复"这类隐蔽问题从设计上排除，并有单测。
- transcript 的尾部恢复、原子提交 sequence、trim 失败不影响已提交序列，都有测试。
- `.gitattributes` 把 dist 标为 generated，且 dist 与源码确实一致；CI 覆盖 race、tmux/screen 集成与浏览器冒烟。

## 建议的处理顺序

1. H1（远端脚本包 `sh -c`）：改动小，影响所有非 POSIX 登录 shell 的主机。
2. H3（不可达主机的丢弃路径）+ L9：否则用户会积累无法清理的记录。
3. H2（本机 tmux 环境）：改为会话命令内 export 或 `-e`。
4. M2（心跳）与 M1（输入背压）：都属于"跑久了才会碰到"的稳定性问题。
5. M3/M4：体验类，改动都很小。
6. 其余低优先级项可与文档一起收尾。

## 综合意见（结合首轮 R01–R19 与生命周期追审 D01–D06）

本节在阅读既有两份 review 之后补写，用于把三方发现合并成一个判断和一条改进路线。

**总判断**

1. 安全边界与工程质量是这个项目的强项，不需要返工：Origin/SameSite/rebinding 防护、凭据加密、指纹 TOFU、sshconfig 只读解析、transcript 崩溃恢复、CI 覆盖 race 与集成，都经得起检查。
2. 真正的短板集中在两类，彼此独立：
   - 运行时生命周期把"连接观察"当作"进程存在性"和"执行意图"，又缺少执行代号、后端身份和跨实例的操作边界。三份 review 的 D01–D06、R02–R04、R07 以及本文的 H3、M1 都是这一根因的不同切面：既会错误丢弃（曾运行的远端任务被 Discard），也会错误保留（不可达主机删不掉）；既会复活已删除记录，也会让一次输入超时杀掉共享后端。
   - 运行环境兼容性：远端脚本依赖 POSIX 登录 shell、本机 tmux 环境变量只对首个会话生效、隔离 tmux 无 truecolor（本文 H1、H2、M4）。这些与生命周期无关，改动小，但影响的是"能不能用"。
3. 连接协议缺口（R01 登录撤销不断开、R03 退出丢尾部输出、D06 后端未就绪即开放输入、本文 M2 无心跳）介于两者之间：修复不依赖重构，但应与生命周期的终态协议一起定义，避免改两次。
4. 不应再逐分支打补丁。`saveMu`、`resizeMu`、`operationMu` 已经证明局部锁堵不住跨实例、跨进程、跨数据库的交错；追审提出的"逻辑会话 → 执行代 → 连接"三层职责划分是正确方向，应作为一个 milestone 一次做完。

**改进路线**

阶段 0：独立小改动，可立即合入，互不依赖。

1. 远端 attach/terminate/probe 脚本统一包进 `exec /bin/sh -c '…'`，补 fish 与 sh 双跑测试（H1）。
2. 本机 tmux 会话命令内显式 export，或使用 `new-session -e`（H2）。
3. 登出、改密、令牌过期时关闭对应 attachment 并回收 writer；输入路径核验令牌（R01）。
4. `changePassword` 的 401 不触发全局登出（R08）；退出失败保留确认框并显示错误（R09）。
5. 服务端 WebSocket 周期 Ping 与读超时，失败即 detach（M2）。
6. 本地与远端 tmux 增加 `terminal-features ",xterm-256color:RGB"` 或 `COLORTERM=truecolor`（M4）。

阶段 1：生命周期重构，一个 milestone 内完成，以追审的回归场景表加本文两个反例（不可达主机删除、tmux 环境）为验收。

1. 数据模型：session 增加 execution generation 与 revision；持久化 BackendRef（后端种类、原目标地址/账户、namespace、mux 名）；进程存在性表达为 unknown/present/absent。
2. Backend 接口拆为 Inspect、CreateExecution、AttachExisting、Terminate。Restore、自动重连、"重试后台"只允许 attach 原执行；原执行缺失时标记已结束或未知，由显式 Restart 开启下一代，不再隐式重跑命令（D01）。
3. 创建流程先登记执行计划再做外部副作用；Terminate 针对已登记的 BackendRef 清理，覆盖"已创建、尚未返回"的窗口；主机编辑只作用于后续执行（D02）。
4. 按逻辑 session ID 串行的操作入口，覆盖 restart/delete/terminate 全过程；运行观察改为带 generation/revision 条件的 UPDATE，去掉 UPSERT 复活路径；trustHost 只更新指纹并校验 revision（D03、D04）。
5. 输入所有权归 execution：输入超时只取消该次写入，采用有界队列或背压，不再 Close 共享后端；明确"可能部分写入"语义（D05、M1）。
6. 丢弃路径按进程存在性决定：确认不存在或从未创建可直接清除；无法连接时允许"仅删除本地记录并标记远端可能残留"，界面给出原因（H3、R02、L9）。

阶段 2：协议与前端，在阶段 1 的 generation 落地后进行。

1. 终态协议携带 generation 与 finalSequence，正常退出先排空输出再发 exited（R03）；其他设备收到换代事件后重新 attach，而不是提示重启（R04）。
2. 输入开放条件加入 backend ready；"立即重连"改为"重试后台连接"，与 Restart 区分（D06、R05、L2）。
3. 重启活跃会话需确认与防重入（R11）；列表轮询结果版本化，旧结果不得关闭新建标签（R07）。
4. 已信任主机可重新探测并人工确认轮换后的密钥（R06）。
5. 引入 `@xterm/addon-webgl` 并保留 DOM 回退（M3）。

阶段 3：体验、边角与文档。

1. 移动端与无障碍：短屏初始化可滚动、触摸滚动历史、横屏特殊键、抽屉焦点、读屏模式、弹窗内联错误、底部菜单翻转（R12–R19、C01）。
2. 删除 `PATCH /api/sessions` 的 cols/rows 或改经 Manager 应用（M5）；attach 回放移出会话锁（M7）。
3. 终端历史加密或提供不录制选项，至少在 README 明确其明文特性（M6）。
4. 其余低优先级项：登录计时侧信道、`recoverRequests` 对 `ErrAbortHandler` 的处理、health 信息、CSP、未知 `/api/*` 回退、截断提示措辞。

## 处理记录（2026-09-05）

按上文"综合意见"的阶段 0 与阶段 1 一次完成，阶段 2 的协议与前端项也一并落地；改动以"删掉补丁式机制、只保留一个持久化边界"为原则，而不是再加抽象层。

**运行时生命周期（`internal/terminal`、`internal/store`、`internal/app`、`internal/api`）**

- 运行时不再写数据库：删除 `Repository.SaveSession`、`save()`/`record()`/`saveMu` 与 `SaveRuntimeSession` 的 UPSERT；所有状态只经 `OnSessionState` 以条件 `UPDATE` 落库（D03）。
- 会话行新增 `generation`（迁移 2）。创建为第 1 代，显式重启经 `BeginSessionRestart` 加一；`UpdateSessionRuntime` 带 `generation = ?` 条件，过期执行的回调被静默忽略（D04）。
- 首次启动才允许 create-or-attach；此后的自动重连与服务重启后的 `Restore` 只 attach，后端不存在返回 `ErrBackendMissing` 并置为 exited，不再重跑命令（D01）。
- `Terminate` 对已 exited/terminated 的运行时不再联系主机；kill 失败保留运行时并返回错误；`Discard` 在任何状态下都可丢弃运行时。`DELETE /api/sessions/{id}` 在主机不可达时仍删除本地记录并返回 `200 {"warning"}`（H3、R02）。
- 输入改为每后端容量 1 的信号量：写超时只让该次请求失败，不再 `Close` 共享后端（D05、M1）。
- `resizeMu`/`saveMu` 改为 `applySize()` 单一序列化点；API 层按会话 ID 用小型 keyed mutex 串行 delete/restart/reconnect；`trustHost` 只写指纹（D04）；`PATCH` 不再接受 cols/rows（M5）。
- `Manager.Reconnect` 接入 `POST /api/sessions/{id}/reconnect`，退避等待与永久错误等待都能被立即唤醒（L2）。

**运行环境兼容性**

- 远端所有脚本统一 `exec /bin/sh -c '…'`，fish/csh 登录 shell 可用；新增 `sh -c` 与 `fish -c` 双跑测试（H1）。
- 本机与远端 tmux 在同一次调用里 `set-option -g update-environment … \; new-session …`，每个会话拿到自己的 `WMUX_SESSION_ID`（H2）。
- 本机与远端 tmux 追加 `terminal-overrides ",xterm*:Tc"`，会话环境加 `COLORTERM=truecolor`（M4）。

**连接协议与前端**

- 每个心跳周期重新校验登录令牌，登出/改密/过期后以 `disconnect(reason=unauthorized)` + 1008 关闭并回收写权（R01）。
- 退出前先排空该连接已缓冲的输出，`state: exited` 携带最终序号（R03）；服务端 30s Ping、10s 超时（M2）。
- `hello`/`state` 携带 `generation`，状态变化经 `Attachment.States` 即时推送；客户端仅在状态为 running 时开放输入（D06）。
- 重启时其他设备收到 `disconnect(reason=restarted)` + 1013 并自动重连（R04）；重启活跃会话需确认且防重入（R11）；列表轮询结果版本化（R07）。
- `changePassword` 的 401 不再触发全局登出（R08）；退出失败保留确认框并显示错误（R09）；"立即重连"改为"重试后台连接"。
- 引入 `@xterm/addon-webgl`，失败或上下文丢失时回退 DOM 渲染（M3）。

**未处理**：移动端与无障碍项（R12–R19、C01）、回放移出会话锁（M7）、历史加密（M6，README 已明确其明文特性）、已信任主机的密钥轮换重验（R06）及其余低优先级项。
