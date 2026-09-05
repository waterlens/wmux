# wmux 代码审查：过度工程与代码风格

**范围**：`internal/store`、`internal/transcript`、`internal/security`、`internal/config`、`internal/sshx`、`internal/sshconfig`、`internal/version`（含全部 `*_test.go`，共约 5 700 行）
**分支**：`main` @ `4fb3e9f`（干净）
**只关注**：A 过度工程、B 代码风格。正确性/安全/性能不在范围内（已由 `docs/reviews/2026-09-04-independent-code-review.md` 覆盖，本文不重复其结论）。
**方法**：逐文件通读全部内容；每条"无调用方 / 死字段 / 重复"结论均以 `grep -rn` 全仓库（含 `client/`、`*_test.go`）核实；辅助运行 `go vet ./...`（通过）、`gofmt -l`（无输出）、`GOOS=windows go build ./...`（通过）。未修改任何文件。

---

## 总览

| 严重程度 | 数量 | 主要问题 |
| --- | --- | --- |
| 高 | 3 | 一整列冗余数据贯穿 5 层；一条无法阅读的 13 参数 SQL；security 导出面比实际需要大 3 倍 |
| 中 | 11 | 死字段/死方法、store CRUD 样板重复 10 处、三层重复校验、transcript 与 sshconfig 内部重复、跨包 SSH 逻辑重复 |
| 低 | 6 | 不可触发的防御分支、可用标准库替代的手写工具、测试小问题 |

---

## 高

### 1. `backend_name` 是一整列纯冗余数据，贯穿数据库 → store → API → 前端（过度工程）

**位置**
- `internal/store/migrations.go:53`（列定义 `backend_name TEXT NOT NULL DEFAULT ''`）
- `internal/store/models.go:72`（`BackendName string`）
- `internal/store/sessions.go:14,38,49,218,226,242,365`
- `internal/app/runtime_repository.go:115`（写入方）
- `internal/api/sessions.go:272`（读出后立刻抹掉）
- `client/src/api.ts:57`、`client/src/types.ts:42`（前端 schema 里的字段）

**问题**：写入这一列的唯一代码是

```go
// runtime_repository.go:113-116
if status.Persistence != "" && status.Persistence != terminal.PersistenceAuto {
    backend = string(status.Persistence)
    backendName = terminal.BackendName(status.ID)   // 由 session ID 确定性推导
}
```

而 `terminal.BackendName`（`internal/terminal/backend.go:167-168`）是 session ID 的纯函数。服务端没有任何一处再读取 `Session.BackendName` 做判断（`runtime_repository.go:40` 恢复用的是 `session.Backend`，不是 `BackendName`），而 `api/sessions.go:272` 在返回给浏览器之前又把它清空。前端 schema 里因此永远只能看到空字符串。

**为什么是问题**：一个数据库列、一个模型字段、一个函数参数、一条前端 schema 字段，承载的信息量为零——它可以由已有的 session ID 在需要的地方现算。它还把 `UpdateSessionRuntime` 撑成 6 参数，并直接导致了下面第 2 条那条不可读的 SQL。

**建议**：删掉这一列和字段（新增 migration 3 `ALTER TABLE sessions DROP COLUMN backend_name`），`UpdateSessionRuntime` 变成 5 参数：

```go
func (s *Store) UpdateSessionRuntime(ctx context.Context, id string, generation int, status, backend string, sessionError *string) error
```

任何需要 tmux/screen 会话名的地方直接调用 `terminal.BackendName(id)`。同时删掉 `client/src/api.ts:57`、`types.ts:42`。

---

### 2. `UpdateSessionRuntime` 的 13 占位符 SQL 无法阅读，且其中一半只为"避免一次冗余写"（风格 + 过度工程）

**位置**：`internal/store/sessions.go:216-249`

```go
UPDATE sessions
SET status = ?,
    backend = CASE WHEN ? = '' THEN backend ELSE ? END,
    backend_name = CASE WHEN ? = '' THEN backend_name ELSE ? END,
    last_error = ?
WHERE id = ? AND generation = ? AND (
    status IS NOT ?
    OR (? <> '' AND backend IS NOT ?)
    OR (? <> '' AND backend_name IS NOT ?)
    OR last_error IS NOT ?
)
```

实参顺序是 `status, backend, backend, backend, backendName, err, id, generation, status, backend, backend, backend, backendName, err`。

**问题一（可读性/疑似笔误）**：第 226 行 `backend_name = CASE WHEN ? = '' THEN backend_name ELSE ? END` 的守卫参数是 **`backend`**（第 241 行），不是 `backendName`；第 231 行同理。也就是说 `backend_name` 的更新条件挂在另一个变量上。行为上目前是对的（唯一调用方 `runtime_repository.go:113-116` 保证两者同时为空或同时非空），但任何读者都会先把它当成复制粘贴 bug，必须翻到另一个包才能确认。这是典型的"隐式契约写在别的包里"。

**问题二（过度工程）**：整个 `AND ( status IS NOT ? OR ... )` 子句（6 个占位符）唯一的作用是"状态没变化时不要写行"。但这条语句根本不更新 `updated_at`，一次冗余写没有任何可观察后果；而 `OnSessionState` 只在状态迁移时被调用（`manager.go:408,646,684,750,873,900,941,971`，没有定时器路径），所以它避免的是几乎不会发生的事。代价是：这 6 个占位符 + `requireSession`（见第 6 条）多出的一次 `SELECT`。

**建议**（配合第 1 条删掉 `backendName` 后）：

```go
result, err := s.db.ExecContext(ctx, `
UPDATE sessions
SET status = ?,
    backend = COALESCE(NULLIF(?, ''), backend),
    last_error = ?
