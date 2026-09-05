# `internal/terminal` 过度工程与代码风格审查（2026-09-05）

审查对象：`main` @ `4fb3e9f`，范围 `internal/terminal/` 全部 11 个文件（约 5100 行），另读了调用方
`internal/app/runtime_repository.go`、`internal/api/websocket.go`、`internal/api/sessions.go`、
`internal/api/hosts.go`、`cmd/wmux/main.go`。

只覆盖两类问题：**过度工程**与**代码风格**。正确性、安全、性能不在范围内（已由
`docs/reviews/2026-09-04-independent-code-review.md` 覆盖），本文不重复其结论。
`docs/architecture.md` 中说明的有意设计（generation、attach-only、ReplayBarrier、
单写多读 lease、独立控制连接 terminate）都不作为问题提出，相关内容见文末第三节。

辅助命令：`go build ./...`、`go vet ./internal/terminal/`、`gofmt -l internal/terminal/` 均干净；
本机无 staticcheck/golangci-lint。所有"无调用方/未使用"的结论都用 `grep -rn` 覆盖全仓库
（含 `*_test.go` 与 `client/`）核实过。

---

## 总体判断

这个包的**运行时语义**是干净的：一个 `Manager` 管一张 map，一个 `runtimeSession` 一条 run loop，
backend 抽象只有 6 个方法，锁的划分基本正确。问题不在架构，而在三处堆积：

1. `manager.go` 1092 行装了 5 个层次的东西，其中 `finishStop`/`discard`/`shutdown`
   是同一套 teardown 的三份手写副本。
2. 包里有一批**只有测试用或完全没人用**的导出符号、结构体字段和方法（约 15 处），
   它们让"这个包对外提供什么"变得不可读。
3. local 与 ssh 两条路径把**相同的配置数据**（screenrc 内容、tmux 选项清单）
   各写了一遍，必须手工保持同步。

下面按类别列出，编号 A=过度工程，B=风格。

---

## A. 过度工程

### A1（高）`manager.go` 承担 5 层职责，其中三份 teardown 是同一段代码的手写副本

**位置**：`internal/terminal/manager.go:1-1092`，重点 `finishStop`（945-973）、
`discard`（976-1002）、`shutdown`（1004-1035）。

文件里混了五件事：

| 行 | 职责 |
|---|---|
| 15-323 | `Manager`：配置默认值、session map 注册表、公开 API |
| 325-385 | `subscriber` / `runtimeSession` 数据结构 |
| 387-733 | run loop、连接/重连状态机、`publish` 扇出 |
| 736-875 | 状态快照、通知、writer 选举、订阅者关闭 |
| 877-1035 | stop / discard / shutdown 三条拆卸路径 |
| 1037-1092 | spec 校验与深拷贝 |

真正的重复在最后一块。三个函数做的是同一件事——"置 closed、取出 backend、cancel、
关客户端、关 backend、等 run loop 结束、关 transcript"——只在**关闭顺序**、
**close reason** 和**错误处理**上有差别：

```go
// finishStop:  cancel → b.Close() → closeClients(reason) → 等 done（超时忽略）→ 置 Terminated → log.Close()
// discard:     cancel → closeClients(Exited) → b.Close()  → 等 done（超时返回 err）→ log.Close()
// shutdown:    cancel → closeClients(ServerShutdown) → b.Close() → 等 done（超时起 goroutine 兜底）→ log.Close()
```

三份副本还导致了**不一致的可观察行为**：只有 `finishStop` 会把状态置为 `StateTerminated`
并推送最后一次 `OnSessionState`，`discard` 和 `shutdown` 不会。这类差异如果是有意的，
应该显式表达；现在只能靠比对三段代码才能发现。

**建议**：抽一个内部方法，把差异做成参数；同时把 `runtimeSession` 整体移出 `manager.go`。

```go
// teardown 结束一次执行；finalState 为空表示不改状态。
func (s *runtimeSession) teardown(ctx context.Context, reason AttachmentCloseReason, finalState SessionState) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	b, started := s.backend, s.started
	s.backend = nil
	s.cancel()
	s.mu.Unlock()

	s.closeClients(reason)
	var err error
	if b != nil {
		err = b.Close()
	}
	if started {
		select {
		case <-s.done:
		case <-ctx.Done():
			err = errors.Join(err, ctx.Err())
		}
	}
	if finalState != "" {
		s.setFinalState(finalState)
	}
	return errors.Join(err, s.log.Close())
}
```

拆分建议（纯移动，不改逻辑）：

- `manager.go`：`Manager` + `Config` 默认值 + 注册表（约 320 行）
- `session.go`：`runtimeSession`、run loop、`launchSpec`、teardown（约 500 行）
- `fanout.go`：`subscriber`、`publish`、`notify*Locked`、writer 选举（约 180 行）
- `attachment.go` 里现有的 `runtimeSession.attach`/`detach`（`attachment.go:29`、`199`）
  也应随 `runtimeSession` 一起走，现在它们是唯一定义在别处的 `runtimeSession` 方法。

---

### A2（中）一批导出符号没有任何包外调用方

逐个 `grep -rn` 核实（全仓库，含测试与 `client/`）：

| 符号 | 位置 | 情况 |
|---|---|---|
| `IsPermanentStartError` | `backend.go:145-146` | **零调用方**，包内外皆无。只是 `isPermanentStartError` 的一行转发 |
| `sshAuth` | `ssh.go:231-233` | 只有 `ssh_test.go` 调用；生产代码走 `sshAuthContext`。非测试文件里的测试专用函数 |
| `Callbacks.OnWriterChanged` | `types.go:146` | 仓库内**全部三个实现都是空函数**（`runtime_repository.go:127`、`types.go:153`、`manager_test.go:335`），却有 5 处触发点（`attachment.go:82/175/220`、`manager.go:681/870`） |
| `NopCallbacks` | `types.go:150-154` | 只被 `manager.go:30` 用作默认值，无包外使用，不必导出 |
| `Config.TmuxPath` / `Config.ScreenPath` | `types.go:173-174` | 生产从不设置（`cmd/wmux/main.go:93-103` 没有，`internal/config` 里也没有对应环境变量），只有测试用 |
| `SessionStatus.LastSequence` | `types.go:140` | 生产无读者；`websocket.go` 的 `sequence` 用的是自己维护的 `delivered`（`websocket.go:141/332`）。只有 `lifecycle_test.go:1041` 断言它 |
| `Manager.RefreshHost` 的返回值 | `manager.go:197` | `hosts.go:138`、`hosts.go:205` 两处都丢弃返回值，只有测试断言 |
| `Attachment.Write` | `attachment.go:100-102` | 只有测试调用；`websocket.go:249` 用 `WriteContext` |

