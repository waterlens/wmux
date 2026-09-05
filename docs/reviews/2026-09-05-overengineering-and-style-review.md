# wmux 过度工程与代码风格审查（2026-09-05）

审查对象：`main` 分支 `4fb3e9f`（refactor: apply lifecycle review findings and fix restart reconnect），工作区干净。

## 范围与方法

- 本次审查只关注两类问题：**过度工程**（只有一个实现的抽象、无人使用的能力、为不可能发生的情形写的防御、重复实现、过重的测试脚手架）与**不当代码风格**（不合语言惯例、命名与分层不一致、超长函数、重复代码、过时注释）。正确性、安全与性能已由 `2026-09-04-independent-code-review.md` 覆盖，本文不重复其内容。
- 由 5 个 Claude Opus 5 子代理并行完成，按模块切分：① `internal/terminal`；② `internal/api`、`cmd/wmux`、`internal/app`、`internal/webui`；③ `internal/store`、`transcript`、`security`、`config`、`sshx`、`sshconfig`、`version`；④ `client/src` 与前端构建配置；⑤ 脚本、部署、CI、文档，以及跨包重复扫描。每个子代理通读所辖范围的**全部**文件（含测试，合计约 2.3 万行），每条"无调用方 / 死代码"结论都以全仓库 grep 核实。
- 汇总时对五份报告去重合并，并抽样复核了 20 余条高严重度断言（死导出、死列、死配置、SQL 参数顺序、跨包重复、前后端协议漂移等），全部成立。基线：`gofmt -l`、`go vet ./...`、`pnpm lint`、`pnpm typecheck` 均通过。
- 五份原始报告（含每条发现的简化后代码形态与更细的行号）保存在 `2026-09-05-overengineering-style/` 目录，本文以 **T-**（terminal）、**A-**（api）、**S-**（storage）、**C-**（client）、**X-**（tooling / cross-package）前缀引用其编号。

## 总体评价

代码整体是"认真写出来的"：并发边界、安全模型、测试覆盖都远超同类个人项目，五份报告都在结尾单独列出了"看起来复杂但其实必要"的部分（见文末）。问题不在架构，而在四种可归类的堆积：

1. **死契约贯穿多层。** 一个字段一旦进入 `store` 模型就自动流到 SQL 列、JSON 响应、zod schema 和 TS 类型，但没有人读它。`backend_name`、`exit_code`、前端的 `generation`、`socketEvent.ClientID/WriterID/Clients` 都是这样。
2. **为测试扭曲生产代码。** 为了让某个测试不必构造真实依赖，生产路径多出了永不触发的降级分支（`terminals == nil` 返回 503）、只有一个实现的接口加可变参数（`sessionSpecProvider`）、只有测试才传的注入参数（`terminalFonts`、`Credential` 指针变体）。
3. **同一份数据或规则在多处手写。** 前端类型与 zod schema、`SessionRecord/SessionSpec/store.Session` 三份映射、local/ssh 各一份 tmux/screen 配置、三层重复校验、两份 SSH 认证实现、两个 `newID`。
4. **为不可能发生的情形写防御。** nil 接收者检查、`sync.Once` 保护已幂等的操作、四层同样的 terminate 守卫、为 Windows 准备的第二套文件锁、每 64 条指令检查一次 ctx。

去重后：**高 12 条、中 47 条、低 22 组**（低组把同类小问题合并计数）。单文件最大的问题点是 `internal/terminal/manager.go`（1092 行）、`client/src/components/TerminalView.tsx`（765 行）和 `scripts/browser-smoke.mjs`（807 行）。

## 发现

### 高

**H1. `manager.go` 1092 行，三份手写 teardown（T-A1）**
`internal/terminal/manager.go:945-973`（`finishStop`）、`:976-1002`（`discard`）、`:1004-1035`（`shutdown`）做同一件事：置 closed、取 backend、cancel、关客户端、关 backend、等 run loop、关 transcript，只在关闭顺序、close reason 和错误处理上有差别。三份副本导致了不一致的可观察行为：只有 `finishStop` 会置 `StateTerminated` 并推最后一次 `OnSessionState`，这只能靠比对三段代码发现。建议抽一个 `teardown(ctx, reason, finalState)`，并把 `runtimeSession`（run loop、fanout）从 `manager.go` 拆到 `session.go` / `fanout.go`，`attachment.go` 里的 `attach` / `detach` 方法随之归位。

**H2. 一批只有测试用或无人用的导出与字段（T-A2 / A3 / A10，X-12）**
- `IsPermanentStartError`（`backend.go:145`）零调用方；`sshAuth`（`ssh.go:231`）仅测试用；`Attachment.Write`（`attachment.go:100`）与 `backend` 接口嵌入的 `io.Writer`（`backend.go:19`）的三个实现零调用。
- `Callbacks.OnWriterChanged`（`types.go:146`）在仓库内的三个实现全是空函数，却有 5 处触发点各自在锁外多做一次分发。
- `localBackend.toolPath / name`、`sshBackend.name`、`SessionSpec.Name`、`SessionRecord.Name`、`OutputFrame.Time` 只写不读；`SessionStatus.LastSequence` 生产无读者（`websocket.go` 用自己维护的 `delivered`）；`NopCallbacks` 不必导出。

建议：全部删除；`Callbacks` 缩为 `OnSessionState` 与 `OnClientDropped` 两个方法。

**H3. `store.Session` 直接充当 HTTP 响应体，`backend_name` 与 `exit_code` 是贯穿五层的死列（A-S1，S-1 / S-4 / S-16，C-10）**
`internal/api/sessions.go:271-281` 的 `publicSession` 靠"改字段值再序列化"脱敏（`session.BackendName = ""`），而 host 有专门的 `hostResponse` DTO（`hosts.go:276-289`）。后果：
- `backend_name`（`migrations.go:53`、`models.go:72`、`runtime_repository.go:115`、`api.ts:57`、`types.ts:42`）唯一的写入是 `terminal.BackendName(status.ID)`，一个 session ID 的纯函数；读出后立刻抹掉，前端永远拿到空串。
- `exit_code`（`migrations.go:60`、`models.go:80`、`api.ts:65`、`types.ts:50`）全仓库没有任何写入（`grep "ExitCode:"` 为空），恒为 NULL。
- 前端 `Session` 的 `backendName / exitCode / error / cols / rows` 在任何 `.tsx` 里都没有读取点。

建议：补 `sessionResponse` DTO（或反向删 `hostResponse`，但不要两种并存），删除两列（迁移 3）、模型字段及前端 schema；`store/models.go` 的 json tag 只保留 `Credentials`（它是 `EncryptJSON` 的真实契约）。