WHERE id = ? AND generation = ?`,
    status, backend, nullableString(sessionError), id, generation)
```

行为等价：generation 不匹配 → 0 行 → `requireSession` 查到行存在 → 返回 nil（现有 `TestSessionGenerationIsolatesRestartsFromLateRuntimeCallbacks` 仍然通过）；幂等更新 → 1 行 → 返回 nil（`store_test.go:357` 的幂等断言仍然通过）。占位符从 14 个降到 5 个。

---

### 3. `internal/security` 导出了 6 个加解密函数，生产代码只用其中 2 个；AAD 是无人使用的过早泛化（过度工程）

**位置**：`internal/security/security.go:138-182`（`Encrypt`/`EncryptWithAAD`/`Decrypt`/`DecryptWithAAD`）、`:199-218`（`EncryptJSON`/`DecryptJSON`）

**核实结果**（`grep -rn` 全仓库，排除 `internal/security/` 自身）：

| 符号 | 包外引用 | 生产代码引用 |
| --- | --- | --- |
| `Encrypt` | 0 | 0 |
| `Decrypt` | 0 | 0 |
| `EncryptWithAAD` | 0 | 0 |
| `DecryptWithAAD` | 0 | 0 |
| `EncryptJSON` | 4 | 2（`api/hosts.go:244`、测试） |
| `DecryptJSON` | 4 | 2（`api/hosts.go:252`、`app/runtime_repository.go:70`） |

AAD 参数在整个仓库中从来没有传过非 `nil` 值——唯一传非 nil 的地方是 `security_test.go:84-99`，即测试在测试一个只有它自己使用的能力。

**为什么是问题**：一个 316 行的安全包，公开 API 面是实际需求的 3 倍。每个导出的加解密入口都是未来必须维护、必须审计、可能被误用（比如有人用 `Encrypt` 而不是 `EncryptJSON`，绕过 JSON 契约）的表面积。AAD 是典型的"看起来严谨"的过早泛化：wmux 只有一种密文（host 凭据 BLOB），且没有需要绑定的上下文。

**建议**：把四个函数收敛成两个未导出的实现，只导出 JSON 两件套：

```go
func seal(key, plaintext []byte) ([]byte, error)     // 原 EncryptWithAAD，去掉 aad 参数
func open(key, ciphertext []byte) ([]byte, error)    // 原 DecryptWithAAD，去掉 aad 参数

func EncryptJSON(key []byte, v any) ([]byte, error)
func DecryptJSON(key, ciphertext []byte, v any) error
```

`TestEncryptDecryptAndAuthentication` 相应改为对 `EncryptJSON` 做篡改检测。真到需要 AAD 那天再加，那时会知道 AAD 应该绑定什么。

---

## 中

### 4. `exit_code` 列 / `Session.ExitCode` 字段从未被写入（过度工程 / 死代码）

**位置**：`internal/store/migrations.go:60`、`models.go:80`、`sessions.go:16,39,57,257,353,373,391-394`、`client/src/api.ts:65`、`client/src/types.ts:50`

**核实**：`grep -rn "ExitCode:" internal cmd` 无任何结果——没有任何调用方在构造 `store.Session` 时设置过 `ExitCode`；`UpdateSessionRuntime` 不写 `exit_code`；`BeginSessionRestart`（`sessions.go:257`）只把它清成 `NULL`。也就是说这一列的值恒为 `NULL`，前端 schema 里的 `exitCode` 永远拿不到值。

顺带：`nullableInt`（`store.go:169-174`）在整个包里只有一个调用点（`sessions.go:57`），就是这个恒为 nil 的字段；`nullableMillis`（`store.go:155-160`）同理只有 `sessions.go:56` 一个调用点，而 `LastAttachedAt` 在创建时同样从未被设置（`grep -rn "LastAttachedAt:"` 无结果）。

**建议**：要么补上真正的退出码上报路径（`terminal.SessionStatus` 里有退出信息），要么删掉列 + 字段 + 前端 schema + `nullableInt`。当前状态是三层结构维护一个永远为空的值。

---

### 5. `Store.UpdateSession` 没有生产调用方（过度工程 / 死代码）

**位置**：`internal/store/sessions.go:104-150`（45 行，含最复杂的 `updated_at CASE` 逻辑）

**核实**：`grep -rn "UpdateSession(" ` 全仓库只有 `sessions.go:106`（定义）与 `store_test.go:289,412`（测试）。API 层改名走的是 `UpdateSessionName`（`api/sessions.go:126`），改尺寸走 `UpdateSessionSize`（`api/websocket.go:285`），没有任何地方需要"整行替换"。

**为什么是问题**：这是包里最难的一段 SQL（14 个占位符，`CASE WHEN name IS NOT ? OR kind IS NOT ? OR ...` 用来实现"只有产品字段变化才更新 updated_at"），它唯一的读者是为它写的两个测试。`TestStaleProductAndRuntimeUpdatesDoNotOverwriteEachOther`（`store_test.go:395-423`）证明的性质——"过期的产品更新不覆盖运行时字段"——在没有 `UpdateSession` 的世界里根本不存在。

**建议**：删除 `UpdateSession` 及其两个测试；`validateSession` 只保留 `CreateSession` 需要的部分。这一步能顺带减掉约 70 行。

---

### 6. store 的 CRUD 样板重复 10 处（风格）

**位置**（`rowsChanged(result)` + `if err != nil { return fmt.Errorf("check ...") }` + `if !ok { return ErrNotFound }` 三段式）：
`user.go:42-48`、`user.go:101-107`、`auth_sessions.go:107-113`、`auth_sessions.go:122-128`、`hosts.go:117-123`、`hosts.go:135-141`、`hosts.go:154-160`、`sessions.go:142-148`、`sessions.go:157-164`、`sessions.go:270-276`