**为什么是问题**：这些符号让包的"对外契约"看起来比实际大一倍。特别是
`Callbacks.OnWriterChanged`——它使 `Attachment.TakeControl`、`detach`、`publish`
三处都要在锁外多做一次回调分发，而接收端什么也不做。

**建议**：

```go
// types.go
type Callbacks interface {
	OnSessionState(status SessionStatus)
	OnClientDropped(sessionID, clientID, reason string)
}
// 删除 OnWriterChanged 后，attachment.go / manager.go 里 5 处调用与配套的
// writerChanged 布尔、writer 局部变量一并消失。

// backend.go：删掉 IsPermanentStartError；把 backendName 直接改名为 BackendName，
// 删掉 backend.go:167-170 的转发包装（backendName 包内 5 处调用改名即可）。

// ssh.go：删掉 sshAuth，测试改调 sshAuthContext(t.Context(), cred)。

// types.go：NopCallbacks → nopCallbacks；LastSequence 若无人读则删除，
// 连同 runtimeSession.lastSeq（manager.go:355/380/650-652/772）一起去掉。
```

`Config.TmuxPath`/`ScreenPath`：要么在 `internal/config` 里补上
`WMUX_TMUX_PATH`/`WMUX_SCREEN_PATH` 让它成为真正的配置项，要么改名为
明确的测试注入（和已有的 `Config.launcher` 一样降为非导出字段）。现在这种
"导出了但产品里接不上"的状态最糟。

---

### A3（中）三个 backend 结构体字段被写入但从不读取

- `localBackend.toolPath`（`local.go:23`，写于 `local.go:99`）——无读取点
- `localBackend.name`（`local.go:24`，写于 `local.go:100`）——无读取点
- `sshBackend.name`（`ssh.go:29`，写于 `ssh.go:133`）——无读取点

`grep -rn "toolPath\|b\.name" internal/` 确认：这三个字段只在构造时赋值。
`backendName(spec.ID)` 的结果通过 `l.tmuxArgs(...)` 在启动时就用完了，
终止走的是 `launcher.terminate` 的独立控制连接，不需要 backend 记住名字。

同类的还有 `SessionSpec.Name`（`types.go:94`）与 `SessionRecord.Name`（`types.go:112`）：
`grep -rn "\.Name" internal/terminal/` 全包只有 `manager.go:126` 一处
（把 record.Name 赋给 spec.Name），此后再无读者；`runtime_repository.go:40/140`
两处填充也是白填。以及 `OutputFrame.Time`（`types.go:158`）——
`publish`（`manager.go:653`）与 `attach`（`attachment.go:53`）都会写它，
但 `websocket.go:393-401` 的二进制帧只用 `Sequence` 和 `Data`，全仓库无 `frame.Time` 读者；
同一结构体上的 `json:"..."` 标签（`types.go:157-159`）也从未被 `json.Marshal` 用到
（包内 `grep json` 只命中这三行）。

**建议**：删掉 5 个字段 + 3 个 json 标签。`SessionSpec.Name` 若确实想保留给日志，
应该真的在某处打印它，否则它只是让 `restoreOne` 和 `RuntimeRepository` 各多一行赋值。

---

### A4（中）`Credential` 的指针分支只为一行测试而存在

**位置**：`ssh.go:237-241`、`247-251`、`267-271`；配套的
`cloneSpec` 深拷贝分支 `manager.go:1070-1077`。

`sshAuthContext` 的类型 switch 有 8 个 case：三种凭据 × (值 / 指针) + `nil` + `default`。
指针形态由谁构造？`grep -rn "&PasswordCredential\|&PrivateKeyCredential\|&AgentCredential"`
全仓库只有 `ssh_test.go:65` 一处。生产侧 `runtime_repository.go:167-178` 只返回值类型。

于是：3 个指针 case（12 行）+ 3 个 `if value == nil` 空指针检查 + `cloneSpec` 里
`*PrivateKeyCredential` 的 8 行深拷贝，全部服务于一个测试写法。

**建议**：只保留值类型 case，测试改成传值。

```go
func sshAuthContext(ctx context.Context, credential Credential) ([]ssh.AuthMethod, []io.Closer, error) {
	switch value := credential.(type) {
	case PasswordCredential:
		...
	case PrivateKeyCredential:
		...
	case AgentCredential:
		...
	case nil:
		return nil, nil, errors.New("terminal: SSH credential is required")
	default:
		return nil, nil, fmt.Errorf("terminal: unsupported SSH credential %T", credential)
	}
}
```

`cloneSpec`（`manager.go:1057-1081`）随之从 25 行缩到 15 行。

---

### A5（中）`signal()` 在输出热路径上被无意义调用；8 处调用只有 3 处有效

**位置**：`manager.go:793-798`（定义）；调用点 `attachment.go:80`、`attachment.go:218`、
`manager.go:647`、`manager.go:686`、`manager.go:940`、`manager.go:955`、`manager.go:993`、`manager.go:1019`。

`wake` 的唯一消费者是 `waitIfTerminating`（`manager.go:617`）与
`waitForRetry`（`manager.go:704`、`716`）。这两个函数**只在没有活动 backend 时运行**；
而 `publish` 只会从 `consume`（`manager.go:627`）调用，即只在 backend 活着时运行。
两者互斥，所以：