**H4. `UpdateSessionRuntime` 的 13 占位符 SQL 不可读，`backend_name` 的守卫挂在另一个变量上（S-2）**
`internal/store/sessions.go:216-249`：`backend_name = CASE WHEN ? = '' THEN backend_name ELSE ? END` 的守卫实参是 `backend`（第 241 行）而不是 `backendName`。行为目前正确，因为唯一调用方 `runtime_repository.go:113-116` 保证两者同时为空或同时非空，但任何读者都会先当成笔误，要翻到另一个包才能确认。`AND (status IS NOT ? OR ...)` 那 6 个占位符只为"状态未变时不写行"，而该语句不更新 `updated_at`，冗余写没有任何可观察后果。配合 H3 删列后可缩成 5 参数：`backend = COALESCE(NULLIF(?, ''), backend)`，现有 generation 隔离与幂等测试仍通过。

**H5. `internal/api` 为测试保留的降级模式与三套依赖注入（A-O1 / O2 / O3，X-17）**
- `New` 的 `providers ...sessionSpecProvider`（`server.go:40-59`）：接口只有 `*app.RuntimeRepository` 一个实现，6 个调用点只有 `main.go:112` 传值，传的正是 `New` 自己兜底构造的同一类型；5 个测试调用点都不传。生产路径每次 `New` 都构造一个 `RuntimeRepository` 然后丢弃。
- `terminals` / `transcripts` 可为 nil（`sessions.go:28,147,161,174,217,239`、`hosts.go:137,204`、`websocket.go:57`）只因 3 个测试传 `nil, nil`；9 处判空风格不统一（一半返回 503 `terminal_unavailable`，一半静默跳过），且这个错误码一路传到 `client/src/api.ts:202` 维护了用户文案。
- `probeSSH` 的 nil 兜底（`hosts.go:162-165,186-189`）在 `server.go:71` 无条件赋值后不可达。

建议：接口与可变参数删掉改为必填参数；`terminals / transcripts` 定为不变量，删 9 处判空与 `terminal_unavailable`；测试统一用真实 `terminal.NewManager`（`Create` 前不启动任何进程）。

**H6. `internal/security` 导出 6 个加解密函数，生产只用 2 个；AAD 从未传过非 nil（S-3）**
`security.go:138-182` 的 `Encrypt / Decrypt / EncryptWithAAD / DecryptWithAAD` 包外零引用，AAD 只在自己的测试里出现。一个 316 行的安全包，公开面是实际需求的 3 倍。建议收敛成未导出的 `seal / open`，只导出 `EncryptJSON / DecryptJSON`。

**H7. 前端类型与 zod schema 是同一契约的两份手写副本（C-1，X-14）**
`client/src/types.ts:1-52,65-79` 与 `client/src/api.ts:6-67` 六个类型逐字段重复；zod 默认 strip 未知字段，schema 落后于类型时不报错，只在运行时吞字段。建议只保留 schema，`z.infer` 推导类型，schema 搬到 `types.ts` 避免循环依赖。约 45 行重复消失。

**H8. `Workspace` 自维护的 `generations` 计数与服务端下发的 `session.generation` 重复（C-2）**
`Workspace.tsx:86,230,384` 仅用于 restart 后强制 remount `TerminalView`，而 `restartSession` 的响应已带新 `generation`（`store/models.go` 无 omitempty）且已 `updateSession`。改为 `key={`${session.id}:${session.generation}`}`，顺带获得"别的设备重启会话时本端随轮询自动 remount"的能力。

**H9. `TerminalView.tsx` 765 行：6 组 ref/state 镜像加 210 行连接 effect（C-3）**
`TerminalView.tsx:91-125` 有 `writer / ctrl / alt / liveStatus / active / preferences` 六组镜像和 4 个命令式句柄 ref；`connect / resetReplay / finishReplay / canSendInput / sendFrames` 全部定义在 `:254-465` 的 useEffect 内部，既读 ref 也调 setState，无法单独阅读或测试。这也是 `TerminalView.test.tsx` 需要 170 行假 xterm / 假 WebSocket 脚手架的根因。建议把传输层抽成不依赖 React 的 `TerminalConnection` 类，组件只留 setState 回调；**不建议**按 UI 区块拆组件，那只会增加 prop drilling。

**H10. 单测用正则匹配 `styles.css` 源码、断言 CSS 规则的书写顺序（C-4）**
`Sidebar.test.tsx:9,101-106` 用 `indexOf` 比较两条规则在源码中的先后；`TerminalView.test.tsx:11,285-288` 用正则匹配 `background` 与 `color` 的书写顺序。jsdom 未加载样式表，顺序对渲染无影响；`readFileSync('client/src/styles.css')` 相对 cwd 的路径还让测试只能从仓库根运行。删除。

**H11. `browser-smoke.mjs` 手写重造 `@playwright/test` 基础设施，并与 vitest 逐字重复（X-T01 / T02）**
`package.json:57` 只依赖 `playwright-core`，脚本自写 `findChrome`（7 个硬编码路径）、`availablePort`、`waitForHealth`、`stopChild`（`:744-802`）；50 处 `throw new Error` 代替带重试的 `expect`，因此出现 5 处固定等待（`:129,215,224,460,476`）；整个脚本是一个扁平 try 块，任何一处 throw 都中止后续全部检查。同时 12 组断言与 `HostManager.test.tsx:73-80`、`Sidebar.test.tsx:85-87`、`SSHConfigImport.test.tsx:63-110`、`SessionDialog.test.tsx:26`、`UI.test.tsx:85`、`Workspace.test.tsx:105-106` 等是同一字符串、同一语义的复制。建议换 `@playwright/test`（`webServer` 配置替代四个辅助函数），只保留 jsdom 做不到的部分：真实 PTY/tmux 往返、大尺寸 bracketed paste、reload 后重放隔离、WebGL 关闭回退、webfont 时序、构建版本注入。粗估 807 行可降到 250 行以内。

**H12. 在 `setState` updater 内部再派发两层 `setState`（C-5）**
`Workspace.tsx:151-156,188-200` 在 `setOpenIds` 的 updater 里调用 `setActiveId`，其中再嵌 `setCurrentView`。`main.tsx:17` 开着 StrictMode，updater 会被重复调用以暴露副作用，现在没出错只是因为内层恰好幂等。`closeTab` 不需要 `useCallback([])`（无 memo 化消费者），直接读 state 平铺三个 setState。

### 中

按主题归并；括号内为原始报告编号。

**跨包重复**