十处结构完全相同，只有中间那句错误文案不同：`"check auth session touch"`、`"check host update"`、`"check host fingerprint update"`、`"check session deletion"`……而且 `sessions.go:144` 与 `sessions.go:272` 用了**完全相同**的文案 `"check session update"`，分别来自 `UpdateSession` 和 `requireSession`，出错时无法区分来源。

另外，这些 `"check ..."` 分支包裹的是 `sql.Result.RowsAffected()` 的错误——`modernc.org/sqlite` 在 `Exec` 成功后不会让它失败，所以这十个错误分支实际上都是不可达代码。

**建议**：抽一个执行辅助函数，把十处压成一处：

```go
// execAffecting 执行一条应当命中恰好一行的语句，未命中时返回 ErrNotFound。
func (s *Store) execAffecting(ctx context.Context, action, query string, args ...any) error {
    result, err := s.db.ExecContext(ctx, query, args...)
    if err != nil {
        return fmt.Errorf("%s: %w", action, err)
    }
    if count, _ := result.RowsAffected(); count == 0 {
        return ErrNotFound
    }
    return nil
}
```

`UpdatePassword`、`TouchAuthSession`、`DeleteAuthSession`、`UpdateHostFingerprint`、`DeleteHost`、`DeleteSession` 都能直接收敛成一行调用。`Setup`（返回 `ErrAlreadySetup`）和 `requireSession`（"未变更但存在"语义）保留各自的特例。

---

### 7. 同一组约束被写了三遍：API normalize / store validate / SQLite CHECK（过度工程）

**位置**
- `internal/api/types.go:103-170`（`hostInput.normalize` / `sessionInput.normalize`）
- `internal/store/hosts.go:160-181`（`validateHost`）、`internal/store/sessions.go:306-348`（`validateSession`、`validSessionStatus`）
- `internal/store/migrations.go:30-56`（`CHECK (port BETWEEN 1 AND 65535)`、`CHECK (auth_type IN (...))`、`CHECK (kind IN ('local','ssh'))`、`CHECK (persistence IN (...))`、`CHECK (status IN (...))`、`CHECK (cols > 0)`、`CHECK (rows > 0)`）

端口范围、auth_type 取值、kind 取值、persistence 取值、status 取值、cols/rows 正数——这六组规则每一组都存在于三个地方。

**为什么是问题**：中间那一层（store 的 Go 校验）产生的 `ErrInvalidInput` **没有任何调用方区分**——`grep -rn "ErrInvalidInput" internal cmd`（排除 store 包自身）结果为 0；`api/hosts.go:291-297` 的 `handleStoreError` 只识别 `ErrNotFound`，其余一律 500 `"服务发生内部错误"`。也就是说：这层校验只在 API 层校验有 bug 时才会触发，而触发时用户看到的是 500 而不是有用信息。它同时也让新增一个 status 值需要改三个地方。

**建议**：保留 SQLite `CHECK`（防止手工改库把非法值写进来）和 API 的 `normalize`（负责用户可见文案），把 store 的 `validateHost`/`validateSession` 缩到只覆盖 CHECK **表达不了**的跨列规则：

```go
func validateSession(session Session) error {
    switch session.Kind {
    case SessionKindLocal:
        if session.HostID != nil {
            return fmt.Errorf("%w: local session cannot reference a host", ErrInvalidInput)
        }
    case SessionKindSSH:
        if session.HostID == nil || strings.TrimSpace(*session.HostID) == "" {
            return fmt.Errorf("%w: SSH session requires a host", ErrInvalidInput)
        }
    }
    return nil
}
```

（枚举值、端口、尺寸交给 CHECK；这样 `validSessionStatus` 也只剩 `UpdateSessionRuntime` 一个使用者。）

---

### 8. `transcript.inspectSegment` 里同一段"截断恢复"代码复制了 4 次（风格）

**位置**：`internal/transcript/store.go:177-183`、`:188-194`、`:198-204`、`:208-214`

四处完全相同：

```go
if recoverTail && readerAtEOF(reader) {
    if truncateErr := f.Truncate(validSize); truncateErr != nil {
        return segment{}, fmt.Errorf("transcript: recover %s: %w", path, truncateErr)
    }
    seg.size = validSize
    break
}
```

（第一处的条件是 `errors.Is(readErr, io.EOF)` 而不是 `readerAtEOF(reader)`，其余三处逐字相同。）加上四个不同的 `return segment{}, fmt.Errorf(...)` 分支，`inspectSegment` 的循环体有 55 行、嵌套 4 层。

**建议**：把恢复动作提成一个闭包，把"这行坏了"统一成一个判断：

```go
recover := func() (bool, error) {
    if !recoverTail || !readerAtEOF(reader) {
        return false, nil
    }
    if err := f.Truncate(validSize); err != nil {
        return false, fmt.Errorf("transcript: recover %s: %w", path, err)
    }
    seg.size = validSize
    return true, nil
}
```

循环体里每个校验点变成 `if bad { if done, err := recover(); ... }`，函数长度大约减半，恢复语义只写一遍。

---

### 9. `transcript.Config` 与 `transcript.DirectoryConfig` 是同一组字段的两份拷贝（风格）

**位置**：`internal/transcript/store.go:42-47` 与 `:427-432`

```go
type Config struct { Dir string; SegmentBytes int64; MaxBytes int64; SyncWrites bool }
type DirectoryConfig struct { Root string; SegmentBytes int64; MaxBytes int64; SyncWrites bool }
```

