# wmux 代码审查：过度工程与代码风格（internal/api、cmd/wmux、internal/app、internal/webui）

审查对象：`main` @ `4fb3e9f`，工作树干净。
范围：`internal/api/*.go`（含全部测试）、`cmd/wmux/main.go`+`main_test.go`、`internal/app/runtime_repository.go`+测试、`internal/webui/*.go`，共约 4550 行，已逐行读完。
交叉核对：`client/src/api.ts`、`client/src/types.ts`、`client/src/terminalProtocol.ts`、`client/src/components/TerminalView.tsx`、`docs/architecture.md`、`docs/reviews/2026-09-04-independent-code-review.md`。
只读验证：`go build ./...`、`go vet ./...` 均无输出（通过）。

**不在本报告范围**：正确性 bug、安全漏洞、性能。上一轮审查已覆盖，本文不重复其 H1–H3/M1 等结论。

---

## 过度工程

### O1 [高] `sessionSpecProvider` 接口 + 可变参数 + 内部兜底构造，三套机制服务同一个实现

**位置**：`internal/api/server.go:35`、`server.go:40-42`、`server.go:52`、`server.go:56-59`；调用方 `cmd/wmux/main.go:112`

```go
type sessionSpecProvider interface {
	SessionSpec(context.Context, store.Session) (terminal.SessionSpec, error)
}

func New(cfg config.Config, database *store.Store, masterKey []byte, terminals *terminal.Manager,
	transcripts *transcript.Directory, logger *slog.Logger, providers ...sessionSpecProvider) *Server {
	...
	provider := sessionSpecProvider(&app.RuntimeRepository{Store: database, MasterKey: masterKey, Logger: logger})
	if len(providers) != 0 && providers[0] != nil {
		provider = providers[0]
	}
```

**问题**：`grep -rn "SessionSpec" --include=*.go` 全仓库只有 `*app.RuntimeRepository` 一个实现；`grep -rn "New("` 显示 6 个调用点中只有 `main.go:112` 传了第 7 个参数，而它传进去的正是 `New` 自己会构造的同一个类型；5 个测试调用点全部不传。也就是说：一个只有一个实现的接口、一个只有一个调用者的可变参数、一个永远等价于被覆盖值的兜底构造，三者叠加，却没有任何测试或生产代码从中获益。

`New` 因此变成 7 个参数（其中一个可变），签名比它承担的职责复杂得多。

**为什么是问题**：读者看到 `providers ...` 会以为存在多种 provider 或测试替身，需要 grep 全仓库才能确认没有；可变参数还掩盖了"传 nil 会怎样"的语义（代码里专门判了 `providers[0] != nil`，也是无人触发的分支）。

**建议**：删掉接口和可变参数，直接持有具体类型；`main.go` 复用同一个实例只是为了共享 `logStateChange` 的去重缓存，把它变成显式必填参数即可：

```go
// server.go
type Server struct {
	...
	sessionSpecs *app.RuntimeRepository
}

func New(cfg config.Config, database *store.Store, masterKey []byte, terminals *terminal.Manager,
	transcripts *transcript.Directory, logger *slog.Logger, runtime *app.RuntimeRepository) *Server {
```

若不想让 `api` 依赖 `app` 的具体类型，保留接口但删掉可变参数与兜底构造（改为必填参数）也已经足够，能去掉约 8 行。

---

### O2 [高] `terminals`/`transcripts` 可为 nil 的"降级模式"只为测试而存在，散落 9 处判空

**位置**：`internal/api/sessions.go:28`、`147`、`161`、`174`、`217`、`239`；`internal/api/hosts.go:137`、`204`；`internal/api/websocket.go:57`

生产路径上 `cmd/wmux/main.go:112` 永远传入非 nil 的 manager 和 recordings（`main.go:83-107` 构造失败会直接 `return err`）。nil 只可能来自 `internal/api/auth_test.go:28`、`auth_test.go:100`、`ssh_config_test.go:82` 这三处为了省事传 `nil, nil` 的测试。

后果是：

1. 三个 handler 有一条永不执行的早退分支，还带一个专用错误码：
   ```go
   if s.terminals == nil {
       writeError(w, http.StatusServiceUnavailable, "terminal_unavailable", "终端服务不可用")
       return
   }
   ```
   这个错误码一路传到了前端 `client/src/api.ts:202` 的 `publicMessages`，为一个不可能发生的状态维护了用户文案。
2. 判空风格还不统一：create/restart/reconnect/websocket 用 `== nil` 早退返回 503，而 `deleteSession`（`sessions.go:147`）、`updateHost`（`hosts.go:137`）、`trustHost`（`hosts.go:204`）用 `!= nil` 静默跳过。同一个不变量、两种处理方式，读者无法判断哪种才是本意。

**为什么是问题**：这是典型的"为不可能发生的情形写的防御代码"，而且它让真实的不变量（Server 一定持有终端运行时）无法从代码上读出来。