- `manager.go:686`——**每读到一段 PTY 输出就做一次 channel send**，永远没有接收者，
  只是往容量 1 的 buffer 里塞一个陈旧 token，让下一次 `waitForRetry` 白转一圈。
- `manager.go:647`（transcript append 失败分支）同理。
- `manager.go:955`/`993`/`1019` 都紧跟在 `s.cancel()` 之后，`ctx.Done()` 已经能唤醒
  两个等待点，`signal()` 是冗余的。

真正需要的只有 `attachment.go:80`（首个客户端接入 → 让 `waitForRetry` 从"无限等"切到退避）、
`attachment.go:218`（最后一个客户端离开）、`manager.go:940`（`abortStop` 把 `terminating` 置回 false）。

**建议**：删掉 5 处冗余调用，并在 `signal` 上写清它的契约：

```go
// signal wakes waitForRetry / waitIfTerminating after a client-count or
// terminating-flag change. The run loop's own ctx covers cancellation.
func (s *runtimeSession) signal() { ... }
```

---

### A6（中）`Manager.Create` 的 `ctx` 参数和返回值都没有人用

**位置**：`manager.go:61-102`。

- `ctx` 在函数体内**一次都没出现**（`sed -n '61,102p' | grep ctx` 只命中签名行）。
  这是有意的——run loop 用自己的 background context，Create 不该被请求生命周期取消——
  但签名没有表达这一点，读者会以为传进去的 ctx 起作用。
- 返回的 `SessionStatus`：31 个调用点（`sessions.go:97`、`sessions.go:201` + 29 处测试）
  **全部写成 `_, err :=`**。

**建议**：二选一。要么让签名说实话：

```go
// Create starts the runtime for a persisted session row; only this first launch
// may create the tmux/screen session. The runtime outlives the caller's request,
// so Create takes no context.
func (m *Manager) Create(spec SessionSpec) error
```

要么保留 ctx 但真的用它（`if err := ctx.Err(); err != nil { return err }`，
和 `Attach`（`manager.go:178-180`）保持一致）。同样地，`RefreshHost` 的
`int` 返回值（`manager.go:197`）生产侧两处都丢弃，可以改成无返回值，
或者由 `hosts.go` 记进日志。

---

### A7（中）`SessionRecord` / `SessionSpec` 之间存在三处逐字段手写映射

**位置**：
- `internal/app/runtime_repository.go:38-56`（`store.Session` → `terminal.SessionRecord`）
- `internal/app/runtime_repository.go:138-151`（`store.Session` → `terminal.SessionSpec`）
- `internal/terminal/manager.go:124-142`（`SessionRecord` → `SessionSpec`）

`SessionRecord`（`types.go:110-124`，13 字段）与 `SessionSpec`（`types.go:92-107`，11 字段）
只差三项：`HostID string` vs `Host *HostSpec`、`ResolvedPersistence`、`Active`。
其余 10 个字段在三处各抄了一遍，任何字段增删都要改三个地方——而
`Name` 字段（见 A3）正是这样一路抄进来却没人读的。

**建议**：让 `SessionRecord` 直接包住 spec，`restoreOne` 的整段构造随之消失。

```go
// types.go
type SessionRecord struct {
	Spec                SessionSpec // Spec.Host is nil; HostID names the host to load.
	HostID              string
	ResolvedPersistence Persistence
	Active              bool
}

// manager.go: restoreOne
spec := cloneSpec(record.Spec)
if record.HostID != "" {
	host, err := m.cfg.Repository.LoadHost(ctx, record.HostID)
	if err != nil {
		return fmt.Errorf("load host: %w", err)
	}
	spec.Host = &host
}
```

`RuntimeRepository` 侧也只剩一个 `store.Session → SessionSpec` 的映射函数，
`ListSessions` 与 `SessionSpec()` 共用它。

---

### A8（低）`reconnectDelay` 为不可能的输入写了默认值，且退避写法绕

**位置**：`backend.go:172-190`。

```go
if minimum <= 0 { minimum = 250 * time.Millisecond }
if maximum <= 0 { maximum = 10 * time.Second }
if maximum < minimum { maximum = minimum }
```

唯一调用点是 `manager.go:708`，传入 `cfg.ReconnectMin`/`ReconnectMax`，
而 `NewManager`（`manager.go:38-43`）已经把这两个值归一化为正数。
三个防御分支保护的是不可能到达的状态；`maximum < minimum` 连 `NewManager` 都没检查，
真要防也防不住配置错误。

循环 `for i := 0; i < attempt && delay < maximum/2; i++ { delay *= 2 }` 实际是
`min(minimum<<attempt, maximum)`，但 `< maximum/2` 的边界让人要算两遍才确认没有溢出。

**建议**：

```go
// reconnectDelay is minimum doubled per attempt, capped at maximum.
// NewManager guarantees both bounds are positive.
func reconnectDelay(minimum, maximum time.Duration, attempt int) time.Duration {
	delay := minimum
	for i := 0; i < attempt && delay < maximum; i++ {
		delay *= 2
	}
	return min(delay, maximum)
}
```

---

### A9（低）`Attachment` 的 nil 接收者检查与 `sync.Once` 都是多余的

**位置**：`attachment.go:105`、`134`、`158`、`182`、`192`（`if a == nil || a.session == nil`），
`attachment.go:26` + `195`（`once sync.Once`）。

`*Attachment` 的唯一产地是 `runtimeSession.attach`（`attachment.go:29-98`），
成功路径必然填好 `session`/`client`；失败路径返回 `nil, err`。调用方
`websocket.go:95-108` 检查了 error 才继续。所以 5 个方法各带一行
"防止别人拿 nil 接收者调用"的检查，防的是不存在的情形。

`Close` 的 `sync.Once` 同样多余：`detach`（`attachment.go:199-205`）开头就有
`if client != expected { return }`，第二次 Close 时 `s.clients[clientID]` 已被删除，
天然幂等。

**建议**：删掉 nil 检查和 `once`；如果担心零值 `Attachment{}` 被误用，
把类型改成只能通过 `attach` 获得即可（现在已经是了）。
另外 `Resize`（`attachment.go:130-136`）把参数校验放在接收者检查**之前**，
顺序也反了——即使保留检查，也应先判接收者。