三个字段完全相同，只有 `Dir`/`Root` 名字不同，`Directory.Open`（`:454-459`）逐字段手工搬运。新增一个调节项要改三处。

**建议**：

```go
type Limits struct {
    SegmentBytes int64
    MaxBytes     int64
    SyncWrites   bool
}
type Config struct { Dir string; Limits }
type DirectoryConfig struct { Root string; Limits }
// Open: Config{Dir: ..., Limits: d.cfg.Limits}
```

补充：`SyncWrites` 在生产里被硬编码为 `false`（`cmd/wmux/main.go:87`），没有任何环境变量能打开它，只有测试传 `true`。它作为"测试用的可观察性开关"是合理的，但值得在注释里说明它不是运维旋钮，避免读者以为存在配置项。

---

### 10. sshconfig 为 `User` 实现了 8 个 %-token 展开，并因此把 `proxyJump` 一路穿到 `expandUser`（过度工程）

**位置**：`internal/sshconfig/sshconfig.go:383-427`（`expandUser`，45 行）、`:263`（调用点）、`:229-230`（`resolvedOptions.proxyJump/jumpSet` 因此被读两次）

对照 `internal/api/ssh_config.go` 实际用到的东西：`Candidate.Alias`、`Address`、`Port`、`Username`、`HasIdentityFile`、`Unsupported`——导入流程把 `Username` 原样塞进 `store.Host.Username` 就结束了。

而 `expandUser` 支持 `%d %h %i %j %l %L %n %p %u %%`，其中：
- `%j` 需要把 `options.proxyJump` 作为第 6 个参数传进来（`:263`、`:401-405`），但导入路径在 `Unsupported` 非空时已经直接 422 拒绝（`api/ssh_config.go:80-83`）——也就是说凡是 `ProxyJump` 有值的 host 根本走不到导入，`%j` 展开出的用户名永远不会被存下来；
- `%l`/`%L` 会为了拼一个用户名去调 `os.Hostname()`；
- `%d`（家目录）、`%i`（uid）展开进"SSH 用户名"在现实里没有意义。

**为什么是问题**：这 45 行 + 一个跨函数参数，服务的是 OpenSSH 文档里存在但实际配置中极罕见的写法，而且其中一个 token 的结果在本项目的数据流里不可能被使用。它还与同文件 `:387-389` 的设计原则（`User` 对 `${ENV}` 一律 fail-closed）不一致：环境变量 fail-closed，但八种 token 却全力支持。

**建议**：与 `${ENV}` 保持同一姿态，`User` 只接受字面值，遇到 `%`（`%%` 除外）就 fail-closed，理由与 `ProxyJump` 一样清晰可解释：

```go
func expandUser(value string) (string, error) {
    if strings.Contains(value, "${") {
        return "", errors.New("environment expansion is disabled for User")
    }
    return expandPercentTokens(value, func(token byte) (string, error) {
        return "", fmt.Errorf("token %%%c in User is not supported by discovery", token)
    })
}
```

这样 `expandUser` 从 6 参数降到 1 参数，`resolve` 里 `options.proxyJump` 的读取点从 3 处降到 1 处，删掉约 40 行。`TestUserExpandsSafeAccountAndTargetTokens` 改成断言"带 token 的 User 被明确拒绝"。（`expandHostName` 只支持 `%h`，规模合适，保留。）

---

### 11. `sshconfig.Discoverer` 接口是多余的：消费者已经自己声明了一份（过度工程 / 风格）

**位置**：`internal/sshconfig/sshconfig.go:38-54` vs `internal/api/server.go:44-47`

```go
// sshconfig 包里
type Discoverer interface {
    Discover(context.Context) (Result, error)
    Resolve(context.Context, string) (Candidate, error)
}
func New(path string) Discoverer { return &discoverer{path: path} }

// api 包里，一模一样又写了一遍
type sshConfigDiscoverer interface {
    Discover(context.Context) (sshconfig.Result, error)
    Resolve(context.Context, string) (sshconfig.Candidate, error)
}
```

`grep -rn "sshconfig.Discoverer"` 在包外结果为 0——测试用的 `fakeSSHConfigDiscoverer`（`api/ssh_config_test.go:25`）实现的是 api 自己那个接口。

**为什么是问题**：这正是 Go Code Review Comments 里 "return concrete types, accept interfaces" 说的反例：生产者定义接口、构造函数返回接口，导致 `*discoverer` 的具体类型对调用方不可见（想加一个方法就必须动接口），而真正的抽象点已经在消费者那边正确地存在了。

**建议**：删掉 `Discoverer`，`New` 返回 `*Discoverer`（把 `discoverer` 改名导出，或直接 `func New(path string) *discoverer` 不可行 → 建议 `type Discoverer struct{...}` + `func New(path string) *Discoverer`）。api 侧不需要任何改动。

顺带：`newWithHome`（`:56-60`）与 `processUsername`（`:173-179`）在非测试文件里，但 `grep` 显示两者只被 `sshconfig_test.go` 使用（`:48,110,152,516,539,596,687,700` 与 `:359,399,600`）。按 Go 惯例应移到 `export_test.go`，或让测试直接构造结构体、直接调 `runningAccount("")`。

---

### 12. `internal/sshx` 与 `internal/terminal` 各写了一份 SSH 认证与指纹校验；且 `Credentials.AgentSocket` 是死字段（过度工程 / 重复）