**建议**：把 `terminals`、`transcripts` 定为必填不变量，删掉全部 9 处判空和 `terminal_unavailable` 分支。测试侧的代价很小——`terminal.NewManager` 只需要一个 `transcript.Directory`（`t.TempDir()`），在 `Create` 之前不会启动任何进程。配合 O7 统一成一个测试 fixture，改动是净减少代码。

---

### O3 [中] `probeSSH` 的 nil 兜底是死分支，且重复两遍

**位置**：`internal/api/hosts.go:162-165`（probeHost）与 `internal/api/hosts.go:186-189`（trustHost）

```go
probe := s.probeSSH
if probe == nil {
	probe = sshx.Probe
}
```

`server.go:71` 在 `New` 里无条件设置 `probeSSH: sshx.Probe`；唯一手工构造 `&Server{...}` 的地方是 `sessions_test.go:24`，它只调用 `availableSessionName`，永远走不到这两个 handler。两个测试（`ssh_config_test.go:155`、`260`）确实会覆盖这个字段，但覆盖的是非 nil 的函数值。

**建议**：两处都改成直接 `s.probeSSH(...)`，删掉 8 行。

---

### O4 [中] `failureWindow.evictOldestLocked`：为一个防不住的场景写的 O(n) 淘汰

**位置**：`internal/api/middleware.go:156`、`185-196`

`fail()` 每次调用都会先 `pruneLocked(now)` 清掉全部过期项（`middleware.go:154`、`168-183`），因此只有在 5 分钟窗口内出现 4096 个**不同**的失败来源 IP 时，`len(f.entries) >= maxKeys` 才成立。届时 `evictOldestLocked` 会做一次全表扫描，删掉"最后一次失败时间最早"的一个 key。

问题在于：这时被淘汰的多半是**真实用户**的记录（攻击者的 IP 刚刚失败过，时间最新），也就是说这段代码在它唯一会触发的场景里，效果与"直接不记录新 key"完全等价（都等于对新来源放弃限流），却多了 12 行和一次 O(n) 扫描。

**建议**：保留 `maxKeys` 上限，用三行表达同样的策略：

```go
func (f *failureWindow) fail(key string, now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pruneLocked(now)
	if _, exists := f.entries[key]; !exists && len(f.entries) >= f.maxKeys {
		return // 记录已满：新来源本轮不计数，窗口过期后自然恢复
	}
	f.entries[key] = append(f.entries[key], now)
}
```

`middleware_test.go:53-74` 的 `TestFailureWindowPrunesAndCapsKeys` 断言的是"entries 不超过上限"和"过期项被清掉"，两者在新写法下依然成立。

---

### O5 [中] `requestLog` 中间件既不记录状态码也不记录结果，并且有一条已过期的注释依赖它

**位置**：`internal/api/middleware.go:40-48`；相关注释 `internal/api/response.go:24-28`

```go
func requestLog(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		if r.URL.Path != "/api/health" {
			logger.Debug("request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(started))
		}
	})
}
```

它是三层中间件之一（`server.go:113`），但产出的日志没有 status、没有字节数、没有 client IP，只有方法/路径/耗时，且只在 Debug 级别。它排除 `/api/health` 靠的是硬编码路径字面量（这个路径同时出现在 `server.go:78`、`middleware.go:44`、`Dockerfile:36`）。

更关键的是，`response.go:24-28` 的注释写着：

```go
if err := json.NewEncoder(w).Encode(value); err != nil {
	// Headers are already committed. The request logger will still record the
	// disconnected response; there is no second valid HTTP response to send.
	return
}
```

"the request logger will still record the disconnected response" 是**不成立的**——`requestLog` 不记录任何错误或状态。这条注释在描述一个不存在的行为。

**为什么是问题**：一个不能用来排障的访问日志，其存在只提供"看起来严谨"的价值；而依赖它的注释又误导后来者以为错误已被记录。

**建议**：二选一。要么让它有用（包一层 `statusRecorder` 记录 status/size，并把级别提到 Info 或按 status 分级）；要么删掉整个中间件，把 `response.go` 的分支改成不带假承诺的形式：

```go
// 头部已提交，无法再返回第二个有效响应。
_ = json.NewEncoder(w).Encode(value)
```

（顺带：`if err := ...; err != nil { return }` 出现在函数末尾时，`if` 本身是空操作，`_ =` 更直白。）

---

### O6 [中] `webui.Handler` 的 no-cache 文件名枚举等价于"不在 assets/ 下"，且会随构建产物漂移

**位置**：`internal/webui/handler.go:24-34`

```go
switch {
case strings.HasPrefix(requestPath, "assets/"):
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
case requestPath == "index.html" || requestPath == "sw.js" || requestPath == "manifest.webmanifest" ||
	strings.HasPrefix(requestPath, "icon-") || requestPath == "apple-touch-icon.png":
	w.Header().Set("Cache-Control", "no-cache")
}
```

`internal/webui/dist/` 当前内容是：`assets/`、`index.html`、`sw.js`、`manifest.webmanifest`、`icon-192.png`、`icon-192.svg`、`icon-512.png`、`icon-512.svg`、`apple-touch-icon.png`、`third-party-notices.txt`。也就是说这份枚举**穷举了除 `third-party-notices.txt` 以外的所有非 assets 文件**，而漏掉的那一个恰好是唯一得不到 `Cache-Control` 的响应；同一个函数的 SPA 兜底分支（`handler.go:39`）对所有未命中文件设的也正是 `no-cache`。