- **M1.** SSH 认证与严格指纹回调在 `internal/sshx/sshx.go:89-95,122-157` 与 `internal/terminal/ssh.go:217-229,235-289` 各写一份，连"passphrase 为空用 `ParsePrivateKey` 否则用 `WithPassphrase`"的细节都相同；`sshx.Credentials.AgentSocket`（`sshx.go:27`）无人设置；认证类型字面量在 `sshx.go` 与 `api/types.go:124-132` 重复，而 `store/models.go:6-8` 已有常量（S-12，X-14）。
- **M2.** `SessionRecord` / `SessionSpec` / `store.Session` 之间三处逐字段手写映射：`runtime_repository.go:38-56,138-151`、`manager.go:124-142`；两个结构体 13 / 11 个字段只差 3 项。建议 `SessionRecord` 内嵌 `Spec`（T-A7）。
- **M3.** local 与 ssh 各抄一份 screenrc（`local.go:345` vs `ssh.go:476-482`）和 tmux 服务器选项（`local.go:141-146` vs `ssh.go:411-414`）；两侧测试各断言一份，说明维护者已知必须手工同步。把数据提到 `backend.go` 共享，两侧各自渲染（T-B1）。
- **M4.** 同一组约束写了三遍：`api/types.go:103-170` normalize、`store/hosts.go:160-181` 与 `sessions.go:306-348` validate、`migrations.go:30-56` CHECK。中间层产生的 `ErrInvalidInput` 没有任何调用方区分（`handleStoreError` 只认 `ErrNotFound`，其余一律 500）。store 校验缩到 CHECK 表达不了的跨列规则（S-7）。
- **M5.** 两个 `newID`（`store/store.go:139` 裸 hex，`api/types.go:176` 带前缀），导致 host 与 auth session 无前缀、终端会话 `ses_`、WebSocket 客户端 `client_`，`store/sessions.go:25-31` 的兜底成了死代码（X-01）。
- **M6.** `host:port` 拼装：`api/types.go:184-186`（`fmt.Sprint`）与 `app/runtime_repository.go:80`（`strconv.Itoa`）同一表达式两种写法；`terminal/ssh.go:646` 同名 `sshAddress` 却是反向补默认端口（A-S8，X-02）。
- **M7.** tmux/screen 名称清洗写了三遍且上限不同：`main.go:157-160`、`backend.go:59-68`（32）、`backend.go:154-165`（40）（T-B7，X-13）。
- **M8.** 默认终端尺寸两个包两个值：`store/sessions.go:298-303` 的 120×36 被 `api/sessions.go:56-57` 同样的硬编码抢先，永不生效；`terminal/local.go:399-407` 是 80×24（S-18，X-10）。
- **M9.** 协议常量跨语言重复且 TS 侧退化为魔法数：帧标记 `0 / 1`、9 字节头在 `terminalProtocol.ts:35,81-84` 与 `websocket.go:394-397`、`terminal_test.go:68-69` 都是裸数字，两侧没有互指注释（X-09）。
- **M10.** `hostInput` 与 `hostPatch`（`api/types.go:20-38`）字段完全同构，`updateHost`（`hosts.go:73-112`）8 段手写合并，三个凭据字段用三种空值口径且无注释。用 `json.Decode` 预填合并可删一个类型和约 30 行；若"空串忽略"是有意的产品行为，保留并注释（A-S6）。
- **M11.** 会话名校验重复（`api/types.go:146-148` vs `sessions.go:120-125`），上限 80 硬编码三处，文案两句（A-S7）。
- **M12.** `transcript.Config` 与 `DirectoryConfig`（`store.go:42-47,427-432`）三字段相同，`Directory.Open` 手工搬运（S-9）。
- **M13.** `hasContent()` 递归遍历 Fragment（`UI.tsx:99-106`）只为兼容 `ConfirmDialog.tsx:390` 一处 `{error && ...}` 产生的 `''`；Fragment 分支生产代码永不触达，只有 `UI.test.tsx:19` 自己构造空 Fragment 来测它（C-7）。
- **M14.** 前端两套会话状态词表：`sessionStatus.ts:3-10` 与 `TerminalView.tsx:72-80`，6 个状态 5 个措辞不同，侧栏显示"运行中"时工具栏显示"已连接"（X-07）。
- **M15.** `notify` 的 tone 联合类型在 `TerminalView.tsx:50`、`HostManager.tsx:27`、`SSHConfigImport.tsx:9` 手抄三遍，`types.ts:98-102` 已有 `Toast`（C-14）。

**为不可能情形写的防御 / 为测试扭曲生产代码**

- **M16.** `Credential` 指针分支只为 `ssh_test.go:65` 一行存在：`sshAuthContext` 8 个 case、3 个空指针检查、`cloneSpec` 8 行深拷贝（T-A4）。
- **M17.** `signal()` 8 处调用只有 3 处有效；`publish` 每读一段 PTY 输出就做一次无接收者的 channel send（T-A5）。
- **M18.** terminate 守卫铺了四层（`manager.go:910-923`、`backend.go:79-81`、`local.go:248`、`ssh.go:500`），外加两条不可能返回的错误分支（`local.go:458`、`ssh.go:598`）伪装成运行时错误（T-A11）。
- **M19.** `Manager.Create` 的 `ctx` 参数在函数体内从未出现、返回值在 31 个调用点全部被 `_` 丢弃；`RefreshHost` 返回值生产两处丢弃（T-A6）。
- **M20.** `failureWindow.evictOldestLocked`（`middleware.go:185-196`）只在 5 分钟内出现 4096 个不同失败 IP 时触发，届时淘汰的多半是真实用户的记录，效果等价于"不记录新 key"，却多 12 行 O(n) 扫描（A-O4）。
- **M21.** `requestLog`（`middleware.go:40-48`）不记 status / 字节 / IP 且只 Debug 级；`response.go:24-28` 注释"the request logger will still record the disconnected response"描述的行为不存在（A-O5）。
- **M22.** `webui.Handler` 用 5 个文件名枚举 no-cache（`handler.go:24-34`），恰好穷举了除 `third-party-notices.txt` 外的所有非 assets 文件，等价于 `else { no-cache }`，却随 Vite 产物漂移（A-O6；缓存策略本身正确，见"不建议改"）。
- **M23.** `terminalFonts.ts:6-32` 两个注入参数只有测试传过；`openTerminalAfterFonts` 6 行函数体有 4 行是按顺序调三个回调，返回值生产丢弃；`TerminalView.test.tsx:109` 已用 `vi.mock` 替换 `loadFonts`，同一件事两套 mock（C-6）。
- **M24.** `parseControlMessage` 为服务端从不发送的 `isWriter / writable` 别名和 `disconnected / terminated / stopped` 状态写兼容分支（`terminalProtocol.ts:163-194`），前后端同一二进制发布不存在版本错配；`terminalProtocol.test.ts:100-115` 把死分支钉成契约（C-8，X-08）。
- **M25.** `sshconfig` 为 `User` 展开 8 个 %-token（`sshconfig.go:383-427`），其中 `%j` 需把 `proxyJump` 穿过 6 个参数，而 `ProxyJump` 非空的主机在 `api/ssh_config.go:80-83` 已被 422 拒绝，展开结果永不落库；与同文件 `${ENV}` fail-closed 的姿态不一致（S-10）。
- **M26.** `sshconfig.Discoverer` 接口与 `api/server.go:44-47` 的 `sshConfigDiscoverer` 逐字相同，生产者返回接口让具体类型不可见；`newWithHome` / `processUsername` 仅测试用却在非测试文件（S-11）。
- **M27.** `sshconfig` 11 处 ctx 取消检查散布在"读几 KB 本地文件"的路径上，`index%64` 循环在 `parser.go:294-299` 与 `sshconfig.go:287-292` 逐字重复且无注释，`contextError` 的 nil 分支不可达（S-17）。
- **M28.** `config` 的锁分三个文件，`lock_fallback.go` 的构建约束实测只覆盖 Windows，而 `creack/pty` 在 Windows 不可用；`removeOnClose` 字段只为这条路径存在，且 O_EXCL 方案崩溃后锁文件永久残留、语义更差（S-15，X-19）。
- **M29.** `store.Options.MaxOpenConns` 全仓库无人设置，`OpenWithOptions` 包外零引用；`Store.UpdateSession`（`sessions.go:104-150`，包内最复杂的 14 占位符 SQL）只有自己的两个测试调用（S-5，S-13）。
- **M30.** `socketEvent.ClientID / WriterID / Clients`（`websocket.go:37-43`）前端 `parseControlMessage` 不提取、`architecture.md` 未记载（A-O9）。