**位置**
- 认证方式构造：`internal/sshx/sshx.go:122-157`（`authMethod`）与 `internal/terminal/ssh.go:235-289`（`sshAuthContext`）——两者都实现 password / privateKey(+passphrase) / agent（含 `SSH_AUTH_SOCK` 回退）三条分支，连"passphrase 为空则用 `ParsePrivateKey`，否则 `ParsePrivateKeyWithPassphrase`"的细节都一样。
- 严格指纹回调：`internal/sshx/sshx.go:89-95` 与 `internal/terminal/ssh.go:217-229`。
- 认证类型字面量：`sshx.go:124,129,141` 用 `"password"`/`"privateKey"`/`"agent"` 字符串，而 `store/models.go:6-8` 已经导出了 `HostAuthPassword`/`HostAuthKey`/`HostAuthAgent` 常量（`api/types.go:124,128,132` 也重复了一遍字面量）。

**死字段**：`sshx.Credentials.AgentSocket`（`sshx.go:27`）全仓库只在自己的 `authMethod:142` 被读取，**没有任何调用方设置过它**（唯一构造点 `api/hosts.go:226-231` 只填 AuthType/Password/PrivateKey/Passphrase）。它是 `terminal.AgentCredential.Socket` 的一个从未接线的镜像。

**建议**
1. 删掉 `Credentials.AgentSocket`，`authMethod` 的 agent 分支直接读 `SSH_AUTH_SOCK`。
2. `sshx.Credentials.AuthType` 的 `switch` 改用 `store.HostAuth*` 常量（sshx 已经在被 api 用 `store.Host` 的字段填充，引入 store 依赖不增加耦合）；或者把三个常量提到一个更底层的位置，让 store / api / sshx / terminal 共用一处定义。
3. 中期：让 `sshx` 复用 `terminal` 已有的 `Credential` 接口与 `sshAuthContext`/`strictHostKeyCallback`，`sshx` 只保留 `Probe`（探测未信任 host key 这件事确实只有它需要）。当前两份实现如果哪天在 passphrase 或 agent 行为上分叉，用户会遇到"测试连接成功但会话连不上"这类难查的不一致。

---

### 13. `store.Options` 有一半是死配置；`OpenWithOptions` 只被自己的测试调用（过度工程）

**位置**：`internal/store/store.go:29-33`、`:47`、`:79-84`

```go
type Options struct {
    Now          func() time.Time
    MaxOpenConns int
}
```

`grep -rn "MaxOpenConns"` 全仓库只有 `store.go:32,79,83`——**没有任何调用方设置过它**，`maxConns` 永远走 `= 8` 的默认分支。`OpenWithOptions` 包外引用为 0（生产路径是 `cmd/wmux/main.go:72` 的 `store.Open`）。

**建议**：删掉 `MaxOpenConns`，直接 `db.SetMaxOpenConns(maxOpenConns)` 用一个包级常量；`Options` 只剩 `Now`，此时更自然的形态是把测试钩子做成一个未导出的构造：

```go
const maxOpenConns = 8

func Open(ctx context.Context, path string) (*Store, error) { return open(ctx, path, time.Now) }
// store_test.go: open(ctx, path, func() time.Time { return *now })
```

导出面从 `Open`+`OpenWithOptions`+`Options` 收敛成 `Open`。

---

### 14. `PutAuthSession` 被导出，但只有包内一个调用方；其文档注释描述的场景不存在（过度工程 / 过时注释）

**位置**：`internal/store/auth_sessions.go:21-63`

```go
// PutAuthSession persists a preconstructed session, generating missing ID and
// timestamps. It is primarily useful for imports and deterministic tests.
```

`grep -rn "PutAuthSession"` 全仓库只有 `auth_sessions.go:18`（`CreateAuthSession` 内部调用）与定义本身。既没有"imports"，测试也没用它（`store_test.go:170` 用的是 `CreateAuthSession`）。

**建议**：改成未导出的 `putAuthSession`，或者直接把它内联进 `CreateAuthSession`——后者能顺带删掉 `auth.CreatedAt.IsZero()` / `LastSeenAt.IsZero()` / `auth.ID == ""` 三个只在"预构造"场景才有意义的分支（`:29-42`），因为唯一调用方永远传空 ID 和零时间。同时修掉注释。

---

### 15. `config` 的锁分成三个文件，其中一套只服务于一个跑不起来的平台（过度工程）

**位置**：`internal/config/lock.go`（89 行）、`lock_unix.go`（36 行）、`lock_fallback.go`（25 行）、`lock.go:20`（`removeOnClose` 字段）

`lock_fallback.go` 的构建约束是 `!darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd`，注释说"keeps builds portable"。`GOOS=windows go build ./...` 确实通过，但那个二进制无法工作：`internal/terminal/local.go:87` 依赖 `creack/pty`，Windows 上直接返回不支持（包里的测试也用 `t.Skip("creack/pty direct test is Unix-only")` 承认了这点）。

**为什么是问题**：为一个跑不起来的平台维护了**语义不同**的第二套锁：`O_EXCL` 创建文件的方案在进程崩溃后会留下永久残留的锁文件、没有任何恢复路径，而 flock 版本天然随进程退出释放。`removeOnClose` 字段（`lock.go:20,83`）只为这条不可达路径存在，读 `lock.go` 的人必须先搞清楚它什么时候是 true。

**建议**：删掉 `lock_fallback.go` 和 `removeOnClose`，`lock_unix.go` 的构建约束换成 Go 1.17 起就有的 `//go:build unix`，文件合并回 `lock.go`：

```go
//go:build unix
```

三个文件 150 行变成一个文件约 100 行，语义只剩一种。如果确实想保留"能交叉编译到 Windows"这个性质，正确做法是给 `acquirePlatformLock` 一个返回 `errors.ErrUnsupported` 的存根，而不是提供一个语义更差的替代实现。