**建议**：折叠成一条规则，行为对现有文件完全不变、对新增文件更安全，同时消除与 Vite 产物的耦合：

```go
if strings.HasPrefix(requestPath, "assets/") {
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
} else {
	w.Header().Set("Cache-Control", "no-cache")
}
```

`internal/webui/handler_test.go:10-45` 的断言（manifest 的 Content-Type/Cache-Control、`/` 与 `/sw.js` 的 no-cache、SPA 兜底）全部继续通过。

---

### O7 [中] `internal/api` 测试里有三套并行的 fixture 和四个重叠的读取辅助函数

**位置**：
- fixture 三套：`lifecycle_test.go:35-74`（`newLiveAPIFixture` / `newLiveAPIFixtureWithReplayLimit`）、`terminal_test.go:29-53`（内联在 `TestLocalTerminalOverWebSocket` 里，逐行重复 store.Open → transcript.NewDirectory → RuntimeRepository → NewManager → New → httptest.NewServer）、`ssh_config_test.go:67-91`（`newSSHConfigAPIFixture` / `WithConfig`）
- 创建会话两套：`lifecycle_test.go:461-470` `createSessionForTest` vs `terminal_test.go:98-130` `createLocalSessionOverHTTP`（同样是 POST /api/sessions + 断言 201 + 解码 store.Session）
- setup 已经共享（`terminal_test.go:74` `setupOverHTTP` 被 lifecycle fixture 复用），说明"共用 fixture"本来就是这个包的既定方向，只是没做完
- 读取辅助四个：`readSocketEventForTest`（`lifecycle_test.go:522`，4 处调用）与 `awaitSocketEvent`（`lifecycle_test.go:581`）职责重叠，前者是后者去掉过滤能力的弱化版；`sendAndAwaitOutput`（`601`）与 `sendAndCollectOutput`（`622`）差别只在于后者返回累积的 bytes，前者可以直接用后者实现

另外 `lifecycle_test.go` 的辅助函数群（461-648）夹在测试函数中间——前面 6 个测试、后面 5 个测试，`waitForTerminalState` 又单独落在 `860`。文件的阅读顺序被打断。

**建议**：把 fixture 收敛为一个（`newAPIFixture(t, ctx, opts...)`，manager/transcripts 永远真实构造，配合 O2 一起删掉 9 处判空）；`readSocketEventForTest` 的 4 处调用改成 `awaitSocketEvent(..., "hello", nil)`；`sendAndAwaitOutput` 改为 `sendAndCollectOutput` 的一行包装；辅助函数统一放到文件末尾或单独的 `helpers_test.go`。

**关于与 `internal/terminal/lifecycle_test.go` 是否重复覆盖**：核对后结论是**没有实质重复**，见文末"合理"部分 R5。

---

### O8 [低] 测试脚手架里的死字段与不必要的匿名接口

**位置**：`internal/api/ssh_config_test.go:32`、`52`；`ssh_config_test.go:420-422`、`438-440`

- `fakeSSHConfigDiscoverer.resolveFunc func(string) (sshconfig.Candidate, error)` 只有声明（32）和读取（52）两处，`grep -rn "resolveFunc" internal/api/` 确认**从未被任何测试赋值**。整个"函数式覆盖"能力是为想象中的用例准备的。
- `assertAPIError` 与 `responseErrorCode` 的第一个参数写成 `interface{ Result() *http.Response }`，但全部 9 个调用点传的都是 `*httptest.ResponseRecorder`。为一个具体类型发明一个匿名接口，还让签名跨了三行。

**建议**：删掉 `resolveFunc` 及 `Resolve` 里对应的三行分支；两个断言辅助改成 `response *httptest.ResponseRecorder`。

---

### O9 [低] `socketEvent` 有三个既无人消费、也未写入协议文档的字段

**位置**：`internal/api/websocket.go:37`（`ClientID`）、`42`（`WriterID`）、`43`（`Clients`）；填充点 `websocket.go:120`、`125-126`、`330-331`

`client/src/terminalProtocol.ts:182-207` 的 `parseControlMessage` 只提取 `type/status/reason/writer/message/sequence/generation/truncated`，其余字段一律丢弃（`terminalProtocol.test.ts:132` 明确断言未列出的字段被丢掉）。`docs/architecture.md` 的 "WebSocket protocol" 一节列举了 `hello`/`state` 携带的内容（status、generation、sequence、truncated），也**没有**提到 clientId/writerId/clients。

（`Backend` 字段同样未被前端消费，但 architecture.md 第 55 行写明 "`hello` and `state` carry the backend status"，属于有意公开的协议字段，不建议删。）

**建议**：删掉 `ClientID`、`WriterID`、`Clients` 三个字段及其三处赋值；`clientID` 本身仍需生成（`websocket.go:79` 传给 `terminals.Attach`），只是不必回传给浏览器。若认为它们是有意保留的协议扩展点，则应写进 architecture.md，否则它们就是没有契约的字节。