**风格与组织**

- **M31.** 结构化日志 message 中英混用：`internalError(w, action, err)` 把中文短语当 slog message（`auth.go:22,47,62`、`hosts.go:18`、`sessions.go:94` 等），同包其余日志是英文；`handleStoreError` 一律记"数据库操作失败"丢掉上下文（A-S3）。`internal/sshx` 是唯一用中文写错误的底层包（14 / 15 条），`"不支持的 SSH 认证方式"` 在 `api/types.go:134` 与 `sshx.go:155` 各写一遍，同文件全角与半角标点混用（X-03，S-21）。
- **M32.** 错误前缀约定分裂：transcript / terminal / sshconfig 全带 `包名: `，store / security / config 只有哨兵带；`security.go:39-42` 与 `:53-74` 同包两派（X-04）。
- **M33.** 超长函数：`terminalSocket` 182 行（`websocket.go:50-231`，末尾 select 8 个 case）、`run` 101 行（`main.go:55-155`）、`updateHost` 84 行、`createSession` 82 行（A-S5）；`startSSH` 六个失败点各写一遍清理，`ctx.Err()` 两种写法交替（`ssh.go:39-151`）（T-B2）；run loop 的三个 bool helper 名字与返回语义不符，`attempt *int` 出参外加三处赋值（T-B3）。
- **M34.** `createSession` 的 `sessionNameMu` 用 `defer` 覆盖整个 handler（含 SSH 建连），注释却说"命名和创建共享一个临界区"；同包 `createImportedSSHHost` 的写法恰好相反且注释正确（A-S4）。
- **M35.** `runtimeSession` 三个互斥量并排声明无归属注释，不可变字段 `created` 混在受保护字段里，`spec` 声明在 `mu` 之前却持锁改写（T-B5）。
- **M36.** `"backend"` 一词三个含义：io 流（`terminal/backend.go:19`）、持久化种类（`store.Session.Backend`、`socketEvent.Backend`、`types.ts:41`，在 `terminal` 包里叫 `Persistence`）、tmux 会话名（`BackendName`、`ErrBackendMissing`）（X-06）。
- **M37.** 通用错误响应辅助分散三个文件：`internalError` 在 `auth.go:226`、`handleStoreError` 在 `hosts.go:291`、`upstreamError` 在 `sessions.go:266`，而 `response.go` 只有 48 行；`failureWindow` 限流器放在 `middleware.go`，工具函数放在 `types.go`（A-S2，A-S11）。
- **M38.** store CRUD 样板 `rowsChanged + "check ..." + ErrNotFound` 三段式重复 10 处，两处文案相同（`sessions.go:144,272`），且包裹的 `RowsAffected()` 错误在 modernc sqlite 下不可达（S-6）。
- **M39.** `transcript.inspectSegment` 同一段截断恢复复制 4 次（`store.go:177-214`），循环体 55 行 4 层嵌套（S-8）。
- **M40.** 对话框挂载约定两套并存（条件挂载加恒为 true 的 `open` vs 常驻加 `open`），`RenameSessionDialog` 调用点保证非 null 却按 nullable 写三处防御（C-12）。
- **M41.** `AppState` 存整份 `StatusResponse`，`authenticated / setupRequired` 只写不读，还要伪造 `version: ''` 对齐类型（C-13）。
- **M42.** `Workspace` 9 个 `useCallback` 与 2 个 `useMemo` 只有 `notify` 必需（它进了 `TerminalView` 连接 effect 的依赖数组），其余组件无 `memo`；`SessionDialog.tsx:27-28` memo 了一次 `find`，而每次渲染扫全表的 `availableDefaultName` 反倒没 memo（C-11）。
- **M43.** `styles.css` 的 `.manager-view` 在 `:1138` 被设 `position:absolute; inset:0`，`:1497` 又 `position:relative; inset:auto` 推翻，`:1492` 与 `:1146` 逐字重复；`.about-*`、`.host-avatar` 各分散两处（C-15）。终端浮层 12 个裸十六进制色与全文件 token 化风格不一致（C-16）。
- **M44.** 测试脚手架：`api` 包三套 fixture（`lifecycle_test.go:35-74`、`terminal_test.go:29-53` 逐行复制、`ssh_config_test.go:67-91`）、两套创建会话、四个重叠的 socket 读取辅助；`terminal` 包四个只覆写 `Resize / Read` 的 stub 变体、`waitRunning` 等价于 `waitState(StateRunning)`、`sizeRecordingBackend.sizes` 只写不读、11 处 `GOOS == "windows"` 跳过而 CI 只有 ubuntu；`TestDiscardWorksInAnyState...` 中途无同步地改写 `manager.launcher`（A-O7，T-A13，X-18）。
- **M45.** 前端测试为"已删除的旧文案不存在"写了 8 处纯否定断言（`HostManager.test.tsx:73-80`、`Workspace.test.tsx:106-107`、`Sidebar.test.tsx:83-84` 甚至断言页面不出现字符 "1" 与 "2"），删掉组件也能通过；`TerminalView.test.tsx:520-559` 两个用例只在验证 `FakeWebglAddon` 自身（C-18，C-19）。
- **M46.** `browser-smoke.mjs` 其余问题：`assertDialogLayout` 92 行把 CSS token 的像素值（36 / 44 / 14 / 16-18）硬编码成断言且不指回 CSS（X-T03）；`WMUX_BROWSER_URL` 等 6 个环境变量零文档，7 处 `if (ownedServer)` 把脚本劈成两半（X-T04）；19 张截图写进 mkdtemp 且只在成功时打印路径，CI 无 upload（X-T05）；`modalChecks` 与成功 JSON 报告无消费者（X-T07）。
- **M47.** 工具链死配置与不一致：`pnpm-workspace.yaml` 的 `allowBuilds` 中 `cpu-features / node-pty / ssh2` 不在依赖树（X-T08）；`.env.example` 没有任何东西读它，与 systemd 示例指向不同路径，且缺两个变量（X-T09）；`dist` 同时指 Go 二进制目录与前端产物，`.dockerignore` 只列一个（X-T10）；`Dockerfile:20-22` 与 `build-server.sh:21-23` 两条 ldflags 路径不一致（X-T11）。CI 跑了 `pnpm build` 却没有 `git diff --exit-code -- internal/webui/dist`，改了 `client/src` 忘记提交重建产物时 CI 全绿而二进制内嵌旧 UI（X-T10）。