---

### A10（低）`backend` 接口里的 `io.Writer` 没有任何调用方

**位置**：`backend.go:19-28`（接口），实现见 `local.go:412-414`、`ssh.go:553-555`、
`lifecycle_test.go:67-69`。

`grep -rn "b\.Write(\|backend\.Write("` 显示：三个 `Write(p []byte)` 实现
**没有任何调用者**。生产路径 `websocket.go:249 → Attachment.WriteContext →
b.WriteContext` 完全绕开它；测试里的 `attachment.Write` 也只是转调
`WriteContext(context.Background(), ...)`（`attachment.go:100-102`）。

也就是说，`io.Writer` 的嵌入迫使每个 backend 实现一个死方法，
只为让接口"看起来像标准库"。

**建议**：

```go
type backend interface {
	io.Reader // consume() 用
	WriteContext(context.Context, []byte) (int, error)
	Resize(cols, rows uint16) error
	Wait(context.Context) error
	Close() error
	Terminate(context.Context) error
	Reconnectable(error) bool
}
```

三个 `Write` 方法与 `Attachment.Write` 一并删除，测试改用 `WriteContext`。

---

### A11（低）"不该走到这里"的守卫铺了四层

`killBackend`（`manager.go:910-923`）已经保证：只有 `resolved` 是
`""`/`none`/`auto` 时才调用 `b.Terminate`，否则走独立控制连接。可是下游还有三层同样的判断：

- `launcher.terminate`：`backend.go:79-81` —— `resolved == "" || resolved == PersistenceAuto` 时返回 nil
- `terminateLocal`：`local.go:248-249` —— `case PersistenceNone: return nil`
- `terminateSSH`：`ssh.go:500-502` —— `if resolved == PersistenceNone { return nil }`

再加上 `localBackend.Terminate`（`local.go:458-461`）和 `sshBackend.Terminate`（`ssh.go:598-601`）
里那两条 `if b.kind != PersistenceNone { return errors.New("must be terminated through ...") }`
——按上面的调用约束，这两条错误分支**永远不可能返回**（`s.resolved` 在
`activateBackend`（`manager.go:505`）里只会被赋成 launcher 返回的 tmux/screen/none）。

**建议**：把判断留在 `killBackend` 一处（那里也是唯一读得懂上下文的地方），
下游三层直接删。两条不可达的错误分支如果想保留成断言，至少改成
`panic("unreachable: ...")` 或加注释说明它是断言而非运行时分支——现在它伪装成
可返回的错误，读者会去找触发路径。

---

### A12（低）`stopTimer` 是 Go 1.23 之前的遗留写法

**位置**：`manager.go:725-729`，调用于 `manager.go:711`、`714`、`717`。

```go
func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		<-timer.C
	}
}
```

`go.mod` 声明 `go 1.26.0`。自 Go 1.23 起 `time.Timer` 的 channel 变为无缓冲，
未被引用的 timer 可直接回收，不再需要 drain；这个 helper 在
"timer 已触发且值已被 `case <-timer.C` 取走"的路径上甚至会永久阻塞（当前代码恰好
没有这样调用，但这正是这类 helper 危险的地方）。

**建议**：删掉 helper，改成 `timer := time.NewTimer(...); defer timer.Stop()`，
select 里三个分支直接 `return`/`continue`。

同一时期的遗留还有 `CloseContext`（`manager.go:283-289`）里显式传循环变量：

```go
for i, s := range sessions {
	wg.Add(1)
	go func(index int, session *runtimeSession) { ... }(i, s)
}
```

Go 1.22 起循环变量已按迭代作用域化，可以直接 `go func() { errs[i] = s.shutdown(ctx) }()`。

---

### A13（中）测试脚手架：死字段、重复 helper、为不支持的平台写的跳过

**位置**：`lifecycle_test.go`（1194 行）、`manager_test.go`（521 行）。

1. **死字段**：`sizeRecordingBackend.mu` / `.sizes`（`lifecycle_test.go:410-411`）
   在 `Resize` 里被写入（`:429-431`），但**没有任何读取点**——
   唯一的使用者 `TestResizeDuringConnectingIsAcceptedAndReconciled`（`:439-482`）
   只读 `recorder.resized` channel。这个 stub 应该退化成一个只有 channel 的类型：

   ```go
   type sizeRecordingBackend struct {
   	*backendStub
   	resized chan [2]uint16
   }
   ```

2. **四个 stub 变体**：`blockingResizeBackend`（`:323`）、`sizeRecordingBackend`（`:408`）、
   `closeBlockedResizeBackend`（`:415`）、`finiteReadBackend`（`:535`）都只覆写 `Resize`
   或 `Read`。三个 Resize 变体可以合并成一个带回调的 stub：

   ```go
   type scriptedBackend struct {
   	*backendStub
   	onResize func(cols, rows uint16) error
   }
   func (b *scriptedBackend) Resize(c, r uint16) error {
   	if b.onResize != nil { return b.onResize(c, r) }
   	return nil
   }
   ```

3. **重复 helper**：`waitRunning`（`manager_test.go:503-521`）与
   `waitState`（`lifecycle_test.go:1123-1141`）是同一个轮询循环，
   前者等价于 `waitState(t, ctx, m, id, StateRunning)`（只有 ticker 5ms vs 1ms 之差）。
   另外 `screenSessionExists`（`manager_test.go:475-478`）重新实现了生产代码
   `localScreenExists`（`local.go:224-229`）的判定逻辑，两者的匹配串必须保持一致。
   建议只留 `waitState`，并把测试 helper 集中到 `helpers_test.go`——
   现在 `waitRunning`/`waitForOutput` 在 `manager_test.go`、
   `waitState`/`waitCondition`/`await*` 在 `lifecycle_test.go`、
   `waitForFileContent` 在 `local_test.go`，跨文件互相引用。