---

### O10 [低] 零散的单次使用包装与不必要的导出

- `internal/app/runtime_repository.go:180-186` `logError(message string, err error, args ...any)`：可变参数包装，全仓库只有 `runtime_repository.go:119` 一处调用，展开成两行即可（`if r.Logger != nil { r.Logger.Error(...) }`）。
- `internal/webui/embed.go`：整个文件 9 行，只为一个 `var Assets embed.FS`；而 `Assets` 只被同包的 `handler.go:13` 读取，`grep -rn "webui\." --include=*.go` 确认包外只用到 `webui.Handler()`。应改为小写 `assets` 并并入 `handler.go`，减少一个导出符号和一个文件。
- `internal/api/hosts.go:272-274` `stringPointer`：只服务 `updateHost` 里的三行（`hosts.go:79-81`）。可接受，但若按 S6 重构掉 `hostPatch`，它会一起消失。

---

## 代码风格

### S1 [高] 会话响应直接复用 `store.Session`，主机响应却有专门 DTO——分层不一致，且脱敏靠"改字段值"

**位置**：`internal/api/sessions.go:20-24`、`sessions.go:271-281`（`publicSession`）vs `internal/api/types.go:42-53`、`internal/api/hosts.go:276-289`（`hostResponse` / `publicHost`）

主机走的是标准做法：api 定义自己的 `hostResponse`，`publicHost` 做显式映射。会话走的是另一条路：

```go
func publicSession(session store.Session) store.Session {
	session.BackendName = ""
	if session.Error != nil {
		message := "会话暂时不可用，请检查工作目录、命令或连接设置"
		...
		session.Error = &message
	}
	return session
}
```

即：`internal/store/models.go:62-82` 的 JSON tag 就是对外 API 契约，脱敏方式是"把敏感字段改写成空值/替换文案再序列化"。

**为什么是问题**：

1. 同一个包对两种资源用两种截然不同的方式，读者必须分别理解。
2. store 层任何字段增删都会静默改变 HTTP 契约。真实后果已经出现：`BackendName` 被 `publicSession` 恒置空（配合 `json:"backendName,omitempty"` 意味着**永远不出现**在响应里），`ExitCode` 在 `internal/store` 之外没有任何写入点（`grep -rn "ExitCode"` 只有 models、sessions.go 的读写扫描和迁移），也就是恒为 null——但 `client/src/api.ts:57,65` 和 `client/src/types.ts:42,50` 仍在为这两个字段维护 schema 和类型。契约两端已经不一致，只是没人发现。
3. 函数签名 `func publicSession(store.Session) store.Session` 无法从类型上区分"内部模型"和"对外视图"，很容易被误用（比如把 `publicSession` 的返回值再写回数据库）。

**建议**：统一到一种做法。考虑到项目规模，推荐让 `api` 同时拥有两个 DTO：

```go
// types.go
type sessionResponse struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Kind        string     `json:"kind"`
	HostID      string     `json:"hostId,omitempty"`
	HostName    string     `json:"hostName,omitempty"`
	Cwd         string     `json:"cwd,omitempty"`
	Command     string     `json:"command,omitempty"`
	Persistence string     `json:"persistence"`
	Backend     string     `json:"backend,omitempty"`
	Status      string     `json:"status"`
	Generation  int        `json:"generation"`
	Cols, Rows  int        `json:"cols"` // 略
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	LastAttachedAt *time.Time `json:"lastAttachedAt,omitempty"`
	Error       string     `json:"error,omitempty"`
}
```

这样 `backendName` / `exitCode` 从契约里消失是显式的（前端可同步删除），而不是靠一句 `session.BackendName = ""`。若不愿新增类型，反过来做也可以——删掉 `hostResponse`，在 store 层提供 `HasSecret`——但不要两种并存。

---

### S2 [中] 三个通用错误响应辅助函数分别定义在三个不同的 handler 文件里

**位置**：`internalError` → `internal/api/auth.go:226-229`；`handleStoreError` → `internal/api/hosts.go:291-297`；`upstreamError` → `internal/api/sessions.go:266-269`；而 `internal/api/response.go` 只有 `writeJSON`/`writeError`/`decodeJSON`

三者都是跨资源通用的：`internalError` 在 auth/sessions/hosts/ssh_config/websocket 五个文件里被调用；`handleStoreError` 在 sessions/hosts/websocket 里被调用；`upstreamError` 在 sessions 和 hosts 里被调用（`hosts.go:168`、`192`、`234`）。定义位置和使用范围完全不匹配——想知道"这个包怎么返回 500"，得先猜它在 auth.go 里。

**建议**：三个函数全部移入 `response.go`。`response.go` 目前只有 48 行，正好是它们的归宿；`server.go` 的 `sameOrigin`（`server.go:118-126`）同理，它是响应/中间件语义，不是 Server 装配。

---

### S3 [中] 结构化日志的 message 中英混用