补充（低）：`DataDirLock.mu`（`lock.go:17`）保护的 `Close` 在生产里只有 `cmd/wmux/main.go:67` 的一个 `defer` 调用，测试里是顺序调用两次（`config_test.go:129-134`）。并发 `Close` 从未发生，`if l.file == nil { return nil }` 已足够，这个 mutex 可以去掉。

---

### 16. `store.Session` 同时充当数据库模型和 HTTP 响应体，而 `Host`/`User` 却有专门的 DTO（风格 / 分层）

**位置**：`internal/store/models.go:33-82`（`User`/`AuthSession`/`Host`/`Session` 全部字段带 `json:"..."` tag）、`internal/api/sessions.go:271-281`（`publicSession` 直接返回 `store.Session`）vs `internal/api/hosts.go:276-289`（`publicHost` 转成 `hostResponse`）、`internal/api/auth.go:222-224`（`publicUser` 转成 `map`）

四个模型里只有 `Session` 是原样序列化给浏览器的，另外三个都有转换层。后果：

- `store/models.go` 里的 `json:"-"`（`User.PasswordHash` 在 `models.go:35`、`AuthSession.TokenHash` 在 `:43`）是为了"万一被序列化"而存在的安全网，但这两个类型**从来没有**被序列化过；
- `Host` 的全套 json tag（`models.go:50-59`）完全没有使用者（响应走 `hostResponse`）；
- `Session` 的 API 契约实际上写在 store 包里——加一个数据库列就会自动出现在 HTTP 响应里，`api/sessions.go:272` 那句 `session.BackendName = ""` 就是这种耦合的补丁。

**建议**：统一到一侧。最小改动是给 session 也补一个 `sessionResponse` DTO（与 `hostResponse` 对称），然后删掉 `store/models.go` 里全部 json tag（`Credentials` 除外——它的 tag 是 `EncryptJSON` 的真实契约，`store.go` 注释也说明了这点）。这样"哪些字段对外可见"就集中在 `api/types.go` 一个文件里。

---

### 17. sshconfig 的取消检查过度（过度工程 + 魔法数字）

**位置**：`internal/sshconfig/sshconfig.go:69,98,111,136,288`、`parser.go:62,110,127,137,295`、`sshconfig.go:208-213`（`contextError`）

11 个取消检查点散布在一个"读几 KB 本地文件"的路径上，其中两处是：

```go
for index, directive := range file.directives {
    if index%64 == 0 {
        if err := contextError(ctx); err != nil {
            return err
        }
    }
```

`parser.go:294-299` 与 `sshconfig.go:287-292` 逐字重复，`64` 没有任何注释解释。此外 `contextError` 里的 `if ctx == nil` 分支（`:209-211`）永远不会触发——所有入口都来自 `r.Context()`（`api/ssh_config.go:21,71`）或测试的 `context.Background()`。

**为什么是问题**：`Discover` 的实际工作量是解析用户自己的 `~/.ssh/config`（通常几十行），成本在毫秒级。逐 64 条指令检查取消是为一个不存在的长任务准备的；重复两遍且带魔法数字，读者会误以为这里有性能风险。

**建议**：只在 I/O 边界保留取消检查——`Discover`/`Resolve` 入口各一次、`loadFile` 打开文件前一次（`parser.go:62`），共 3 处；删掉两处 `index%64` 循环和 `contextError` 的 nil 分支，直接用 `ctx.Err()`。`TestCanceledContextStopsDiscoverAndResolve` 仍然通过（它取消的是入口）。

---

### 18. 一批不可触发的防御分支与重复的默认值（过度工程）

**位置与说明**

1. `internal/store/sessions.go:298-303`：`if session.Cols == 0 { session.Cols = 120 }` / `Rows = 36`。唯一的创建入口 `api/sessions.go:56-57` 已经硬编码了同样的 `Cols: 120, Rows: 36`——同一对魔法数字在两个包里各写一遍，store 里的那份永远不会生效。建议：要么删掉 store 的默认值，要么删掉 api 的，把 120/36 变成一个有名字的常量放在唯一的一侧。
2. `internal/store/migrations.go:96-98`：`if next >= len(migrations) || migrations[next] == ""`。`currentSchemaVersion == 2` 且 `len(migrations) == 3`，循环上界就是 `currentSchemaVersion`，这个分支恒假。
3. `internal/store/store.go:121-123`：`func (s *Store) Close()` 里的 `if s == nil || s.db == nil`。`Open` 要么返回非 nil 且 db 非 nil，要么返回错误。
4. `internal/security/security.go:288`：
   ```go
   if uint64(r)*uint64(p) >= 1<<30 || logN >= strconv.IntSize-1 || uint64(1<<logN) > uint64(math.MaxInt)/(128*uint64(r)) {
   ```
   前一行（`:284`）已经把 `logN ∈ [10,20]`、`r ≤ 32`、`p ≤ 16` 卡死，因此 `r*p ≥ 2^30`（最大 512）和 `logN ≥ 31`（或 63）两个条件恒假；只有第三个条件在 32 位平台上还有意义。注释说"scrypt's memory/work parameters must also fit in int safely"，但实际上前一行的硬上界已经完成了这件事。建议：删掉前两个条件，第三个保留并把注释改成"32 位平台上 2^20 × 128 × 32 会溢出 int"。（顺带可以去掉 `math` 这个 import。）

这些单独看都很小，但合在一起说明了一种倾向：为不可能发生的情形写代码，读者需要逐个验证"这真的会发生吗"。

---

## 低

### 19. `transcript.Replay` 的文件关闭手工写了 5 次（风格）

**位置**：`internal/transcript/store.go:347-404`，`_ = f.Close()` 出现在 `:371, 378, 383, 389`，正常路径在 `:395`。