4. **Windows 跳过**：`internal/terminal` 有 11 处 `runtime.GOOS == "windows"` 跳过
   （`manager_test.go:44/51/113/157/200/342`、`local_test.go:99/160/215/271`、`script_test.go:32`）。
   但 CI 只有 `ubuntu-latest`（`.github/workflows/ci.yml:12`），而包本身依赖
   `creack/pty`、`syscall.EIO`、`/bin/sh`、tmux/screen——Windows 上不可能工作。
   连带 `directShell()`（`manager_test.go:43-48`）的 `cmd.exe` 分支也是死代码：
   它的三个调用方都先跳过了 Windows。
   建议：整个包用一个 `//go:build unix` 或在 `TestMain` 里统一跳过，
   删掉 11 处逐测试的判断和 `directShell`。

5. **单函数塞多场景**：`TestDiscardWorksInAnyStateAndNeverKillsTheBackend`
   （`lifecycle_test.go:790-844`）在函数中段直接改写
   `manager.launcher = staticLauncher(...)`（`:827`）来开第二个场景。
   这既绕过了构造函数，又在其他 goroutine 可能读 `m.launcher`
   （`manager.go:434`）时无同步地写它。同一份文件里
   `TestReconnectWakesPermanentErrorAndTimedBackoff`（`:727`）
   和 `TestAttachmentCloseReasonsAndWriterNotifications`（`:875`）
   已经用了 `t.Run` 子测试，风格应统一：拆成两个子测试，各自 `managerWithLauncher`。

---

## B. 代码风格

### B1（中）local 与 ssh 把同一份配置数据抄了两遍

**screenrc 内容**：
- `local.go:345`：`[]byte("startup_message off\nhardstatus off\ncaption splitonly\nvbell off\nescape ^^^\n")`
- `ssh.go:476-482`：`configLines := []string{"startup_message off", "hardstatus off", "caption splitonly", "vbell off", "escape ^^^"}`

**tmux 服务器选项**：
- `local.go:141-146`：`{"set-option","-g","status","off"}` / `prefix None` / `prefix2 None` / `mouse on`
- `ssh.go:411-414`：`tmux+" set-option -g status off"` / `prefix None` / `prefix2 None` / `mouse on`

外加两边各自的 `terminal-features`/`terminal-overrides` 追加逻辑
（`local.go:156-159` 与 `ssh.go:416-417`）。这些是**数据**，不是逻辑，
两边的执行机制不同（exec.Command vs 拼 shell）可以理解，但数据本身不该抄。
`local_test.go:84` 与 `ssh_test.go:155-156` 各自断言其中几行，说明维护者已经
意识到它们必须同步。

**建议**：在 `backend.go` 里放一份共享数据，两条路径各自渲染。

```go
// backend.go
var screenConfigLines = []string{
	"startup_message off", "hardstatus off", "caption splitonly", "vbell off", "escape ^^^",
}

// tmuxServerOptions are the isolated server's fixed settings.
var tmuxServerOptions = [][2]string{
	{"status", "off"}, {"prefix", "None"}, {"prefix2", "None"}, {"mouse", "on"},
}
```

`local.go` 用 `strings.Join(screenConfigLines, "\n")+"\n"`，
`ssh.go` 用现有的 `shellQuote` 逐行拼；tmux 选项同理各自展开成 argv / shell 片段。

---

### B2（中）`startSSH` 的错误清理重复六遍，且 `ctx.Err()` 用了两种写法

**位置**：`ssh.go:39-151`。

六个失败点各写一遍"关掉刚建好的东西 + 判断是不是 ctx 取消 + 包装错误"：
`:56-63`、`:64-67`、`:73-80`、`:87-94`、`:95-103`、`:110-124`。
而且同一函数里两种惯用法交替出现：

```go
if ctxErr := ctx.Err(); ctxErr != nil { return nil, "", ctxErr }   // :59, :76, :99
if ctx.Err() != nil { return nil, "", ctx.Err() }                  // :64, :90, :120
```

再叠加 `stopSetupCancellation` + `setupComplete` 布尔 + 一个 defer
（`:44-50`），控制流需要来回跳三次才能确认哪条路径会关掉 client。

**建议**：用命名返回值 + 单个 defer 收敛清理，并统一 ctx 写法。

```go
func (l launcher) startSSH(ctx context.Context, spec SessionSpec, requested Persistence, create bool) (b backend, resolved Persistence, err error) {
	client, closers, err := dialSSH(ctx, *spec.Host)
	if err != nil {
		return nil, "", err
	}
	stopCancel := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer func() {
		if err != nil {
			stopCancel()
			_ = client.Close()
			closeAll(closers)
			if ctxErr := ctx.Err(); ctxErr != nil {
				err = ctxErr
			}
		}
	}()
	...
}
```

六处 `cleanup()` + 六处 ctx 判断塌缩成一处，函数从 113 行降到约 70 行。

---

### B3（中）`run()` 的控制流：布尔返回值语义不明 + `*int` 出参 + 重复的重连判定

**位置**：`manager.go:411-492`（run）、`:542-570`（handleStartError）、
`:603-620`（waitIfTerminating）、`:690-723`（waitForRetry）。

三个 helper 都返回 `bool`，但含义各不相同且都不是名字的字面意思：

| 函数 | 名字暗示 | 实际返回 |
|---|---|---|
| `waitIfTerminating() bool` | "是否在终止中" | true = **继续跑** |
| `waitForRetry(attempt) bool` | "是否等到了重试" | true = **可以重连** |
| `handleStartError(err, *int) bool` | "处理启动错误" | true = **再试一次** |

`handleStartError` 还通过 `attempt *int` 出参改写调用方的计数器（`:561`、`:568`），
而 run loop 自己另有三处 `attempt++` / `attempt = 0`（`:446`、`:455`、`:484`）。
读者必须同时跟踪一个指针和三个赋值点才能知道退避计数在哪一步被重置。

同时，run loop 里有两段几乎相同的"重连还是收尾"判定：