`internalError(w, action, err)` 把 `action` 直接当作 slog 的 message（`auth.go:227`：`s.logger.Error(action, "error", err)`），而所有调用点传的都是中文短语：`"读取初始化状态"`（auth.go:22）、`"生成密码哈希"`（auth.go:47、147）、`"读取管理员"`（auth.go:62、114、134）、`"列出 SSH 主机"`（hosts.go:18）、`"准备终端配置"`（sessions.go:94）、`"生成终端客户端 ID"`（websocket.go:81）等。

同一个包里的其他日志则是英文：`sessions.go:150` `"terminate session for deletion"`、`sessions.go:163` `"remove terminal transcript"`、`websocket.go:97` `"attach terminal WebSocket"`、`ssh_config.go:168` `"SSH config is unavailable"`、`app/runtime_repository.go:132` `"terminal client dropped"`、`main.go:122` `"wmux is ready"`。

**为什么是问题**：slog 的 message 是日志检索的主键。中英混排让 `grep`/日志聚合无法用统一的模式匹配；而且面向用户的中文文案（写进 `writeError` 的 message）和面向运维的日志 message 在同一个参数里被复用，两者的受众和稳定性要求完全不同。

**建议**：日志 message 统一英文（与包内多数、与 main.go 保持一致），中文只保留在返回给浏览器的 `writeError` 文案里：

```go
func (s *Server) internalError(w http.ResponseWriter, action string, err error) {
	s.logger.Error("request failed", "action", action, "error", err)
	writeError(w, http.StatusInternalServerError, "internal_error", "服务发生内部错误")
}
```

顺带修掉 `handleStoreError`（`hosts.go:296`）——它对所有非 `ErrNotFound` 的 store 错误一律记 `"数据库操作失败"`，丢掉了调用上下文（是查主机、查会话还是删会话），排障时无法定位。

---

### S4 [中] `createSession` 的命名锁用 `defer` 覆盖了整个 handler，与同包 `createImportedSSHHost` 的写法相反

**位置**：`internal/api/sessions.go:41-43` vs `internal/api/ssh_config.go:108-139`

```go
	autoNamed := input.Name == ""
	// Naming and creation share one critical section so defaults stay distinct.
	s.sessionNameMu.Lock()
	defer s.sessionNameMu.Unlock()
```

注释说的是"命名和创建共享一个临界区"，但 `defer` 让锁一直持有到 handler 返回，中间包含 `s.terminals.Create(r.Context(), spec)`（`sessions.go:97`，SSH 会话要建连接、跑远端脚本）、再次 `GetSession`（`102`）和 `writeJSON`（`107`）。

而同一个包里处理**完全同类问题**（先查重再插入，SQLite 无唯一约束）的 `createImportedSSHHost` 写法恰好相反——把临界区收进一个小函数，并显式注明边界：

```go
// SQLite does not impose a uniqueness constraint on connection tuples.
// Serialize only the duplicate check and insert so a double-click or two
// tabs cannot import the same endpoint twice. Response I/O happens unlocked.
s.hostImportMu.Lock()
defer s.hostImportMu.Unlock()
```

**为什么是问题**：不是"锁得对不对"，而是同一个包对同一个模式给出两种示范，且其中一种的注释与实际作用域不符——读者按注释理解，会以为锁只覆盖命名和 `CreateSession`。

**建议**：按 `createImportedSSHHost` 的样子把 `sessions.go` 这段收进一个小函数，让作用域和注释一致：

```go
// reserveSessionRow 在一个临界区内完成默认命名与建行，其余 I/O 不持锁。
func (s *Server) reserveSessionRow(ctx context.Context, model store.Session, autoNamed bool) (store.Session, error) {
	s.sessionNameMu.Lock()
	defer s.sessionNameMu.Unlock()
	if autoNamed {
		name, err := s.availableSessionName(ctx, model.Name)
		if err != nil {
			return store.Session{}, err
		}
		model.Name = name
	}
	return s.store.CreateSession(ctx, model)
}
```

---

### S5 [中] 四个明显超长的函数

| 函数 | 位置 | 行数 | 具体问题 |
|---|---|---|---|
| `terminalSocket` | `internal/api/websocket.go:50-231` | 182 | 鉴权/校验/Accept/Attach/hello/replay/心跳循环七件事在一个函数里；末尾的 `select` 有 8 个 case，其中 `<-ticker.C` 分支本身又是 20 行、4 层嵌套 |
| `run` | `cmd/wmux/main.go:55-155` | 101 | 配置、目录锁、主密钥、DB、清理协程、transcript、terminal manager、restore、HTTP server、信号、两阶段关停 |
| `updateHost` | `internal/api/hosts.go:58-141` | 84 | 8 段几乎逐字重复的 patch 合并（见 S6） |
| `createSession` | `internal/api/sessions.go:27-108` | 82 | 校验、命名、建行、建 spec、启运行时、失败回滚、重读、序列化 |

**为什么是问题**：这几个函数都不是"复杂算法密集"，而是"步骤密集"，每一步都可以独立命名和独立阅读。它们也是这个包里唯一需要滚动屏幕才能看完的函数。