Go 惯用法是把单次迭代提成函数用 `defer`：

```go
for _, seg := range s.segments {
    if seg.last <= after { continue }
    done, err := replaySegment(seg.path, after, &remaining, limit, yield)  // 内部 defer f.Close()
    if err != nil { return err }
    if done { return nil }
}
```

现在的写法只要将来加一个 `return` 就会漏关文件描述符。

### 20. `transcript.Append` 的轮转判断条件过长（风格）

**位置**：`internal/transcript/store.go:254`

```go
if s.active == nil || (len(s.segments) != 0 && s.segments[len(s.segments)-1].size > 0 && s.segments[len(s.segments)-1].size+int64(len(line)) > s.cfg.SegmentBytes) {
```

单行 175 字符，`s.segments[len(s.segments)-1]` 出现两次。建议抽成 `func (s *Store) needsRotationLocked(recordSize int) bool`，函数名本身就替代了注释。

### 21. `sshx` 的若干小问题（风格）

- **错误文案语言不一致**：`sshx.go` 的 14 处错误消息是中文（`:46,66,68,76,92,113,117,126,138,147,151,155`），而同为"库层"的 `store`/`terminal`/`transcript`/`config`/`sshconfig` 全部是英文，中文只出现在 `internal/api`（面向用户的响应文案）。`sshx` 的错误实际上经由 `api/hosts.go:168,192,234` 的 `upstreamError` 包装后才对外，包装时已经提供了中文文案，所以 `sshx` 内部的中文只出现在服务端日志里，与其它库层日志混排。建议统一改成英文，与包注释（英文）保持一致。
- **`authMethod` 返回 6 次 `func() {}`**（`:126,128,138,140,147,151,155）**：清理函数只有 agent 分支真正需要。改成返回 `io.Closer`（nil 表示无需清理）或者让调用方 `if cleanup != nil`，能去掉 5 处噪音。
- **`captured` 哨兵错误从未被匹配**（`:51,58,62`）：命名和 `errors.New` 让人以为后面会 `errors.Is(handshakeErr, captured)`，但实际判断依据是 `fingerprint != ""`。建议直接在回调里 `return errors.New("host key captured")` 并加一行注释说明"返回非 nil 是为了在认证前中止握手"。
- **`subtle.ConstantTimeCompare` 用在公开值上**（`:91`）：指纹是要展示给用户的公开数据，常数时间比较没有安全收益，只是让代码看起来更"严谨"。`internal/api/hosts.go:195` 有同样的用法。（`internal/terminal/ssh.go:224` 同理，但那里至少加了长度预判。）

### 22. 手写工具函数可以换成标准库（风格）

- `internal/transcript/store.go:13,113` 和 `internal/sshconfig/parser.go:11,177` 都 `import "sort"` 只为了 `sort.Strings`。`go.mod` 是 `go 1.26.0`，应当用 `slices.Sort`。
- `internal/sshconfig/sshconfig_test.go:806-813` 的 `contains` 就是 `slices.Contains`。
- `internal/sshconfig/parser.go:505-533` 的 `wildcardMatch`（29 行手写通配符匹配）与 `path.Match` 在 Host 别名（不含 `/`）上行为等价，唯一差别是 `path.Match` 还会解释 `[...]` 字符类。如果确认要保留手写版本（fail-closed、不解释字符类是合理理由），建议在函数注释里写明"刻意不使用 path.Match，因为 OpenSSH 的 Host 模式不支持字符类"，否则下一个读者会重新提出这个问题。

### 23. sshconfig 的两处命名与签名问题（风格）

- `hostPatternsMatchAll`（`parser.go:362-373`）读起来像"所有模式都匹配"，实际含义是"这组模式匹配任意主机"（即存在未否定的 `*`）。与相邻的 `hostPatternsMatch(patterns, alias)`（`:489`）并列时特别容易误读。建议改名 `hostPatternsAreUniversal`。
- `collectAliases`（`parser.go:289`）用 `aliases *[]string` 输出参数，同时又传 `seen map[string]struct{}`——`seen` 已经能提供去重，`aliases` 只是为了保持顺序。建议改成方法接收一个小的收集器结构（`type aliasCollector struct { order []string; seen map[string]struct{} }`），把 6 个参数降到 4 个，也不再需要指针到切片。
- `document.hasLiteralAlias`（`parser.go:345-356`）先构造完整别名列表再线性查找；`Resolve` 因此把整份配置走了两遍（一遍枚举、一遍 `resolve`）。功能上没问题，但可以让 `literalAliases` 直接返回 `map[string]struct{}` 或让 `hasLiteralAlias` 复用 `seen`。

### 24. 测试中的小问题（风格）

- `internal/store/store_test.go:31` 声明 `now`，`:64` 用 `_ = now` 消化它——整个 `TestOpenMigratesAndConfiguresSQLite` 根本不需要这个变量。直接删掉两行。
- `internal/store/store_test.go` 一个 518 行的文件覆盖了 user / auth session / host / session 四个源文件。按 Go 惯例应拆成 `user_test.go`、`auth_sessions_test.go`、`hosts_test.go`、`sessions_test.go`，与源文件一一对应（`transcript`、`config` 都已经是这个形态）。
- `internal/version/version_test.go:5-12` 断言 `Version != ""` 和 `Commit != ""`，而这两个变量的默认值就在同一个包的 `version.go:6-7` 写死为 `"dev"` / `"unknown"`；不加 `-ldflags` 时这个测试恒真，加了 `-ldflags` 时测试也不会运行。真正的检查已经由 `scripts/browser-smoke.mjs:102-104`（"本地构建的二进制不应报告 dev"）完成了。建议删掉这个测试文件（`version` 包本身因为 `-ldflags -X` 需要独立的包路径，是合理的，见下）。
- `internal/security/security_test.go:51,65`、`internal/config/config_test.go:57,83,139`、`internal/store/store_test.go:61` 里的 `runtime.GOOS != "windows"` 分支：结合第 15 条，wmux 在 Windows 上无法运行，这些分支为一个不存在的目标增加了阅读成本。删掉 `lock_fallback.go` 时可以一并去掉。

---

## 看起来复杂但其实合理，不建议改

以下几处在通读时曾被我列为"可疑"，核实后认为复杂度是必要的，特此标注以免后续审查重复提出：

1. **transcript 的尾部崩溃恢复**（`store.go:152-230`）。四种坏尾形态（读到一半的行、JSON 坏、序号非单调、base64 坏）只在**最后一个 segment 的最后一行**被容忍，之前的任何损坏一律拒绝（`TestStoreRejectsCorruptionBeforeTail` 覆盖）。这正是 architecture.md "输出连续性由单调序号 + 有界 JSONL 提供"所要求的语义。第 8 条只建议消除其中的**重复**，不建议削弱恢复逻辑。

2. **transcript 的 `failed` 中毒状态与 `rollbackAppendLocked`**（`store.go:238-303`）。回滚失败后段文件长度不确定，继续追加会写出无法解析的记录；把 store 标为不可用、要求重开是唯一安全的选择，注释（`:275-277,284-286`）也解释了"提交内存状态必须晚于落盘"这一顺序。

3. **transcript 的 segment 分片 + 大小上限**。看似比"有上限的回放"重了，但没有分片就无法在不重写整个文件的前提下淘汰旧数据；`trimLocked`（`:322-335`）保留至少一个 segment 的处理和它的注释都是对的。

4. **store 的 generation 条件更新**（`sessions.go:218-267`）。architecture.md 明确说明这是防止"被取代的执行的迟到回调复活已删除的行"的机制。第 2 条只建议简化 SQL 的写法，`AND generation = ?` 必须保留。

5. **`store.Open` 里的 `openMu` + `BEGIN IMMEDIATE` 迁移事务**（`store.go:26,95-96`；`migrations.go:80-89`）。`PRAGMA journal_mode = WAL` 在并发打开时可能返回非 wal，进程内串行化是最简单的修法；注释也说清了跨进程由 `busy_timeout` + `config.DataDirLock` 负责。虽然生产里只打开一次，但代价只有一个包级 mutex。

6. **`security` 的自描述 scrypt 哈希格式**（`security.go:220-300`）。`$scrypt$ln=..,r=..,p=..$salt$hash` 让参数可以随硬件升级而变化且旧哈希仍能验证，这是密码存储的标准做法，不是过度设计。第 18.4 条只针对其中两个恒假的溢出判断。

7. **`security.LoadOrCreateMasterKey` 的临时文件 + 硬链接发布**（`security.go:77-113`）。`os.Link` 在目标存在时失败的语义正是"不覆盖别人刚创建的密钥"，比 `os.Rename` 更合适；`ok` 标志与两个 `defer` 的配合也是正确的。

8. **`config` 的跨进程数据目录锁本身**（不是它的三文件实现）。architecture.md 明确要求"同一时刻只有一个 wmux 进程持有数据目录"，否则两个 manager 会 attach 同一个 tmux 并写冲突的 transcript 序号。第 15 条只反对为不可运行平台准备第二套语义。

9. **`sshconfig` 选择自己写解析器，而不是引入现成库**。这个决定是站得住的：需求包含惰性 `Include` 展开（未激活的 `Include` 绝不打开，`TestInactiveConditionalIncludeIsNeverOpened` 覆盖）、`Match` 一律 fail-closed 且 `Match exec` 绝不执行（`TestMatchExecIsNeverExecuted`）、`IdentityFile` 只暴露布尔且绝不读内容（`TestIdentityFileIsBooleanOnlyAndNeverRead`）、`User` 禁止环境变量展开以免泄漏服务端 secret（`TestUserEnvironmentExpansionFailsClosedWithoutLeakingValue`）。这些是安全语义，通用库不会提供，事后加壳也不可靠。同样，`stripComment`/`splitArguments`（引号与转义）、`hostPatternsMatch`（否定模式）、`Include` 环路与深度限制，都是"正确解析 OpenSSH 配置"的最小集合。真正超出需求的是 `User` 的 token 展开（第 10 条）与取消检查的密度（第 17 条），不是解析器本身。

10. **`sshconfig` 把 `Include` 保持为惰性语法树节点、并区分 `literalAliases`（枚举）与 `resolve`（求值）两条遍历**。两条遍历看似重复，但枚举必须 fail-closed（只跟进全局 / `Host *` / `Match all` 下的 Include），求值必须跟进当前别名激活的 Include，规则确实不同，architecture.md "OpenSSH config discovery" 一节写明了这一点。

11. **`sshx.Probe` 通过在 `HostKeyCallback` 里返回错误来中止握手**（`sshx.go:51-68`）。这是 `golang.org/x/crypto/ssh` 下"只取 host key 不认证"的标准技巧，没有更干净的写法。TOFU 指纹流程（probe → 展示 → trust → 每次连接校验）也与 architecture.md 一致。

12. **`internal/version` 单独成包**。`-ldflags -X` 需要一个可寻址的包路径（`Dockerfile:21`、`scripts/build-server.sh:22` 都在用），把它并进 `internal/config` 或 `cmd/wmux` 会让构建脚本更脆弱。包本身 8 行，代价可以忽略。只有它的单元测试（第 24 条）没有价值。