```go
// :447-462（激活失败）
if b.Reconnectable(resizeErr) {
	s.setState(StateDisconnected, resizeErr)
	if s.waitForRetry(attempt) { attempt++; continue }
	return
}
s.finishExited(resizeErr)
return

// :481-490（读/等待失败）
if !errors.Is(backendErr, ErrBackendMissing) && b.Reconnectable(backendErr) {
	s.setState(StateDisconnected, backendErr)
	if s.waitForRetry(attempt) { attempt++; continue }
	return
}
s.finishExited(backendErr)
return
```

（顺带一提，第二段多出的 `!errors.Is(..., ErrBackendMissing)` 与
`sshBackend.Reconnectable`（`ssh.go:613`）里已有的同一判断重复；
local 侧根本产生不了这个错误。）

**建议**：把 helper 改名为肯定式谓词，把 `attempt` 提为 run loop 的局部状态或
一个小结构体，并抽出共用的判定：

```go
func (s *runtimeSession) keepRunning() bool  // 原 waitIfTerminating
func (s *runtimeSession) awaitRetry(attempt int) bool

// retryOrFinish 返回 true 表示 run loop 应当再连一次。
func (s *runtimeSession) retryOrFinish(b backend, err error, attempt *int) bool {
	if b.Reconnectable(err) {
		s.setState(StateDisconnected, err)
		if s.awaitRetry(*attempt) {
			*attempt++
			return true
		}
		return false
	}
	s.finishExited(err)
	return false
}
```

---

### B4（中）`configPath()` 靠反解析 argv 找回上一行才构造过的变量

**位置**：`local.go:231-280`。

```go
case PersistenceScreen:
	config, screenEnv, err := l.screenRuntime(spec.Env)   // :242 —— config 在 case 作用域内
	...
	args = []string{"-c", config, "-S", name, "-X", "quit"}
...
if resolved == PersistenceScreen {
	return waitLocalScreenAbsent(ctx, path, configPath(args), env, name)  // :270
}

func configPath(args []string) string {   // :275-280
	if len(args) >= 2 && args[0] == "-c" { return args[1] }
	return ""
}
```

`configPath` 存在的唯一原因是 `config` 被声明在 `switch` 的 case 块里、
出了块就看不见了，于是写了个函数从刚拼好的 argv 里再抠出来——
一旦以后 args 的顺序变了，它会静默返回 `""`。

**建议**：把 `config` 提到函数作用域，删掉 `configPath`。

```go
func (l launcher) terminateLocal(ctx context.Context, spec SessionSpec, resolved Persistence) error {
	name := backendName(spec.ID)
	var path, config string
	var args, env []string
	switch resolved {
	case PersistenceTmux:
		...
	case PersistenceScreen:
		var err error
		config, env, err = l.screenRuntime(spec.Env)
		if err != nil {
			return err
		}
		args = []string{"-c", config, "-S", name, "-X", "quit"}
	...
	}
	...
	if resolved == PersistenceScreen {
		return waitLocalScreenAbsent(ctx, path, config, env, name)
	}
	return nil
}
```

同一函数里还有 `_, path, _ = l.resolveLocal(PersistenceTmux)`（`:238`、`:241`）——
丢掉 error 之后再靠 `if path == ""` 重新造一条不同措辞的错误（`:253-255`：
`"terminal: %s is not installed"` vs `resolveLocal` 自己的
`"terminal: tmux was requested but is not installed"`）。两条信息说同一件事，应统一。

---

### B5（中）`runtimeSession` 三个互斥量没有说明各自保护什么

**位置**：`manager.go:345-362`。

```go
mu          sync.Mutex
operationMu sync.Mutex
sizeMu      sync.Mutex
state       SessionState
lastErr     string
...
created     bool
started     bool
terminating bool
closed      bool
```

三个锁并排声明，后面跟 14 个字段，没有任何注释交代边界。实际情况是：
`mu` 保护后面全部字段；`operationMu` 序列化 `stop`/`discard` 两个操作（不保护字段）；
`sizeMu` 序列化 `applySize` 对 backend 的调用（也不保护字段）。
而 `created` 其实是**构造后不可变**的，`run()`（`:414`）不持锁就读它——
放在这一组里会让人误以为它受 `mu` 保护。

**建议**：按 Go 惯例注明归属，并把不可变字段挪出去。

```go
type runtimeSession struct {
	// Immutable after construction.
	manager *Manager
	log     transcript.Log
	created bool
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	wake    chan struct{}
	retry   chan struct{}

	// operationMu serializes stop and discard; it guards no fields.
	operationMu sync.Mutex
	// sizeMu serializes Resize calls into the live backend; it guards no fields.
	sizeMu sync.Mutex

	// mu guards every field below.
	mu          sync.Mutex
	spec        SessionSpec
	state       SessionState
	...
}
```

（`spec` 目前在 `mu` 之前声明（`:337`），却在 `launchSpec`（`:596-599`）里被
持锁改写——这也是当前分组具有误导性的证据。）

---

### B6（低）"只保留最新值"的 channel 推送写了两遍

**位置**：`manager.go:777-791`（`notifyLocked`）与 `manager.go:831-844`（`notifyWritersLocked`）。

两处都是同一个 8 行嵌套 select：

```go
select {
case client.states <- status:
default:
	select {
	case <-client.states:
	default:
	}
	client.states <- status
}
```

**建议**：Go 泛型可用，抽一个 helper（注意在注释里写明"必须在持有 s.mu 时调用，
否则最后那次发送可能阻塞"）：

```go
// replaceLatest keeps only the newest value in a capacity-1 channel.
// The caller must hold s.mu so no other producer can refill the slot.
func replaceLatest[T any](ch chan T, value T) {
	select {
	case ch <- value:
	default:
		select {
		case <-ch:
		default:
		}
		ch <- value
	}
}
```

另外 `notifyLocked` 既做推送又返回 status 供锁外回调使用，名字没有体现返回值；
叫 `snapshotAndNotifyLocked` 或让它返回值出现在文档注释里会更清楚。

---

### B7（低）名字消毒逻辑重复，且两个不同的截断长度