**建议**（只举最有价值的一个）：`terminalSocket` 拆成握手与泵两段，边界天然清晰：

```go
func (s *Server) terminalSocket(w http.ResponseWriter, r *http.Request) {
	auth, attachment, conn, ok := s.acceptTerminalSocket(w, r) // 鉴权/校验/Accept/Attach/hello/replay
	if !ok {
		return
	}
	defer conn.CloseNow()
	defer attachment.Close()
	s.pumpTerminalSocket(r.Context(), conn, attachment, auth) // select 循环
}
```

`run` 同理可拆出 `openRuntime(cfg, logger) (*runtime, error)` 与 `serve(...)`；`updateHost` 见 S6。

---

### S6 [中] `hostInput` 与 `hostPatch` 字段完全同构，合并逻辑靠 8 段手写 if，且三个凭据字段用了三种"空值"规则

**位置**：`internal/api/types.go:20-38`（两个结构体逐字段对应，唯一差别是 `hostPatch` 全部加了指针和 `omitempty`）；`internal/api/hosts.go:73-112`（合并）

```go
if patch.Password != nil && *patch.Password != "" {              // 空串 = 忽略
	input.Password = patch.Password
}
if patch.PrivateKey != nil && strings.TrimSpace(*patch.PrivateKey) != "" {  // 空白 = 忽略
	input.PrivateKey = patch.PrivateKey
}
if patch.Passphrase != nil {                                     // 空串 = 清空
	input.Passphrase = patch.Passphrase
}
```

三条相邻的分支用了三种不同的判空口径，没有一句注释说明为什么（大概是"密码/私钥不能被误清空，但口令可以被清空"，但这需要读者自己推断）。加上前面 5 段纯搬运的 `if patch.X != nil { input.X = *patch.X }`，`updateHost` 有一半篇幅是机械映射。

**建议**：`hostPatch` 可以整个删掉——Go 的 `json.Decode` 只覆盖出现在 JSON 里的键，把已有值预填进 `hostInput` 再解码即可自动完成合并：

```go
input := hostInput{
	Name: host.Name, Address: host.Address, Port: host.Port,
	Username: host.Username, AuthType: host.AuthType,
	Password: &credentials.Password, PrivateKey: &credentials.PrivateKey, Passphrase: &credentials.Passphrase,
}
oldAuthType := input.AuthType
if !decodeJSON(w, r, &input) {   // 只有请求里出现的键会被覆盖
	return
}
if input.AuthType != oldAuthType {
	input.Password, input.PrivateKey, input.Passphrase = nil, nil, nil
}
```

这样能删掉一个类型（types.go:31-38）和约 30 行合并代码。**注意语义差异**：显式传 `"password": ""` 在新写法下会清空密码，而当前实现会忽略它——如果这个"忽略空串"是有意的产品行为（前端不填就不改密码），请保留一条显式判断并**加注释说明**，而不是让三种不同口径静默共存。

---

### S7 [中] 会话名校验逻辑重复、上限魔法数字重复、文案不一致

**位置**：`internal/api/types.go:146-148`（创建路径）与 `internal/api/sessions.go:120-125`（更新路径）

```go
// types.go：sessionInput.normalize()
if len(v.Name) > 80 {
	return errors.New("会话名称不能超过 80 个字符")
}

// sessions.go：updateSession
if name == "" || len(name) > 80 {
	writeError(w, http.StatusBadRequest, "invalid_session", "会话名称不能为空且不能超过 80 个字符")
}
```

同一条业务规则写了两遍，上限 `80` 硬编码两处，规则本身还不一致（创建允许空名并自动命名，更新不允许），文案也是两句。主机名的同一条规则（`types.go:108`，同样是 80）又是第三处。

**建议**：抽一个 `func validateDisplayName(name string, allowEmpty bool) error`（或至少提取 `const maxDisplayName = 80`），两个调用点共用，文案只留一份。

---

### S8 [中] SSH 地址拼接在两个包里各写了一遍，实现细节还不同

**位置**：`internal/api/types.go:184-186` 与 `internal/app/runtime_repository.go:80`

```go
// internal/api/types.go
func sshAddress(address string, port int) string {
	return net.JoinHostPort(strings.Trim(address, "[]"), fmt.Sprint(port))
}

// internal/app/runtime_repository.go（LoadHost 内联）
Address: net.JoinHostPort(strings.Trim(host.Address, "[]"), strconv.Itoa(host.Port)),
```

同一个"去掉 IPv6 方括号再 JoinHostPort"的规则，一处用 `fmt.Sprint`，一处用 `strconv.Itoa`。api 侧的 `sshAddress` 有三个调用点（`hosts.go:166`、`190`、`223`）。

**建议**：留一份实现。`app` 已经是 store→terminal 的转换层，把它导出（`app.SSHAddress(host store.Host) string`）后 api 复用最自然；同时把 `fmt.Sprint(int)` 换成 `strconv.Itoa`（`fmt.Sprint` 走反射，对 int 是没必要的）。

---

### S9 [低] `issueLogin` 的 `context.Context` 不在第一个参数位

**位置**：`internal/api/auth.go:188`