### 低

- **L1.** `reconnectDelay`（`backend.go:172-190`）为 `NewManager` 已归一化的输入再写默认值，退避循环的 `< maximum/2` 边界要算两遍（T-A8，X-11）。
- **L2.** `Attachment` 5 个方法的 nil 接收者检查与 `Close` 的 `sync.Once` 都多余，`detach` 已幂等（T-A9）。
- **L3.** Go 版本遗留写法：`stopTimer` 是 Go 1.23 前的 drain 模式，`CloseContext` 显式传循环变量是 Go 1.22 前写法，`go.mod` 声明 1.26（T-A12）；`sortedKeys` 可换 `slices.Sorted(maps.Keys())`，`setEnvironment` 可用 map 合并，transcript / sshconfig 仍 `import "sort"`，`sshconfig_test.go:806` 的 `contains` 就是 `slices.Contains`（T-B8，S-22）。
- **L4.** `configPath()`（`local.go:275-280`）从刚拼好的 argv 反解析上一行才构造过的变量，只因 `config` 声明在 case 块内（T-B4）。
- **L5.** "只保留最新值"的嵌套 select 写了两遍（`manager.go:777-791,831-844`），可抽泛型 helper（T-B6）。
- **L6.** 超时常量散落：screen 等待 2s / 20ms 在 `local.go:206,283` 与 `ssh.go:536`（40 × 0.05）三处；WebSocket 写超时 `10*time.Second` 三处硬编码而同文件顶部已有常量块（T-B9，A-S10）。
- **L7.** 命名：接口叫 `backendLauncher` 实现叫 `launcher`，四个 "launcher" 指三样东西；`output / pipe` 是同一 pipe 两端；`started` 两个含义；`Manager.callbacks` 与 `cfg.Callbacks` 同值；`launcher` 值类型持 `*sync.Mutex` 并对永不成立的 nil 兜底（T-B10）；`hostPatternsMatchAll` 实际含义是"匹配任意主机"（S-23）；日志字段 `session_id`（`websocket.go:97`）与其余 7 处 `session` 不一致（X-15）。
- **L8.** `issueLogin(w, ctx)` ctx 不在首位（A-S9）；8 个测试 helper 也把 ctx 放第二位（T-B11）。
- **L9.** 10 处无格式动词的 `fmt.Errorf`（`config.go:50-131`、`lock.go:28`、`security.go:53,123`）应为 `errors.New`，同包另有 `errors.New` 用法（X-05）。
- **L10.** `WMUX_LOG_LEVEL` 绕过 `internal/config` 直接 `os.Getenv`（`main.go:28,40-53`），`config_test.go` 覆盖不到（X-16）。
- **L11.** `PutAuthSession` 导出但只有包内一个调用方，注释描述的 "imports" 场景不存在（S-14）。
- **L12.** 不可触发的分支：`migrations.go:96-98`、`Store.Close` 的 nil 检查、`security.go:288` 前两个溢出条件已被 `:284` 的硬上界卡死、`DataDirLock.mu`（S-18，S-15）。
- **L13.** `transcript.Replay` 手写 5 次 `f.Close()`（`store.go:371-395`）；`Append` 轮转条件单行 175 字符（S-19，S-20）。
- **L14.** `sshx`：`authMethod` 返回 6 次 `func() {}`、`captured` 哨兵从未被 `errors.Is` 匹配、`ConstantTimeCompare` 用在公开的指纹上（S-21）。
- **L15.** `discoverSSHConfig` 刻意丢弃 `result.Source` 而无注释；`sshconfig.Result.Source` 生产无消费者（A-S12）。
- **L16.** 错误码无单一事实来源：`client/src/api.ts:207` 的 `terminal_config` 后端从不产生（A-S13，X-21）。
- **L17.** `logError` 可变参数包装只一处调用；`webui/embed.go` 9 行单文件只为一个导出变量，包外只用 `Handler()`（A-O10）；测试脚手架里 `fakeSSHConfigDiscoverer.resolveFunc` 从未赋值，`interface{ Result() *http.Response }` 的全部调用点都是 `*httptest.ResponseRecorder`（A-O8）。
- **L18.** `encodeTerminal*Frames` 的 `maxFrameBytes` 参数与 `assertUsableFrameSize` 从未被非默认值使用（C-20）；`api.ts` 的 `body()` 是 `JSON.stringify` 别名、`request` 的位置布尔参数、`ApiError.details` 无读者、`schemas` 导出唯一消费者是测一个无人读字段的用例（C-21，C-10）。
- **L19.** 前端零散：`Modal` 的 `symbol` 身份与两份 focusable 选择器（C-22）；`Sidebar.restartingIds` 默认值只为测试（C-23）；`preferences.ts` 混装 `isMobileLayout`，`loadPreferences` 四字段四种校验且回退值不走 `DEFAULT_PREFERENCES`，11 / 22 与 scrollback 档位跨文件重复（C-24）；4 处无注释的 `setTimeout(fit, …)` 与两个近重复的清修饰键函数（C-25）；`LiveStatus` 的 `'offline'` 永不到达（C-9）；`persistenceLabel` 参数放宽为 `string` 丧失穷尽检查（C-29）；空 `componentDidCatch`（C-30）；头像首字母渲染两处仅一处 `aria-hidden`（X-20）；`HostEditor` 三层嵌套三元中一支是恒真表达式（C-17）。
- **L20.** 死 CSS 与死属性：`.user-avatar--large`、`.warning-callout`、`.info-callout` 无使用；`.host-menu`、`.auth-shell--plain`、`modal-layer--form / --settings` 无规则；`data-terminal-ready / data-replay-complete` 重复标注；`.desktop-only` 无基础规则与 `.mobile-only` 不对称（C-26）。
- **L21.** PWA：`sw.js` 的 SHELL 不含 `/assets/*`，离线兜底返回的 HTML 引用的 bundle 必然 404，离线路径事实上不工作；每小时 `registration.update()`；`activateUpdate` 每次调用叠加监听器；`CACHE_NAME` 手工到 v6 无 bump 说明（C-28，X-T22）。
- **L22.** 构建与配置：`tsconfig.base.json` 唯一消费者立刻覆盖其两个关键项，`no-explicit-any: 'error'` 与 recommended 默认相同（C-27）；`test:all` 无引用，CI 与 npm scripts 各写一遍流程（X-T14）；prettier 靠手写路径列表，`.prettierignore` 3 / 4 条够不着（X-T15）；`Dockerfile:26` tab 与空格混用（X-T16）；`.dockerignore` 漏 `internal/webui/dist`（X-T17）；`.gitignore` 的 `/client/dist/`、`coverage/` 与 `.editorconfig` 的 `[Makefile]` 是死条目（X-T18 / T19）；`compose.yaml:6` 二次硬编码版本号，Node 版本四处各说各话（X-T12 / T13）；`icon-192.svg` 零引用却打进二进制（X-T20）；法律声明两份手工副本（X-T21）；`longFixtureName` 自检断言恒真（X-T06）；`version_test.go` 断言正上方字面量非空（S-24）；`store_test.go` 518 行单文件覆盖四个源文件，`now` 变量声明后 `_ = now`（S-24）。