**位置**：`backend.go:59-69`（`newLauncher`）与 `backend.go:154-165`（`backendName`）。

```go
// newLauncher
name := unsafeSessionName.ReplaceAllString(cfg.MuxName, "-")
name = strings.Trim(name, "-")
if name == "" { name = "wmux" }
if len(name) > 32 { name = name[:32] }

// backendName
name := unsafeSessionName.ReplaceAllString(id, "-")
name = strings.Trim(name, "-")
if name == "" { name = "session" }
if len(name) > 40 { name = name[:40] }
```

同一段四步逻辑，两份副本，两个魔法上限（32 / 40），两个不同的兜底值。
截断还是按字节切的，多字节字符会被切断成非法 UTF-8（虽然随后只用作
tmux socket / session 名，不致命）。

**建议**：

```go
const (
	maxMuxNameLen     = 32
	maxBackendNameLen = 40
)

func sanitizeName(value, fallback string, limit int) string {
	name := strings.Trim(unsafeSessionName.ReplaceAllString(value, "-"), "-")
	if name == "" {
		name = fallback
	}
	if len(name) > limit {
		name = name[:limit]
	}
	return name
}
```

---

### B8（低）手写的 `sortedKeys` 与标准库重复

**位置**：`backend.go:226-233`。

```go
func sortedKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for key := range env { keys = append(keys, key) }
	sort.Strings(keys)
	return keys
}
```

`go.mod` 是 `go 1.26.0`，文件里已经 import 了 `slices`（`backend.go:11`）：

```go
func sortedKeys(env map[string]string) []string { return slices.Sorted(maps.Keys(env)) }
```

改完可以去掉 `sort` 这个 import（全包仅此一处使用）。

同类的还有 `local.go:357-366` 的 `setEnvironment`——手写的 "KEY=VALUE 切片里覆盖某一项"，
只有 `screenRuntime`（`local.go:353`）一个调用方。既然 `localEnvironment(extra)`
本来就以 map 覆盖为语义（`local.go:382-397`），直接构造合并后的 map 更直接：

```go
values := maps.Clone(extra)
if values == nil { values = map[string]string{} }
values["SCREENDIR"] = dir
env := localEnvironment(values)
```

---

### B9（低）超时/重试常量散落且互不一致

同一件事（等 screen 会话出现或消失）在三处用了三组数字：

- `ensureLocalScreen`：2 秒上限 + 20ms 轮询（`local.go:206`、`:208`）
- `waitLocalScreenAbsent`：2 秒上限 + 20ms 轮询（`local.go:283`、`:285`）
- `remoteTerminateCommand`：40 次 × `sleep 0.05`（= 2 秒）（`ssh.go:536-537`）

另有裸数字：`terminalSize` 的 `80`/`24`（`local.go:399-408`）、
`consume` 的 `32<<10` 读缓冲（`manager.go:623`）、
`dialSSH` 的默认 10 秒（`ssh.go:174-176`）、
`keepAliveInterval` 的默认 15 秒（`ssh.go:640-642`）。

**建议**：至少把"等待多路复用器状态变化"的这组提成命名常量并让本地/远端共享：

```go
const (
	muxSettleTimeout  = 2 * time.Second
	muxPollInterval   = 50 * time.Millisecond
	defaultCols       = 80
	defaultRows       = 24
	backendReadBuffer = 32 << 10
)
```

远端脚本里的 `40` 就写成 `fmt.Sprint(int(muxSettleTimeout/muxPollInterval))`，
以后改一处即可。

---

### B10（低）命名问题若干

1. **接口/实现命名反了**：接口叫 `backendLauncher`（`backend.go:32`），
   唯一实现叫 `launcher`（`backend.go:37`）。Go 惯例是接口取简名、实现取描述性名
   （`Launcher` / `execLauncher`）。同名家族里还有 `Config.launcher` 字段、
   `newLauncher` 构造函数和 `NewManager` 里的局部变量 `selectedLauncher`（`manager.go:47`），
   四个 "launcher" 指三样东西。

2. **`sshBackend.output` / `sshBackend.pipe`**（`ssh.go:25-26`）是同一个
   `io.Pipe` 的两端，名字却一个按用途、一个按类型。建议 `stdout *io.PipeReader` /
   `stdoutWriter *io.PipeWriter`。

3. **`started` 有两个含义**（`manager.go:359`）：run loop 已启动，
   或 `markDormant`（`:399-409`）已把 `done` 关闭。三处 teardown 靠它决定
   要不要等 `done`（`:948`、`:994`、`:1021`），实际语义是"`done` 会被关闭"。
   叫 `hasRunLoop` 或直接总是关闭 `done` 并删掉这个标志更清楚。

4. **`resolveLocal` 的 `find` 闭包返回值不一致**（`backend.go:92-101`）：
   显式路径返回**用户给的原字符串**，回退路径返回 `exec.LookPath` 的**绝对路径**。
   同一个返回值有两种含义，调用方 `startLocal`（`local.go:36`）直接把它当命令名用。
   应统一返回 `LookPath` 的结果。

5. **`launcher` 是值类型却持有 `*sync.Mutex`**（`backend.go:42`，
   `newLauncher` 里 `screenMu: &sync.Mutex{}`），且 `screenRuntime`
   还要 `if l.screenMu != nil` 兜底（`local.go:325`）——而 `launcher{}` 字面量
   全仓库只在 `newLauncher`（`backend.go:68`）里出现一次，兜底判断永不成立。
   改成 `*launcher` + 内嵌 `sync.Mutex` 值，nil 检查可以删掉。

6. **`Manager.callbacks` 与 `Manager.cfg.Callbacks` 是同一个值**
   （`manager.go:18` 与 `:54`，`NewManager` 在 `:29-31` 已把 nil 归一化）。
   `m.cfg.Callbacks` 之后再无读者，`m.callbacks` 只是别名字段。留一个即可。

7. **`Persistence("")` 显式转换**（`manager.go:81`）写成
   `var resolved Persistence` 更直白。**`make([]OutputFrame, 0)`**
   （`attachment.go:51`）按 Go Code Review Comments 应写 `var initial []OutputFrame`
   （`websocket.go:133` 只做 range，nil 切片没问题）。