```go
func (s *Server) issueLogin(w http.ResponseWriter, ctx context.Context) error
```

Go Code Review Comments 明确要求 context 作为第一个参数（`ctx context.Context`）。这是本包唯一一个把 ctx 放在后面的函数——`discardSessionRow`（sessions.go:235）、`availableSessionName`（sessions.go:246）、`createImportedSSHHost`（ssh_config.go:108）都是 ctx 在前。

**建议**：`func (s *Server) issueLogin(ctx context.Context, w http.ResponseWriter) error`，改 3 个调用点（auth.go:56、95、158）。

---

### S10 [低] WebSocket 写超时 `10*time.Second` 硬编码三处，而同文件顶部已有超时常量块

**位置**：`internal/api/websocket.go:248`（输入写入）、`398`（输出帧）、`408`（JSON 事件）；对照常量块 `websocket.go:18-27`

```go
const (
	clientInputFrame  = byte(0)
	serverOutputFrame = byte(1)
	maxSocketMessage  = 128 << 10

	socketStatePeriod = 2 * time.Second
	socketPingPeriod  = 30 * time.Second
	socketPingTimeout = 10 * time.Second
)
```

心跳/ping 的时长都提成了具名常量，写超时却散在三个函数里，且和 `socketPingTimeout` 值相同却语义无关（容易被误改成一个）。

**建议**：加 `socketWriteTimeout = 10 * time.Second` 并替换三处。

---

### S11 [低] 文件归属：`middleware.go` 装了限流器，`types.go` 装了工具函数

- `internal/api/middleware.go:117-201`：`failureWindow` 及其 5 个方法是登录失败限流器，只被 `auth.go:69-97` 使用，不是 `http.Handler` 中间件。放在 `middleware.go` 里让这个文件从 48 行变成 201 行。
- `internal/api/types.go:172-186`：`validSize`（被 websocket.go 用）、`newID`（被 sessions.go/websocket.go 用）、`sshAddress`（被 hosts.go 用）都是工具函数，不是请求/响应类型。

**建议**：`failureWindow` 移到 `auth.go` 或独立的 `ratelimit.go`；三个工具函数移到一个 `util.go`，或分别下沉到唯一使用它的文件。

---

### S12 [低] `discoverSSHConfig` 刻意丢弃 `result.Source`，但调用点没有注释

**位置**：`internal/api/ssh_config.go:21`、`44-48`、`51-56`

```go
result, err := s.sshConfig.Discover(r.Context())
...
writeJSON(w, http.StatusOK, sshConfigResponse{
	Available:  result.Available,
	Source:     s.publicSSHConfigSource(),   // ← 不是 result.Source
	Candidates: candidates,
})
```

这是有意为之（避免把运行账户的家目录路径泄露给浏览器，`ssh_config_test.go:348-371` 专门测了这一点），但在调用点看不出来，读起来像"取了 Source 却忘了用"。`sshconfig.Result.Source`（`internal/sshconfig/sshconfig.go:80`）在生产代码里因此没有任何消费者，只有 sshconfig 自己的测试在断言它。

**建议**：在 `ssh_config.go:46` 加一行注释说明原因（例如 `// 用配置值而非解析出的绝对路径，避免暴露运行账户的家目录。`）；如果确实无人需要，也可以考虑让 `sshconfig.Result` 不再返回 `Source`。

---

### S13 [低] 错误码没有单一事实来源：前端为一个后端从不返回的码维护文案

**位置**：`client/src/api.ts:207`

```ts
terminal_config: '会话配置无效，请检查工作目录和启动命令。',
```

`grep -rn "terminal_config" --include=*.go .` 全仓库无匹配——后端从未产生过这个错误码。同理 `terminal_unavailable`（api.ts:202）对应的是 O2 里那条不可达分支。

**为什么是问题**：错误码是跨端契约，目前两侧各自维护字符串字面量，漂移无法被任何检查发现。

**建议**（后端侧可做的部分）：把错误码集中成 `response.go` 里的一组常量（`const codeNotFound = "not_found"` 等），至少让"后端有哪些码"可以一处 grep 出来；删除已废弃的码时也有明确的清单可对照。前端侧的清理由前端负责人处理。

---

## 看起来复杂但合理，不建议改

### R1 `internal/app` 这一层有存在必要

它不是 api 与 terminal/store 之间的转发层。`RuntimeRepository` 同时实现 `terminal.Repository`（`ListSessions`/`LoadHost`）与 `terminal.Callbacks`（`OnSessionState`/`OnWriterChanged`/`OnClientDropped`），承担的是**解密与状态语义翻译**：把 `store.Credentials` 转成 `terminal.Credential` 接口（`runtime_repository.go:167-178`）、把 `terminal.SessionState` 映射成 `store.SessionStatus*`（`90-103`）。它的存在正是 `internal/terminal` 不必依赖 `internal/store`、也拿不到主密钥的原因，与 architecture.md "The terminal manager never writes session rows itself" 的设计一致。而且 `cmd/wmux/main.go:92-103` 必须在构造 `terminal.Manager` 之前就拿到它，因此它也不能住在 `internal/api` 里。