## 报告之间的分歧与裁定

1. **`terminal.Config.TmuxPath / ScreenPath`。** T-A2 认为"导出了但产品接不上，应接入配置或降为非导出"，X-R7 认为是标准的测试注入。两者对事实的描述一致，分歧在取舍。裁定：与已有的非导出 `Config.launcher` 保持一致，降为非导出测试注入；若日后需要自定义路径，再通过 `internal/config` 暴露 `WMUX_TMUX_PATH`。
2. **`webui.Handler` 缓存头。** A-O6 建议把 5 个文件名枚举折叠成 `else no-cache`，X-R8 认为缓存策略"不能简化"。两者不冲突：A-O6 不改策略只改枚举，对现有文件行为完全一致，且不再随 Vite 产物漂移。采纳 A-O6。
3. **`internal/version/version_test.go`。** S-24 建议删，X-R3 认为"删不删无所谓"。采纳删除，11 行无信息量。

## 看起来复杂但合理，不建议改

合并五份报告的对应章节，供后续审查避免重复提出：

- **分层与适配。** `internal/app` 同时实现 `terminal.Repository` 与 `terminal.Callbacks`，是 `terminal` 不依赖 `store`、拿不到主密钥的原因；`main.go` 需在构造 Manager 前拿到它，不能住在 `api`。`sshx / sshconfig / terminal/ssh.go` 三分边界职责清晰、依赖单向。底层六个包完全不打日志，只返回错误。`internal/version` 独立成包是 `-ldflags -X` 需要包路径的结果。
- **terminal 包的并发结构。** `operationMu / sizeMu / mu` 分工；`backendLauncher` 接口加非导出 `Config.launcher`（20 多个生命周期用例靠它隔离 tmux / SSH）；`create bool` 承载"只有首次启动可创建多路复用器会话"的不变量；terminate 走独立控制连接；`context.AfterFunc` 接到 `Close()`；`subscriber` 四个 channel 对应协议四类消息；`permanentError` 分类；回放在 `s.mu` 内完成（上次审查 M7 的性能取舍，不是过度工程）；`runtimeSession` 持有 ctx 是长生命周期后台任务的公认例外。
- **api 包。** `session_locks.go` 的 `keyedMutex` 及其引用计数；安全中间件与 `originAllowed`、`decodeJSON` 的严格性；`Handler()` 对编译期不变量 panic；两阶段关停与 `dataMuxName`；`internal/api/lifecycle_test.go` 与 `internal/terminal/lifecycle_test.go` 无实质重复（前者测线格式与状态码，后者测状态机；`launcher` 未导出使跨包无法注桩，是包边界的合理结果）。
- **存储与安全。** transcript 尾部崩溃恢复、`failed` 中毒状态、segment 分片；store 的 generation 条件更新、`openMu` 加 `BEGIN IMMEDIATE`；scrypt 自描述哈希格式；主密钥硬链接发布；跨进程数据目录锁本身；`sshconfig` 自写解析器（惰性 Include、Match fail-closed、IdentityFile 只暴露布尔、User 禁环境变量都是安全语义，通用库不提供）及枚举 / 求值两条遍历；`sshx.Probe` 在回调里返回错误中止握手。
- **前端。** `ReplayBarrier` 与 generation 计数；每个 socket 监听器开头的身份检查；重连三件套与退避公式；`fit` 的 `useCallback` 加 `activeRef`；`useState(() => new ReplayBarrier())`；轮询 `requestId` 乱序保护；zod 运行时校验本身（要删的是重复定义）；`Modal` 栈加 `inert` 加焦点归还；`--mobile-layout` 单一断点源；WebGL 动态导入加降级；`encodeInto` 按码点分帧；`Dashboard` 作为私有子组件留在 `Workspace.tsx`。
- **工具链。** 把 `internal/webui/dist` 提交进 git（`go:embed` 要求编译期存在，让 `go install` 无需 Node）；`browser-smoke.mjs` 的 python3 探针（bracketed paste 与重放隔离必须真实 PTY）；`compose.yaml` 关于 `~/.ssh` 挂载的长注释；`eslint.config.js` 为 sw.js 与 scripts 单独配 globals。

## 建议的处理顺序