---

### B11（低）测试组织

1. **helper 的 `ctx` 参数位置**：`waitRunning(t, ctx, ...)`、`waitState`、
   `waitCondition`、`waitForOutput`、`awaitCloseReason`、`awaitWriter`、
   `awaitState`、`waitForFileContent` 共 8 个 helper 都把 `context.Context`
   放在第二位。上一轮 review 的 L11 已经因为同样的理由指出过 `issueLogin(w, ctx)`，
   这里也会被 `revive: context-as-argument` 报出来。测试 helper 里 `t` 在前是常见做法，
   但既然仓库准备统一，就应该一次改齐（或在 lint 配置里显式豁免 `_test.go`）。

2. **错误处理不一致**：同一文件里既有
   `if _, err := manager.Create(...); err != nil { t.Fatal(err) }`，
   也有 `_, _ = manager.Create(...)`（`lifecycle_test.go:881`、`899`、`918`、
   `951`、`999`、`1021`）。建议统一检查——被忽略的那几处一旦失败，
   测试会以更难懂的方式在后面炸掉。

3. **类型与方法交错**：`lifecycle_test.go:408-437` 依次是
   `sizeRecordingBackend` 结构体（408）、`closeBlockedResizeBackend` 结构体（415）、
   `closeBlockedResizeBackend.Resize`（421）、`sizeRecordingBackend.Resize`（427）。
   两个类型的声明和方法互相穿插，应各自聚拢。

4. **残留的 sleep**：`lifecycle_test.go:628`（40ms）与 `:712`（30ms）
   还是"睡一会儿确认没有多余重试"的负向断言。上一轮的处理记录说
   "测试中的 sleep 排序改为通道同步"，这两处属于确实难以用 channel 表达的负向检查，
   保留可以接受，但建议在注释里写明"这是负向断言，缩短会降低灵敏度而非引入 flake"，
   免得后人当成漏网之鱼去删。

---

## C. 看起来复杂、但不建议改的地方

以下都核对过设计意图（`docs/architecture.md`）或并发正确性，**不属于过度工程**：

1. **`runtimeSession.sizeMu` 与 `applySize`**（`manager.go:520-539`）：
   保证"较老的尺寸不会覆盖较新的尺寸"，`TestNewerSizeIsNeverOverwrittenByAnOlderOne`
   （`lifecycle_test.go:351`）固化了这一点。三个锁看起来多，但 `sizeMu` 与 `mu`
   保护的东西不同，合并会在 Resize 阻塞时挡住整个会话。

2. **`operationMu`**（`manager.go:346`）：串行化 stop/discard，
   与 API 层 `s.sessionOps.lock(id)` 是两个层次的保护（后者只覆盖 HTTP 请求，
   前者还要覆盖 `Manager.Close` 等非 HTTP 触发的路径）。

3. **`backendLauncher` 接口 + `Config.launcher` 非导出字段**：
   `lifecycle_test.go` 的 20 多个生命周期用例全靠它才能在不启动真实
   tmux/SSH 的前提下覆盖重连、终止、代际、尺寸等场景。虽然是"只有一个生产实现的接口"，
   但它换来的测试隔离是实打实的，不建议删。

4. **`create bool` 在 launcher 接口里**：architecture.md 明确规定
   "Only the first start of a freshly created generation may create a multiplexer session"。
   这个参数是该不变量的载体，不是多余开关。

5. **`launcher.terminate` 走独立控制连接**（`manager.go:915-918`）：
   architecture.md 说明失败时要保住数据连接，`TestTerminateFailureLeavesRunLoopAndAttachmentAlive`
   固化了它。看起来"为什么不直接杀现有连接"，但这是有意的。

6. **`context.AfterFunc` 三处用于握手/建立期的取消**
   （`ssh.go:44`、`:198`、`:507`、`:602`）：Go SSH 库本身不接受 context，
   这是把 ctx 接到 `Close()` 上的标准做法，不是过度设计。

7. **`subscriber` 的四个 channel（frames / writers / states / closed）**：
   分别对应 architecture.md 里 WebSocket 协议的四类消息，
   合并成一个事件流反而要在 `websocket.go` 里再拆一次。

8. **`permanentError` 包装 + `permanentStartError` 的幂等判断**
   （`backend.go:129-143`）：run loop 靠它区分"等重连"和"等用户操作"
   （`manager.go:553-562`），是必要的错误分类，不是为了好看。

9. **`transcript` 回放在 `s.mu` 内完成**（`attachment.go:33-59`）：
   这正是 `websocket.go:139-141` 注释所依赖的 replay/live 精确分界。
   上一轮 review 的 M7 已经把"移出锁"列为待办，但那是性能取舍，
   不能当成过度工程删掉。

10. **`runtimeSession` 把 `context.Context` 存进结构体**（`manager.go:339`）：
    虽然与 "Do not store Contexts inside a struct type" 相抵，
    但这是长生命周期后台任务的公认例外（ctx 表示的是 session 生命周期而非请求），
    且 `cancel` 与 `done` 配套，改成裸 channel 反而要在 4 个地方手工转换。

---

## 建议的处理顺序

1. **A2 + A3**（删死代码/死字段）：纯删除，无行为变化，先做能让后面的重构少读一半代码。
2. **A1**（拆 `manager.go` + 合并三份 teardown）：本包最大的一处收益。
3. **B1 + B4 + B2**（消除 local/ssh 数据重复、`configPath`、`startSSH` 清理）：局部、独立。
4. **A5 + A8 + A12 + B8**（去掉无效 signal、防御默认值、Go 1.23 前遗留写法、手写工具函数）。
5. **A13 + B11**（测试脚手架瘦身与组织），可与 3、4 并行。
6. **A4 / A6 / A7**（改签名与结构体形状）会触及调用方，放最后一起做一次。
7. **B5 / B10** 属于注释与命名，随手跟着上面的改动一起收尾。