唯一的小意见（不构成建议改动）：包名 `app` 对"一个适配器类型"来说偏泛；`OnWriterChanged` 的空实现（`runtime_repository.go:127`）是接口要求，保留正确；`logStateChange` 的 `states` map（`188-221`）确实是为了抑制日志噪音而存在的额外状态，但 `terminal.Manager` 会在 client 数量变化等场景下重复回调同一个 state（`manager.go:406`、`684`、`748`），所以这个去重是有实际作用的。

### R2 `session_locks.go` 的 `keyedMutex` 必要且实现克制

architecture.md 明确写了 "Delete, restart and reconnect requests for the same session ID are serialized in the HTTP layer"，这是有意设计。`holders` 引用计数（`session_locks.go:26`、`34-38`）不是过度设计：`deleteSession`（`sessions.go:141`）在**校验会话存在之前**就加锁，所以任意路径参数都会创建 entry，没有回收就会随请求无限增长。40 行实现里没有多余抽象。

### R3 安全相关的中间件与 `originAllowed`

`securityHeaders`、`recoverRequests`、`originAllowed`（含 `Sec-Fetch-Site`、DNS rebinding 的 IP/localhost 白名单、`rightmostForwardedValue` 从可信右端取值）都有明确的威胁模型和对应测试（`middleware_test.go:9-51`），注释解释的是"为什么"而不是"做了什么"。`decodeJSON`（`response.go:35-48`）的 `DisallowUnknownFields` + 第二次 `Decode` 拒绝多个 JSON 对象也是必要的严格性，`ssh_config_test.go:336-345` 有针对性测试。

### R4 `Handler()` 里 `fs.Sub` 失败时 `panic`

`internal/webui/handler.go:14-16` 的 panic 针对的是编译期不变量（`//go:embed all:dist` 保证 dist 存在），不是运行时错误处理，符合 Go 惯例。

### R5 `internal/api/lifecycle_test.go` 与 `internal/terminal/lifecycle_test.go` 没有实质重复覆盖

逐个核对后：api 侧测的都是 **HTTP/WebSocket 线格式与端点契约**——replay 分界的序号连续性与 `replay_end` 语义（`lifecycle_test.go:76-168`）、`SetReadLimit` 的边界与 1009 关闭码（`170-208`）、restart 时的 1013 + `disconnect{reason:restarted}`（`817-858`）、logout 后心跳撤销连接的 1008（`735-770`）、退出前把缓冲输出冲干净再发 `state:exited`（`772-815`）、删除时 200+warning 与 204 的分支（`650-708`）。terminal 侧测的是 manager 内部状态机（`TestStopForRestartClosesClientsWithRestartedReason`、`TestTerminateExitedSessionNeverContactsTheBackend`、`TestDiscardWorksInAnyStateAndNeverKillsTheBackend` 等），用的是 `launcherStub`/`backendStub`，**够不到** WebSocket 编码和 HTTP 状态码。

api 测试必须启真实子进程（`cat`、`sh` 循环），是因为 `terminal.Config.launcher` 是未导出字段（`terminal/types.go:178`），跨包无法注入桩——这是包边界的合理结果，不是测试设计问题。真正该收敛的只有 O7 里说的 fixture 与辅助函数重复。

### R6 `cmd/wmux/main.go` 的启动/关停装配整体合理

两阶段关停（先 `httpServer.Shutdown` 再 `terminalManager.CloseContext`）、`errors.Join(shutdownErrors...)` 聚合、收到第一个信号后 `signal.Stop` 让第二个信号能强制退出（`main.go:137-138` 的注释解释得很清楚）、`dataMuxName` 用 data dir 哈希做 tmux 命名空间隔离（并有 `main_test.go:33-45` 覆盖），都是恰当的。`defer stopMaintenance()` 与显式 `stopMaintenance()`（`main.go:81`、`142`）并存看着冗余，但显式那次是为了在关停前就停掉后台清理，是合理的。唯一的意见是函数长度（见 S5）。

---

## 建议的处理顺序

1. **O1 + O2**（同一次改动最省事）：删掉 `sessionSpecProvider` 的接口/可变参数/兜底，把 `terminals`/`transcripts` 定为必填，删掉 9 处判空与 `terminal_unavailable` 分支。净减约 40 行，`New` 的签名和不变量变得可读。
2. **O7**：统一测试 fixture（是 1 的前置条件），顺手合并 4 个重叠的 socket 读取辅助、删掉 O8 的 `resolveFunc`。
3. **S1**：为 session 补 DTO（或反向统一），同时清掉恒空的 `backendName`/`exitCode` 契约。这是唯一会影响前端的一条，建议单独一个提交。
4. **S2 + S3**：错误响应辅助归位到 `response.go`，日志 message 统一英文。纯机械改动，风险最低，可以最先做。
5. **O3、O4、O5、O6、S7、S8、S9、S10**：各自独立的小清理，可以合成一个 "cleanup" 提交。
6. **S4、S5、S6**：函数拆分与 `hostPatch` 重构，改动面最大，建议放最后并逐个提交。