1. **纯删除，无行为变化。** H2、H6、M16、M24、M29、M30、L1、L2、L12、L16、L17、L18、L20、M47 的死配置、H10 的 CSS 源码断言、M45 的否定断言。先做能让后续重构少读一半代码。
2. **api 层收口。** H5（删接口、可变参数、nil 降级）与 M44 统一 fixture 是同一次改动；顺手做 M37 归位错误辅助、M31 日志 message 统一英文。
3. **契约收敛。** H3 加 H4（session DTO、删两列、简化 SQL）一个提交；H7 加 H8 加 M15（前端 schema 单源、复用 generation）一个提交；M9 协议常量互指。这是唯一同时动前后端的批次。
4. **terminal 包重构。** H1 拆 `manager.go` 并合并 teardown；M2、M3、M17、M18、M33（`startSSH` 与 run loop）、M35。
5. **跨包重复。** M1（`sshx` 复用 `terminal` 的 `Credential`）、M4、M5、M6、M7、M8、M32 与 L9。
6. **前端结构。** H9 抽 `TerminalConnection`（工作量最大，可与第 4 步并行）、H12、M13、M23、M40 至 M43。
7. **工具链。** H11 迁到 `@playwright/test` 并砍到 250 行以内，M46、M47、L21、L22 随之。

## 处理记录（2026-09-05）

修复由 8 个 Claude Opus 5 子代理分三波完成，每个子代理在独立的 git worktree 中按文件所有权工作，主会话核对改动范围后以 cherry-pick 并入分支 `refactor/review-2026-09-05`，每次并入后运行完整验证。三波分别是：① 包内清理（terminal+app / store+transcript+security+config+sshx+sshconfig / client 终端模块 / client 其余 / 工具链，5 个并行）；② 跨包契约（Go 契约 / client 契约，2 个并行）；③ `internal/api` 组织与文案。子代理的逐条报告保存在会话记录中，本节只列结论。

### 逐条状态

**高（12/12 完成）**：H1 `manager.go` 拆为 `manager.go`（357 行）+ `session.go` + `fanout.go`，三份 teardown 合并为 `teardown(ctx, reason, finalState)`；H2 全部死导出/死字段删除，`Callbacks` 缩为两个方法，`tmuxPath`/`screenPath` 降为非导出测试注入；H3 `sessionResponse` DTO、migration 3 删除 `backend_name`/`exit_code`、`store` 模型只保留 `Credentials` 的 json tag；H4 `UpdateSessionRuntime` 缩为 5 参数；H5 删 `sessionSpecProvider`、nil 降级与 `probeSSH` 兜底，api 测试 fixture 合并为 `newAPIFixture`；H6 `security` 只导出 `EncryptJSON`/`DecryptJSON`；H7 前端类型全部由 zod schema 推导；H8 `generations` state 删除，改用服务端 `generation`；H9 传输层抽为 `terminalConnection.ts`（314 行，无 React 依赖，12 个直接单测），`TerminalView.tsx` 765→507 行；H10 CSS 源码断言删除；H11 `browser-smoke.mjs` 807 行删除，替换为 `playwright.config.ts` + `tests/browser/smoke.spec.ts`（300 行，5 个用例，只保留 jsdom 做不到的场景）；H12 updater 内嵌套 `setState` 消除。

**中（47/47 完成）**：M1–M47 全部完成；M20、M21、M22、M31 与 M33 的 api 部分、M34、M37、M44 的 api 读取辅助由第三波完成。其中：M4 store 校验缩到跨列规则，且 `ErrInvalidInput` 现映射为 400；M12 第一波只加注释，第二波完成 `Limits` 内嵌；M31 的 sshx 部分第一波完成；M33 的 `startSSH`/run loop 部分第一波完成；M44 的 terminal 部分与 api fixture 已完成；M45 有一条断言经核实是对当前行为的正向断言，保留；M40 中 `SSHConfigImport` 的 `open` 是自身 state，保持不变。

**低（22/22 完成）**：L1–L22 全部完成；L8 的 `issueLogin`、L15、L17 的 api/app 部分由第三波完成。L21 选择删除 `sw.js` 的 navigate 兜底分支（离线行为不变）；L22 中 tsconfig 的处理与审查建议不同，见下。

**第三波（`internal/api` 组织与文案）**：错误响应辅助与 `sameOrigin` 归位到 `response.go`，`failureWindow` 移到 `ratelimit.go`；slog message 统一为固定英文并带 `action` 字段，`handleStoreError` 记录调用上下文；`createSession` 的命名锁收进 `reserveSessionRow`；`terminalSocket` 拆为校验/接入/泵/心跳四段，`main.go` 的 `run` 拆为 `openRuntime`/`serve`/`close`；`hostPatch` 与 8 段手写合并删除，改为预填 `hostInput` 后解码，三种凭据口径保留为一条带注释的显式判断并新增 `TestHostPatchCredentialMergeRules`；`validateDisplayName` 共用；`issueLogin(ctx, w)`；`socketWriteTimeout`；27 个错误码收为常量；`failureWindow` 改为"记录已满时新来源不计数"；`requestLog` 改为记录 method/path/status/bytes/duration（≥500 Warn、≥400 Info、其余 Debug，`/api/health` 只在失败时记录）；`webui.Handler` 缓存头折叠为 `assets/` → immutable、其余 → no-cache，`embed.go` 并入 `handler.go`；api 测试辅助全部收进 `helpers_test.go`，`lifecycle_test.go` 只剩测试函数。

### 与审查建议不同的判断

子代理在核实后做出了以下与报告建议不同的决定，主会话认可：

- **T-B3**：`retryAfterBackendError` 保留 `ErrBackendMissing` 守卫——删掉它等于把 attach-only 语义完全托付给每个 backend 的 `Reconnectable`，而测试 stub 并不满足该约定；改为上提到共用 helper 消除两处不一致。
- **T-A11**：两条不可达的 `Terminate` 错误分支直接删除而非改 `panic`——即使不变量被破坏，它们也只杀 attach 客户端进程，碰不到 tmux/screen 会话本身。
- **S-18**：`security.go` 的 `math` import 必须保留（`math.MaxInt` 在保留的第三个溢出条件里），审查称可删是笔误。
- **C-5 / H12**：轮询回调没有按字面建议直接读闭包里的 `openIds`（该 effect 依赖是 `[]`，闭包值停在挂载时刻，会误删用户之后新开的标签页），改为维护 `openIdsRef`。
- **C-18**：保留一个不含断言的 `@xterm/addon-webgl` 最小桩，否则测试输出会多出 jsdom 的 `getContext()` 警告。
- **C-26**：`modal-layer--form`/`--settings` 作为零成本的 BEM 钩子保留。
- **C-27**：两层 tsconfig 继承保留，但把 `module`/`moduleResolution` 下沉到 base 并删除死掉的 `NodeNext`；新增 Node 侧项目（`playwright.config.ts` + `tests/browser`）与 solution-style `tsconfig.json`，`pnpm typecheck` 改为 `tsc -b` 同时检查两个项目。理由：`tests/browser` 无扩展名导入 `../../playwright.config`，Playwright 的 loader 是 bundler 式解析，两个项目本就该共享同一套 module 设置。
- **C-29 / LiveStatus**：`LiveStatus` 改为 `SessionStatus` 的别名而非收窄成服务端实际发送的 5 元子集，否则 `normalizeLiveStatus('detached')` 的返回值会变化；子集关系写进注释。
- **M7**：`SafeMuxName` 降回非导出而非让 `main.go` 复用——`dataMuxName` 的 sha256 hex 本就不含非法字符。
- **M19**：`Manager.Create` 去掉 ctx（runtime 生命周期长于请求且自带取消，传请求 ctx 反而是错的），`RefreshHost` 返回值保留并在 `hosts.go` 记 Debug 日志。
- **M36**：只改名 Go 侧 helper（`MuxSessionName`、`ErrMuxSessionMissing`）与错误注释，`store.Session.Backend` 与 wire 字段 `backend` 保持兼容；进入 `last_error` 的错误文案未改。
- **H5**：保留 `logger == nil → slog.Default()`——它是导出构造器上的输出端默认值，不是依赖兜底。
- **M8**：store 里永不生效的默认尺寸删除后，8 处测试需显式写尺寸；未新增 Go 校验，单列约束继续由表上的 `CHECK (cols > 0)` 负责。
- **S-23 第三项**：`hasLiteralAlias` 整个删除，`Resolve` 直接查收集器的 map。
- **A-O5 / M21**：`requestLog` 选择"让它有用"而非删除——它是唯一的访问日志，删掉后 `sameOrigin`/`requireAuth`/`decodeJSON` 这几条最常见的拒绝路径将没有任何日志；`statusRecorder` 实现了 `Unwrap()`，WebSocket 升级不受影响（api `-race` 测试与 Playwright 真实 tmux 用例验证）。
- **A-S6 / M10**：核实前端从不发送空串的 `password`/`privateKey`（不改就整键省略），"空串忽略"是对其它客户端的有意兜底，因此保留该行为并写成一条带注释的显式判断，而不是统一三种口径。
- **X-14 store 侧**：给 store 常量加命名类型会波及 8 个文件约 84 处赋值，跳过；api 侧的 normalize 已改用 `store` 常量。

### 有意的行为变化（非纯重构）

1. 会话 JSON 不再包含 `backendName`、`exitCode`；WebSocket `hello`/`state` 不再包含 `clientId`、`writerId`、`clients`；错误码 `terminal_unavailable` 不再出现，新增 `invalid_input`（400）。二进制帧格式不变。
2. 数据库 schema 2→3（删除 `sessions.backend_name`、`sessions.exit_code`），单向升级，旧版 wmux 无法打开 v3 库。`UpdateSessionRuntime` 不再做变更检测，重复回调会重写同一行（不影响 `updated_at`）。
3. 所有配置错误统一以退出码 2 结束（此前只有非法 `WMUX_LOG_LEVEL` 退 2，其余退 1）。
4. 终端工具栏的状态文案与侧栏统一：正在连接→启动中、连接错误→异常、已退出→已结束；"已连接"保留并注释。
5. `sshconfig` 的 `User` 对 `%` token（`%%` 除外）fail-closed，此前展开 8 种 token。
6. `Manager.Discard` 失败时也从注册表移除 runtime（terminal 子代理额外发现的泄漏，独立 commit `fix(terminal)`）。
7. 本地构建也带 `-s -w`（dlv 调试需临时去掉）；`test:go` 改为 `-race` + `vet`；删除 `lock_fallback.go` 后 Windows 交叉编译不再通过（wmux 在 Windows 上本就无法运行）。
8. 浏览器验收测试改为 Playwright：固定端口 8788，`webServer` 启动 `bin/wmux`；不再把服务端 stderr 的 ERROR/WARN 计为失败（日志仍 pipe 到测试输出）。
9. `.env.example` 移到 `deploy/wmux.env.example` 并补齐两个变量；Go 二进制输出目录 `dist/` 改为 `bin/`；CI 新增 `git diff --exit-code -- internal/webui/dist` 守卫与失败时上传 Playwright 产物。
10. `sshx` 的错误文案改为英文（只进服务端日志，用户可见文案由 `upstreamError` 提供，不变）。
11. `WMUX_LOG_LEVEL` 经 `internal/config` 读取并校验。
12. 每个请求产生一条 `request` 日志（method/path/status/bytes/duration），4xx 记 Info、5xx 记 Warn；副作用：`http.MaxBytesReader` 不做 `Unwrap`，超过 2 MiB 的请求体不再提前关闭连接，状态码与响应体不变。
13. 创建会话时名称超长的提示统一为"会话名称不能为空且不能超过 80 个字符"（与重命名一致）；`third-party-notices.txt` 从无缓存头变为 `no-cache`。

### 验证

每一波并入后在主工作区运行：`gofmt -l`、`go vet ./...`、`go build ./...`、`go test -race -count=1 ./...`、tmux 与 screen 集成测试（`WMUX_TMUX_INTEGRATION=1` 三项、`WMUX_SCREEN_INTEGRATION=1` 一项，均 `-v` 确认真实执行）、`pnpm format:check`、`pnpm lint`、`pnpm typecheck`（`tsc -b`）、`pnpm test:unit`（12 个文件 72 个用例）、`pnpm build`、`pnpm test:browser`（5 个用例）。已知的既有 flake：`TestDialSSHHandshakeHonorsContextCancellation` 在 `-count=3 -race` 下偶发，根因是 socket deadline 与 ctx deadline 设在同一时刻，本次未改动其逻辑。

### 遗留与建议

- 研究备忘 `docs/research/2026-09-05-sshconfig-library-evaluation.md`：保留手写解析器；建议补一个以 `ssh -G` 为真值的差分测试、给相对 `Include` 加路径穿越守卫。
- 研究备忘 `docs/research/2026-09-05-ghostty-web-evaluation.md`：现在不换 ghostty-web；建议评估关闭 `allowTransparency`、处理上次审查的 M7（锁内回放）、建立性能基线脚本、锁定 `@xterm/xterm@6.0.0` 直到上游 #6106 关闭。
- PWA 离线形态（离线渲染出立刻报错的空壳）是产品决策，未在本次处理。
- `StatusResponse` 是未被引用的导出类型，为保持 schema/type 成对导出而保留。
- SSH 握手 flake 的正确性修法待做。
- `internal/api/types.go` 里剩余的裸数字（255/128/4096/8192、`validSize` 的 20/1000/5/500）与 `Dockerfile` 中重复的 `/api/health` 字面量未处理。
- X-14 的 store 常量类型化留给一次能同时改 `internal/store/**` 的任务。
