# 决策备忘：`internal/sshconfig` 是否应改用现成的 Go ssh_config 解析库

**日期**：2026-09-05
**范围**：`internal/sshconfig/sshconfig.go`（427 行）、`parser.go`（533 行）、`sshconfig_test.go`（817 行）、消费者 `internal/api/ssh_config.go`（183 行）与 `internal/api/ssh_config_test.go`（449 行）
**结论核实日期**：2026-09-05（所有库的版本 / 提交时间 / 源码行号均在此日核实；下文每条断言都附来源）
**本地环境基线**：OpenSSH_10.3p1, LibreSSL 3.3.6（用作行为真值的对照）

---

## 0. 结论摘要

**建议：保留手写实现，不迁移，也不做"库 + 外层过滤"的混合方案。**

决定性理由只有一条，而且是可复现验证的：

> wmux 最核心的安全语义——**未激活的条件 `Include` 绝不打开文件**（`TestInactiveConditionalIncludeIsNeverOpened`）——在所有候选库里都**不可能通过外层包装恢复**，因为这些库的 `Include` 文件打开动作发生在**解析阶段**，且**无条件**。副作用在库内部、在你拿到任何可过滤的返回值之前就已经发生了。

我用 `kevinburke/ssh_config` v1.6.0 实测确认了这一点（第 5.1 节，附可复现代码）：把 `Include` 指向一个**目录**并放在一个**永远不会被查询的 `Host` 块**里，`Decode()` 直接返回 `Error parsing Include directive: ... is a directory` —— 文件已经被打开了。

其余候选库更糟：`k0sproject/rig/v2/sshconfig` 默认会**真的执行 `Match exec`** 命令（`exec.Command`），并且主机名规范化会做 **DNS 查询**（`net.LookupHost` / `net.LookupCNAME`），同时违反 wmux 的"绝不执行"和"不触网"两条。

同时要指出：`docs/reviews/2026-09-05-overengineering-style/03-*.md` 第 9 条的结论（"不建议改"）**方向正确，但论证不完整**。它说"这些是安全语义，通用库不会提供，事后加壳也不可靠"——"不可靠"这个措辞太弱了。正确的说法是**在架构上不可能**：不是加壳质量问题，是副作用时序问题。这个区别很重要，因为"不可靠"会让下一个读者觉得"那我写好一点的壳就行"，而事实是没有任何壳能挡住 `Decode()` 内部的 `os.Open()`。

另外该条评审**漏掉了两个反向发现**（第 7 节）：wmux 的手写 `wildcardMatch` 在 `?` 通配符上比 `kevinburke/ssh_config` **更正确**（后者有 bug），而 wmux 的 tokenizer 在 tab 分隔上也比它更正确。也就是说"引入库换取正确性"这个通常成立的论据，在这个具体案例里是**反过来的**。

---

## 1. 问题

`internal/sshconfig/` 是一个 ~960 行的手写 OpenSSH `ssh_config` 解析器，零依赖，仅服务于"从 `~/.ssh/config` 导入主机"这一个功能。Go 生态中存在若干现成的 ssh_config 解析库。是否应该：

- (A) 保留手写；
- (B) 整体换成现成库；
- (C) 以现成库为底、在外层加一层安全过滤（混合）。

---

## 2. wmux 现有语义清单

以下是 wmux **实际依赖**的行为与安全语义。每条都给出实现位置与覆盖它的测试。这份清单是后面差距分析的评判标准。

### 2.1 安全语义（不可退让）

| # | 语义 | 实现位置 | 覆盖测试 |
|---|---|---|---|
| S1 | **惰性 `Include`**：只有当前 `Host` / `Match all` 块处于激活状态时才打开被包含文件；未激活的 `Include` **绝不 open、绝不 stat** | `sshconfig.go:302-320`（`case "include": if !active { continue }`）、`parser.go:318-337` | `TestInactiveConditionalIncludeIsNeverOpened`（用"指向目录的 Include"证明未打开） |
| S2 | **`Match` 一律 fail-closed**，只有 `Match all` 被支持；**`Match exec` 绝不执行** | `sshconfig.go:297-301`（注释 "in particular, exec is never run"）、`parser.go:316-317`、`isMatchAll` `parser.go:481-483` | `TestMatchExecIsNeverExecuted`、`TestMatchAllIsSupportedAndItsIncludeIsDiscoverable` |
| S3 | **`IdentityFile` 只暴露布尔**，绝不 stat、绝不读内容、路径绝不进入响应 | `sshconfig.go:346-353`、`Candidate.HasIdentityFile` `sshconfig.go:27` | `TestIdentityFileIsBooleanOnlyAndNeverRead`（断言 JSON 里不含路径、basename、内容）、api `TestSSHConfigConfiguredPathDoesNotExposeIdentityFile` |
| S4 | **`User` 禁止 `${ENV}` 展开**（防止把服务端 secret 展开进 API 响应） | `sshconfig.go:387-389` | `TestUserEnvironmentExpansionFailsClosedWithoutLeakingValue` |
| S5 | **`Include` 的 `%` token 白名单**：允许 `%d %u %i %l %L`（账户相关），拒绝 `%C %h %j %k %n %p %r`（依赖目标主机，发现阶段不安全） | `parser.go:229-253` | `TestIncludeRejectsMissingEnvironmentAndTargetDependentTokens` |
| S6 | **`Include` 的 `${ENV}` 有白名单校验且未设置即报错**（不静默展开为空） | `parser.go:190-227`（`validEnvironmentName`） | `TestIncludeExpandsEnvironmentHomeAndLiteralPercent`、同上 |
| S7 | **只读、不触网、不落库**：不 mutate SQLite、不探测指纹、不连接主机 | `api/ssh_config.go:115-130`（注释 "Deliberately do not copy IdentityFile contents"、"does not probe the network"） | `TestDiscoverSSHConfigIsReadOnlyAndMarksExactExistingHost`、`TestImportSSHConfigReResolvesWithoutCredentialsFingerprintOrProbe` |
| S8 | **账户 home 而非 `$HOME`**：`~`、`%d`、相对 `Include` 都走 `user.Current()`，进程环境不能覆盖 | `sshconfig.go:170-197`（`runningAccount`） | `TestDefaultPathUsesAccountHomeNotEnvironment`、`TestRunningAccountAndPercentDIgnoreEnvironmentHome` |
| S9 | **非常规文件在 open 前拒绝**（stat 检查 + open 后二次检查，防 TOCTOU） | `parser.go:71-73`、`94-97` | `TestNonRegularConfigIsRejectedBeforeOpen` |
| S10 | **错误不泄漏路径 / 行号 / 命令 / 文件内容**到 API 响应与结构化日志 | `api/ssh_config.go:160-172`（注释 "The public error is intentionally stable"） | `TestSSHConfigMissingAndFailuresHaveStableSafeResponses` |
| S11 | **枚举 fail-closed**：只跟进 全局 / 无否定的 `Host *` / `Match all` 下的 `Include`；其他条件块内的别名**故意不枚举** | `parser.go:395-432`（`collectAliases`、`hostPatternsMatchAll`） | `TestConditionalIncludeIsLazyAndDoesNotExposeAliases`、`TestUniversalHostIncludeIsDiscoverableButNegatedHostIsFailClosed` |

### 2.2 解析正确性语义

| # | 语义 | 实现位置 | 覆盖测试 |
|---|---|---|---|
| P1 | `Include` **环路检测 + 深度上限 32** | `sshconfig.go:283,306-316`、`parser.go:17,290,322-332` | `TestIncludeCycleAndDepthAreBounded` |
| P2 | `Host` **否定模式**（`!pattern` 命中即整块失效） | `parser.go:505-518`（`hostPatternsMatch`） | `TestResolveHonorsFirstValueWildcardInheritanceAndNegation` |
| P3 | 通配符 `*` / `?`（`?` = 恰好一个字符） | `parser.go:520-533`（`wildcardMatch`） | 同上 + `TestLiteralAliasesAndHostPatternsAreCaseSensitive` |
| P4 | OpenSSH **first-value-wins**，但 `IdentityFile` 是例外（累积） | `sshconfig.go:335-353` | `TestResolveInheritsLaterWildcardDefaults`、`TestIdentityFileAccumulatesLikeOpenSSH` |
| P5 | `HostName` 的 `%h` 展开（+ `%%` 字面量），其余 token 拒绝 | `sshconfig.go:372-381` | `TestHostNameExpandsAliasAndLiteralPercent` |
| P6 | `User` 的 8 个 token 展开（`%d %h %i %j %l %L %n %p %u`，`%k` 拒绝） | `sshconfig.go:383-427` | `TestUserExpandsSafeAccountAndTargetTokens` |
| P7 | 引号 / 反斜杠转义 / `key=value` 分隔 / 引号内的 `#` 不算注释 | `parser.go:437-503`（`parseDirective`、`stripComment`、`splitArguments`） | `TestQuotedAndEqualsSyntax` |
| P8 | `Include` glob 结果**字典序稳定**、重复 include 去重、别名去重 | `parser.go:166-179`、`parser.go:388-393` | `TestDiscoverRecursivelyIncludesGlobsInStableOrder`、`TestDuplicateIncludesAndAliasesAreDeterministic` |
| P9 | **Host 模式大小写敏感**（与 OpenSSH 10.3p1 一致，见 §7.3） | `parser.go:505-533` | `TestLiteralAliasesAndHostPatternsAreCaseSensitive` |
| P10 | 被包含文件的 `Host` 状态**不回溢**到调用方；裸选项继承调用方的激活状态 | `sshconfig.go:282-296`（`apply` 每层局部 `active := true`） | `TestIncludedHostStateDoesNotEscapeAndBareOptionsInheritActiveCall` |
| P11 | `ProxyJump` / `ProxyCommand` 标记为 `Unsupported`（`none` 与空值忽略），导入路径 422 拒绝 | `sshconfig.go:269-276`、`api/ssh_config.go:80-83` | `TestUnsupportedProxyOptionsAreCanonicalAndNoneIsIgnored`、`TestImportSSHConfigRejectsUnsupportedMissingDuplicateAndInvalid` |
| P12 | `Port` 校验 1..65535 | `sshconfig.go:254-260` | `TestInvalidPortIsRejected` |
| P13 | 每次调用重新加载（用户改配置无需重启） | `sshconfig.go:36-38` 文档注释 + `Discover`/`Resolve` 各自 `loadDocument` | 隐含于全部集成测试 |
| P14 | `context` 取消传播 | `sshconfig.go:208-213`、多处 `contextError` | `TestCanceledContextStopsDiscoverAndResolve` |

### 2.3 消费者实际用到的 API 面

`internal/api/ssh_config.go:140-150`（`publicSSHConfigCandidate`）只读取 6 个字段：

```
Candidate.Alias / Address / Port / Username / HasIdentityFile / Unsupported
```

加上包级的 `Result.Available` / `Result.Source`（`Source` 甚至没有直接用——`api/ssh_config.go:44-49` 的 `publicSSHConfigSource()` 重新从 `s.config.SSHConfigPath` 算了一遍）与 `ErrAliasNotFound`（`api/ssh_config.go:73`）。

**这个 API 面极窄，是"手写划算"的一个正面论据**：需要从库里提取的信息量很小，但需要施加的约束很多。库带来的价值（100+ 配置键、round-trip、写回）在这里**全部用不上**。

---

## 3. 候选库对比表

核实日期均为 **2026-09-05**。

### 3.1 元数据

| 库 | 许可证 | 最新版本 / 日期 | 最近提交 | Star | 依赖 | OSV 已知漏洞 |
|---|---|---|---|---|---|---|
| [`github.com/kevinburke/ssh_config`](https://github.com/kevinburke/ssh_config) | 自定义 MIT 文本（GitHub API 报 `NOASSERTION`；[LICENSE](https://raw.githubusercontent.com/kevinburke/ssh_config/master/LICENSE) 为 MIT 措辞 + go-toml 的 MIT） | v1.6.0（2026-02-16），v1.7 未发布（在 master）<br>[CHANGELOG](https://raw.githubusercontent.com/kevinburke/ssh_config/master/CHANGELOG.md) | 2026-05-04（`pushed_at`，[API](https://api.github.com/repos/kevinburke/ssh_config)） | 472 | **零依赖**（[go.mod](https://raw.githubusercontent.com/kevinburke/ssh_config/master/go.mod) 无 require） | 无（[OSV](https://api.osv.dev/v1/query) 返回 `{}`） |
| [`github.com/trzsz/ssh_config`](https://github.com/trzsz/ssh_config) | 继承上游（`NOASSERTION`） | 无自有 tag；`main` 分支 | 2026-03-29（[API](https://api.github.com/repos/trzsz/ssh_config)） | **4** | 零依赖 | 无 |
| [`github.com/k0sproject/rig/v2/sshconfig`](https://pkg.go.dev/github.com/k0sproject/rig/v2/sshconfig) | **Apache-2.0** | v2.1.1 | 2026-09-02（[API](https://api.github.com/repos/k0sproject/rig)） | 53（整个 rig） | **重**：模块 [go.mod](https://raw.githubusercontent.com/k0sproject/rig/main/go.mod) require winio、go-pageant、winrm、testify、x/crypto、x/term、yaml.v2 + ~19 个 indirect（gokrb5、ntlmssp、goxpath…） | 无 |
| [`github.com/mikkeloscar/sshconfig`](https://github.com/mikkeloscar/sshconfig) | **GPL-3.0** ⛔ | 无 tag | 2025-03-13（[API](https://api.github.com/repos/mikkeloscar/sshconfig)） | 61 | 少 | 无 |
| [`github.com/shuLhan/share/lib/ssh/config`](https://pkg.go.dev/github.com/shuLhan/share/lib/ssh/config) | BSD-3-Clause | — | 2024-03-09；仓库描述已标 **`[Deprecated]`**，迁至 `git.sr.ht/~shulhan/pakakeh.go` | 2 | 巨型 grab-bag 模块 | — |
| [`github.com/petems/go-sshconfig`](https://github.com/petems/go-sshconfig) | MIT | 无 tag | 2024-12-01 | 2 | 少 | — |
| [`github.com/tg123/sshpiper`](https://github.com/tg123/sshpiper) | — | — | — | — | — | **不适用**：这是 sshd 反向代理，不提供可复用的 ssh_config 解析包 |
| [`golang.org/x/crypto/ssh`](https://pkg.go.dev/golang.org/x/crypto/ssh) | BSD-3-Clause | v0.56.0（wmux 已依赖） | — | — | — | **不提供任何 ssh_config 解析能力**；长期未决的请求是 [golang/go#18781](https://github.com/golang/go/issues/18781) |

### 3.2 能力矩阵

| 能力 | kevinburke v1.6.0 / master | trzsz fork | k0s rig v2 | mikkeloscar | wmux 需要 |
|---|---|---|---|---|---|
| **`Include` 惰性**（未激活块不打开） | ❌ **贪婪**：`NewInclude` 在解析时立刻 glob + `parseWithDepth`；源码注释 "Configuration files are parsed greedily (e.g. as soon as this function runs)"（[config.go:748-792](https://github.com/kevinburke/ssh_config/blob/master/config.go)）。**实测确认**（§5.1） | ❌ 同上（fork 未改 `NewInclude`） | ❌ 贪婪：tree parser 里直接 `os.Open(match)`（[tree.go `parseTree`](https://github.com/k0sproject/rig/blob/main/sshconfig/tree.go)，`case keyIncludeLower:`）；`parser.go:297-298` 注释 "the tree parser has already included the file as just another beanch" | ❌ 贪婪（`filepath.Glob` + 直接读） | ✅ **必须惰性** |
| **`Include` 环路检测** | ❌ 无；仅 `maxRecurseDepth = 5`（config.go:724）。实测两文件互相 include → `max recurse depth exceeded`（误导性错误） | ❌ 同上 | ✅ 有（`tree.go` `includes map[string]struct{}` → "circular include directive"），另有路径穿越守卫 | ❌ 无 | ✅ 环路 + 深度 32 |
| **`Match all`** | ✅ v1.5+（parser.go `parseMatch` `case "all"`） | ✅ | ✅ | ❌ 解析即报错 | ✅ |
| **`Match exec`** | ✅ **不执行**，但是**整文件硬解析错误**："ssh_config: Match Exec is not supported"（parser.go `case "exec"`） | ✅ 不执行；**静默跳过整个块**（`skipUntilNextBlock`，[parser.go:175-262](https://github.com/trzsz/ssh_config/blob/main/parser.go)） | ⛔ **真的执行**：`Setter.matchesExec` → `s.executor.Run(cmd, args...)`（[set.go:1633](https://github.com/k0sproject/rig/blob/main/sshconfig/set.go)），默认 executor 为 `exec.Command`（[parser.go:158-166](https://github.com/k0sproject/rig/blob/main/sshconfig/parser.go)）；包文档明写 "Match exec commands are executed directly" | ❌ 不支持 Match | ✅ **绝不执行**（块级跳过，不能让整文件失败） |
| **`Match user` / `localuser` / `originalhost`** | ❌ master 的 `parseMatch` `default:` 分支→**硬错误** `unsupported Match criterion "user"`（实测确认）。注意 CHANGELOG v1.5 声称支持这几个，**与 master 源码矛盾** | ✅ 静默跳过 | ✅ 支持 | ❌ | fail-closed 即可，但**不应让整文件失败** |
| **`Match canonical` / `final`** | ❌ 硬错误 | 跳过 | ✅ 支持（含 `CanonicalizeHostname`） | ❌ | fail-closed |
| **DNS / 网络访问** | ✅ 无 | ✅ 无 | ⛔ **有**：`net.LookupHost` / `net.LookupCNAME`（[set.go:1808,1812](https://github.com/k0sproject/rig/blob/main/sshconfig/set.go)，`CanonicalizeHostname`） | ✅ 无 | ✅ **绝不触网** |
| **`%` token 展开** | ❌ 完全不做（实测：`Get(t,HostName)` 返回原样 `"%h.example.com"`） | ❌ | ✅ 部分（`%%,%C,%d,%h,%H,%j,%L,%n,%p,%r,%u`） | ❌ | 需要 `%h`（HostName）、Include 的账户 token 白名单 |
| **`${ENV}` 展开** | ❌ 不做（实测返回原样 `"${SECRET_ENV}"`） | ❌ | ✅ 做（"Expansion of ~ and environment variables in the values where applicable"） | ❌ | `Include` 要，`User` **必须禁止** |
| **`~` 展开** | 仅在 `Include` 路径里（v1.6.0 新增）；返回值不展开 | ✅ 同上 | ✅ | ✅ | 与 wmux 相同粒度 |
| **`Host` 否定模式** | ✅ 正确（`Host.Matches`，config.go:550-567；实测正确） | ✅ | ✅ | ❌ 不识别 | ✅ |
| **`?` 通配符语义** | ❌ **BUG**：编译成 `.?`（零或一个字符，[config.go:508](https://github.com/kevinburke/ssh_config/blob/master/config.go)）。实测 `Host web?` 匹配了 `web`；OpenSSH 10.3p1 不匹配 | ❌ 同上 | ✅ | — | ✅ wmux 正确 |
| **tab 分隔的 `Host` 模式** | ❌ **BUG**：`strings.Split(val.val, " ")`（parser.go）。实测 `Host<TAB>a<TAB>b` 两个别名都匹配不到 | ❌ | ✅ (`argv_split` 移植) | — | ✅ wmux 用 `unicode.IsSpace` |
| **`IdentityFile` 处理** | 返回**原始字符串**（`GetAll` 累积）。⚠️ 陷阱：包级 `ssh_config.Default("IdentityFile")` 在 v1.6.0 返回 `~/.ssh/identity`，用包级 `Get()` 会**凭空造出**一个 IdentityFile | 同上 | 展开路径 + token 替换（更"热心"） | 存原始字符串 | ✅ **只要布尔，且绝不解析路径** |
| **未知指令** | fail-open（当作 KV 收下，实测无错） | fail-open | 默认 fail-open，`WithStrict()` 可开 | 静默跳过 | fail-open 可接受 |
| **主机别名枚举** | ⚠️ 可绕：遍历导出的 `Config.Hosts[].Patterns[].String()`，但要自己再解析 `!` / `*` / `?` 前缀 | 同上（fork 额外导出了 include struct / pattern 成员） | ❌ 设计上不提供——包文档："The SSH configuration files do not define a list of hosts, it's not a phone-book" | ✅ 返回 `[]*SSHHost` | ✅ 必须 |
| **`context` 取消** | ❌ 无 | ❌ 无 | ❌ 无（`log.Trace(context.Background(), ...)`） | ❌ 无 | ✅ 有 |
| **账户 home 可注入** | ❌ 包级 `homedir()`（config.go:63-70），`osuser.Current()` → `$HOME` 兜底；`NewInclude` 直接调用，无法注入 | ❌ | ✅ `WithUserHome(home)` | ❌ | ✅ 需要（测试 + 语义） |
| **非常规文件拒绝** | ❌ 无 | ❌ | ❌ | ❌ | ✅ 有 |

---

## 4. 差距分析

把第 2 节的语义逐条对照最优候选（`kevinburke/ssh_config` master，因为它零依赖、不执行 exec、不触网、被 526 个包引用）：

### 4.1 库直接提供的

| wmux 语义 | 说明 |
|---|---|
| P2 `Host` 否定模式 | `Host.Matches` 语义与 wmux 一致 |
| P4 first-value-wins（`Config.Get` 遇到第一个匹配即返回） | 一致 |
| P7 引号 / `=` 分隔 / 注释剥离 | 基本一致（v1.6.0 才修好去引号，见 CHANGELOG "#61"）；但 tab 分隔有 bug |
| P9 大小写敏感 | 一致（实测 `Host Prod` 不匹配 `prod`） |
| P10 include 状态不回溢 | 一致（实测返回 2400，与 OpenSSH 10.3p1 一致） |
| S2 的一半：`Match exec` 不执行 | ✅ 一致，但错误粒度不同（见 4.2） |

**只有 6 条**，而且都是 wmux 已经写对了的部分。

### 4.2 需要在库外补（可行，但代码不会变少）

| wmux 语义 | 需要补什么 |
|---|---|
| S3 `IdentityFile` 布尔化 | 用 `Config.GetAll("IdentityFile")`，判空 + 排除 `none`；**必须避开包级 `Get()`**，否则会拿到假的 `~/.ssh/identity` 默认值 |
| S4 `User` 禁 `${ENV}` | 库不展开，所以只要**不自己展开**即可 ✅（但也就意味着 §2.2 P6 的 8 个 token 全部要自己写，代码原样保留） |
| S5/S6 `Include` token / env 白名单 | 库不做——但因为库的 `Include` 是贪婪的，这层过滤**已经晚了**（见 4.3） |
| S7/S10 只读 / 错误不泄漏 | api 层已有，与库无关 |
| S9 非常规文件拒绝 | 库不做；要在把 `io.Reader` 交给 `Decode` 之前自己 stat + open + 二次检查 |
| S11 枚举 fail-closed | 遍历 `Config.Hosts[].Patterns[].String()`，重新解析 `!`/`*`/`?` 前缀 + 自己判断哪些 `Include` 可跟进 |
| P1 环路检测 | 库只有 depth=5、无环检测；要么接受更弱的保证，要么自己在 `Decode` 前预扫 |
| P3 `?` 语义 | 库有 bug，只能**绕过 `Host.Matches` 自己实现** —— 即 `wildcardMatch` 原样保留 |
| P5/P6 `%` token | 库完全不做，`expandHostName` / `expandUser` 原样保留（~55 行） |
| P11 ProxyJump/ProxyCommand | `Config.Get` 拿字符串 + `isNone` 判断，自己写 |
| P12 Port 校验 | 自己写 |
| P13 每次重载 | 自己 `Decode` 而非用 `DefaultUserSettings`（后者有全局缓存） |
| P14 `context` 取消 | 库无法支持；只能在 `Decode` 前后检查，粒度变粗 |
| S8 账户 home | 库的 `homedir()` 是**包级函数、不可注入**；`newWithHome` 那套测试策略要么废弃、要么改成 `t.Setenv("HOME")`（污染全局，与 S8 的立意矛盾） |

### 4.3 库做不到的（决定性）

| wmux 语义 | 为什么做不到 |
|---|---|
| **S1 惰性 `Include`** | `Decode()` 在返回之前就已经把**每一个** `Include` 目标 glob + open + 递归解析完了，**无视外层 `Host` / `Match` 块是否激活**。这不是"库返回了多余数据、外层过滤掉"的问题——**文件已经被打开**了。外层拿不到任何可以阻止它的钩子。<br>唯一的"外层"补救是：先自己扫一遍配置、判断哪些 `Include` 行处于激活状态、把非激活的 `Include` 行**从字节流里删掉**再喂给 `Decode` ——但要判断激活状态，你必须实现 `Host`/`Match` 激活状态机，也就是 `apply` + `collectAliases` 那 ~120 行；而且因为激活状态**依赖目标别名**，你得为每个别名重扫 + 重解析一遍（N × 全量解析）。**混合方案在这里退化成"手写解析器 + 一个多余的依赖"。** |
| **S11 枚举 fail-closed 的完整性** | 同源问题：枚举需要"哪些 Include 可以安全跟进"的判断，而库已经全部跟进了 |

对 `k0sproject/rig` 还要再加两条**硬性做不到**：

- **`Match exec` 会被执行**。`WithExecutor` 可以换掉实现，但该选项自己的文档写的是 "for testing (or disabling?)"——一个带问号的注释，说明作者并不把它当作安全开关。依赖一个"作者自己不确定用途"的选项来守住"绝不执行命令"这条底线，是不可接受的风险面；上游任何一次重构都可能新增一条绕过它的执行路径。
- **`CanonicalizeHostname` 会做 DNS 查询**。它由**用户配置文件里的 `CanonicalizeHostname` 指令**触发，也就是说 wmux 服务端是否发起 DNS 请求由被解析的文件决定——直接违反"discovery 不触网"。虽然可以 `WithNoFinalize()` 跳过 finalize 阶段，但同样是"用一个非安全导向的选项守安全边界"。

### 4.4 "库 + 外层过滤" 能否守住 fail-closed？

**不能。** 归纳一下：

1. 需要守的是**解析阶段的副作用**（打开文件、执行命令、DNS 查询），不是返回值的内容。
2. 外层过滤只能作用于返回值。
3. 因此外层过滤在架构上无法守住 S1 / S2（对 rig）/ S7。
4. 唯一的技术路径是 **fork 库并改造 `Include` 为惰性**——那就不再是"用库"，而是"维护一个 1148 行的第三方代码分支"，比现在自己维护 960 行更贵。

---

## 5. 核实方法（可复现）

所有关键断言都做了实测，不是读文档得出的。探针代码是会话临时目录中的一个独立 Go module（未保留、未触碰 wmux 仓库），下文给出可复现的关键片段。

### 5.1 `Include` 贪婪性（决定性证据）

```go
// 把 Include 指向一个目录，并放在一个永远不会被 Get() 查询的 Host 块里
root := "Host neverselected\n  Include " + aDirectory + "\nHost other\n  HostName other.example\n"
_, err := ssh_config.Decode(strings.NewReader(root))
```

实测输出（kevinburke v1.6.0）：

```
Decode with Include -> directory under NON-matching Host:
  err = (2, 11): Error parsing Include directive: read /.../adir: is a directory
```

**解析阶段就打开了那个从不会被查询的 `Include`。** wmux 的 `TestInactiveConditionalIncludeIsNeverOpened` 正是用同一个手法（`Include` 指向目录）来证明相反的性质。

### 5.2 其他实测结果（kevinburke v1.6.0）

```
== Match ==
  Decode("Match exec \"touch /tmp/pwned\"")  -> err = (1,7): ssh_config: Match Exec is not supported   [整文件失败]
  Decode("Match user bob")                   -> err = (1,7): ssh_config: unsupported Match criterion "user"  [整文件失败]

== '?' 通配符 ==
  Host web?  -> Get("web",Port)="2200"   Get("web1",Port)="2200"   Get("web12",Port)=""
  OpenSSH 10.3p1 (ssh -G): web -> 22      web1 -> 2200              web12 -> 22
  => kevinburke 的 `web?` 错误地匹配了 `web`

== token / env 展开 ==
  Get(t,HostName)     = "%h.example.com"     [不展开]
  Get(t,User)         = "${SECRET_ENV}"      [不展开]
  Get(t,IdentityFile) = "~/.ssh/id_ed25519"  [不展开]

== 未知指令 ==
  Decode("ThisIsNotARealKeyword whatever") -> err = <nil>   [fail-open]

== tab 分隔的 Host 行 ==
  Host<TAB>a<TAB>b -> Get("a",Port)=""  Get("b",Port)=""   [两个别名都丢了]

== Include 环路 ==
  c1 -> c2 -> c1  =>  err = "ssh_config: max recurse depth exceeded"   [靠深度上限兜住，无环检测]

== IdentityFile 默认值陷阱 ==
  Config.Get(t,"IdentityFile") 未设置时 = ""            [OK]
  ssh_config.Default("IdentityFile")   = "~/.ssh/identity"  [包级 Get() 会凭空造出这个值]

== 与 wmux 一致的部分 ==
  Host 否定:      Host * !secret  -> Get("anything")="3"  Get("secret")=""      ✅
  大小写敏感:      Host Prod       -> Get("Prod")="4"      Get("prod")=""        ✅
  include 状态不回溢:  Host target { Include child; Port 2400 } -> 2400          ✅
  裸 include 继承激活:  Host bare { Include child(Port 2300) }   -> 2300          ✅
  Match all:      -> 匹配任意 alias                                             ✅
```

### 5.3 OpenSSH 真值对照（本机 OpenSSH_10.3p1）

```
$ printf 'Host web?\n  Port 2200\n' > w.conf
$ ssh -G -F w.conf web    # port 22    <- 不匹配
$ ssh -G -F w.conf web1   # port 2200  <- 匹配
$ ssh -G -F w.conf web12  # port 22    <- 不匹配
=> `?` 恰好一个字符。wmux 的 wildcardMatch 正确，kevinburke 错误。

$ printf 'Host Prod\n  Port 2222\n' > oc.conf
$ ssh -G -F oc.conf Prod  # port 2222  <- 匹配（hostname 输出被小写成 prod）
$ ssh -G -F oc.conf prod  # port 22    <- 不匹配
=> Host 模式对原始目标串大小写敏感。wmux 的 P9 与 OpenSSH 一致。

$ ssh -G -F root.conf target   # root: Host target { Include child; Port 2400 }, child: Host other { User hidden }
  user waterlens
  port 2400
=> 被包含文件的 Host 状态不回溢。wmux 的 P10 与 OpenSSH 一致。
```

---

## 6. 建议与理由

### 建议：**保留手写（方案 A）**，并做三项低成本增强

#### 理由

1. **决定性理由（架构层）**：wmux 的第一安全语义 S1（惰性 `Include`）与库的解析模型**根本冲突**。所有候选库都在解析阶段无条件打开 `Include`。这不是接口不够、包一层能补的问题——副作用在返回值之前就发生了。混合方案（C）在推导到底之后会退化为"手写状态机 + 一个多余依赖"，是三个方案里最差的。

2. **API 面不匹配**：wmux 只需要 6 个字段（§2.3）。库的核心价值（100+ 配置键、注释保留、round-trip 写回、`String()`/`MarshalText()`）**一项都用不上**。用一个为"编辑 ssh_config 文件"设计的库来做"以最小信息面只读枚举"，方向是反的。

3. **正确性论据反转**：通常"引入库"的最强理由是"别人比你更懂边界情况"。这里不成立——kevinburke 在 `?` 通配符（`.?` 而非 `.`）和 tab 分隔的 `Host` 行上都有实测可复现的 bug，而 wmux 两处都是对的（§5.2、§5.3）。

4. **失败粒度更差**：kevinburke 遇到 `Match user` 会让**整个配置文件解析失败**。真实用户的 `~/.ssh/config` 里有一个 `Match user` 块，wmux 的导入功能就会整体返回 `ssh_config_invalid`，而不是导入其余主机。wmux 现在的行为是块级 fail-closed（跳过该块，继续），**更正确也更可用**。trzsz fork 改成了静默跳过，但那是一个 4 star / 单人维护 / 无 tag 的 fork，作为服务端解析器的信任基线不够。

5. **依赖成本 vs 收益**：wmux 现在 `go.mod` 只有 4 个直接依赖，是个很干净的自托管服务。为了一个次要功能引入解析器（尤其 rig 会带进 gokrb5 / ntlmssp / winrm 这一整片攻击面）不划算。

6. **迁移不会减少代码**（§8 详算）：能删的（tokenizer + 通配符 + 文件扫描 ≈ 200 行）少于必须新增的（枚举适配 + 惰性 Include 的 fork 或预扫 + 各种守卫 ≈ 200+ 行），并且要额外承担一个 fork 的长期维护。

#### 建议的三项增强（都不引入依赖）

| # | 建议 | 成本 | 价值 |
|---|---|---|---|
| E1 | **加一个以 `ssh -G` 为真值的差分测试**（CI 里有 OpenSSH 时运行，否则 `t.Skip`）。语料覆盖引号 / tab / `=` 分隔 / 通配符 / 否定 / first-value / IdentityFile 累积。**注意**：`ssh -G` 会执行 `Match exec`，语料必须不含 exec。 | ~120 行测试 | 这是"引入库换正确性"能给的东西里唯一真正有价值的部分，而且 `ssh -G` 是比任何库都权威的真值源 |
| E2 | 在 `wildcardMatch`（`parser.go:520`）上补一条注释，写明**刻意不用 `path.Match`**（OpenSSH 的 Host 模式不支持 `[...]` 字符类）。同时补一条注释说明 `?` = 恰好一个字符，并引用 §5.3 的 `ssh -G` 结论。 | 4 行注释 | 03-*.md 第 22 条已提过；现在有了"通用库在这里写错了"的实证，注释更有说服力 |
| E3 | 借鉴 rig 的 **`Include` 路径穿越守卫**（`tree.go` 里对 include 相对路径的 `..` 检查）作为一个**纵深防御**：wmux 现在把相对 `Include` join 到 `~/.ssh` 后 `Clean`，`Include ../../etc/xxx` 是可以逃出去的。 | ~8 行 | 优先级低（能写 `~/.ssh/config` 的人本来就控制了这个账户），但成本近零，且能让"discovery 边界 = `~/.ssh` + 显式绝对路径"这句话在代码里成立 |

**不建议采纳的"借鉴"**：kevinburke 的 tokenizer 引号测试用例（`config_test.go` + `testdata/`）——它的 tokenizer 在 tab 上有 bug，其测试语料显然没覆盖到，直接搬过来价值有限。E1 的 `ssh -G` 差分测试是它的严格上位替代。

---

## 7. 手写实现相对通用库"多做了 / 少做了"的地方

### 7.1 多做了（相对通用库，且**应当保留**）

| 项 | 说明 |
|---|---|
| 惰性 `Include` | 无任何库提供。这是整个包存在的理由 |
| `Include` 环路检测（真环检测，非仅深度） | kevinburke 无、mikkeloscar 无；只有 rig 有 |
| 深度上限 32 vs kevinburke 的 5 | wmux 更接近 OpenSSH 的实际容忍度（`ssh_config(5)` 允许 16 层） |
| 非常规文件 stat + open 后二次检查 | 无库提供 |
| `context` 取消 | 无库提供（rig 甚至用 `context.Background()`） |
| 账户 home 可注入 + 不受 `$HOME` 影响 | 只有 rig 提供（`WithUserHome`） |
| `Include` 的 `%` token / `${ENV}` 白名单 | 无库提供这个粒度的白名单 |
| 枚举 API（`literalAliases`）+ 枚举的 fail-closed 规则 | kevinburke 要自己在 `Config.Hosts` 上重造；rig 明确不提供 |
| `?` 通配符正确、tab 分隔正确 | 见 §5.2/§5.3，比 kevinburke 更正确 |

### 7.2 多做了（相对**需求**，可以砍——与 03-*.md 第 10/17 条一致）

| 项 | 说明 |
|---|---|
| `expandUser` 的 8 个 `%` token（`sshconfig.go:383-427`，45 行） | 通用库一个都不做，OpenSSH 文档里有但真实配置极罕见。尤其 `%j` 需要把 `proxyJump` 一路穿参数传进来（`sshconfig.go:263`），而 `Unsupported` 非空的候选在 `api/ssh_config.go:80-83` 已被 422 拒绝——`%j` 展开出的用户名**在本项目数据流里不可能被存下来**。**这一条我独立验证后同意 03-*.md 第 10 条**：删到只留 `%%`（或再留 `%u`/`%d`），与 `HostName` 只留 `%h` 的克制保持一致 |
| `contextError` 的 `ctx == nil` 分支（`sshconfig.go:209-211`） | 所有入口来自 `r.Context()` 或测试的 `context.Background()`，不可能触发 |
| `index%64 == 0` 的取消检查密度（两处逐字重复） | 见 03-*.md 第 17 条 |

### 7.3 少做了（相对 OpenSSH，需要**确认这是有意的**）

| 项 | 说明 |
|---|---|
| **不读 `/etc/ssh/ssh_config`** | `sourcePath`（`sshconfig.go:136-153`）只构造 `~/.ssh/config` 或显式配置路径。OpenSSH 会在用户配置之后再读系统配置，系统级 `Host` 块 / 默认 `User` / `Port` 对 wmux **完全不可见**。kevinburke（`systemConfigFinder`）和 rig（`WithGlobalConfigPath`）都读。<br>**这可能是有意的**（容器部署只挂载用户配置更可控，architecture.md:84 的口径也指向这个），但 `docs/architecture.md:76-84` **没有明说**，评审文档也没提。建议在 architecture.md 里补一句"不读系统配置及其原因"，否则会被当成 bug 反复提出 |
| `Match` 除 `all` 外全部 fail-closed | 有意（S2），与 OpenSSH 不同但方向正确 |
| `Host` 状态不回溢 | 经 `ssh -G` 实测，**与 OpenSSH 10.3p1 一致**，不是 divergence（测试名 `...DoesNotEscape...` 描述准确） |
| 不做 `CanonicalizeHostname` | 有意（不触网） |
| 不做 `%` token 的完整集合 | 有意（S5 白名单） |

---

## 8. 若仍要迁移：步骤与成本

前提：只有 `kevinburke/ssh_config` 值得考虑（零依赖、不执行、不触网）。**并且必须 fork**，因为惰性 `Include` 无法从外部实现。

### 步骤

1. Fork `kevinburke/ssh_config`，把 `NewInclude`（config.go:748-792）改成惰性：只记录 pattern，不 glob、不 `parseWithDepth`；新增 `Include.Resolve(ctx, base, account)` 由调用方在确认块激活后触发。**这会破坏库的 `String()` / `MarshalText()` round-trip 契约**（`Include.String()` 依赖已解析的 `matches`），需要一并处理。
2. 在 fork 里加：真·环路检测、深度改 32、可注入的 `homedir`、`context` 传递、非常规文件拒绝。
3. 修 `?` → `.`（config.go:508）与 `Host` 行的 tab 分隔（parser.go）。
4. 把 `Match` 的 `default:` 与 `exec` 从"整文件硬错误"改成"块级跳过"（可参考 trzsz 的 `skipUntilNextBlock`）。
5. wmux 侧：写 `literalAliases` 适配层（遍历 `Config.Hosts[].Patterns[].String()`，重解析 `!`/`*`/`?`，判断哪些 `Include` 可跟进）。
6. wmux 侧保留：`expandHostName`、`expandUser`、`expandEnvironment`、`expandIncludeTokens`、`expandPercentTokens`、`includeMatches`、`isNone`、Port 校验、`Candidate` 组装（这些库全都不做）。
7. 重写 817 行测试：约 60% 可保留（黑盒 `Discover`/`Resolve` 断言不变），约 40% 需重写（依赖 `newWithHome`、`processUsername` 等内部注入点的用例——fork 后注入方式变了）。
8. 新增：fork 的上游同步流程、`replace` 或独立仓库、fork 自身的 CI。

### 成本估算

| 项 | 行数变化 |
|---|---|
| 可删除的 wmux 代码：`parseDirective`/`stripComment`/`splitArguments`（~90）、`wildcardMatch`/`hostPatternsMatch`（~45）、`loadFile` 扫描部分（~70） | **−205** |
| 必须新增的 wmux 代码：枚举适配层（~60）、fork 注入点胶水（~40）、fail-closed 守卫（~40） | **+140** |
| **wmux 仓库净变化** | **≈ −65 行** |
| **新承担的第三方 fork** | **+1148 行**（kb config.go 878 + parser.go 270，尚不含 lexer.go / validators.go / position.go） |
| 测试改动 | ~330 行重写 + fork 侧测试 |
| **净拥有代码量** | **从 960 行 → ≈ 2000 行**（自有 895 + fork 1148） |

### 风险

- **上游同步负担**：惰性 `Include` 是对库核心数据流的侵入式改造，几乎不可能被上游接受（它与 `String()` round-trip 这个库的立身之本冲突），意味着**永久分叉**。上游每次发版（v1.7 已在 master，含大量默认值破坏性变更）都要人工 rebase。
- **安全回归窗口**：改造期间 S1/S2/S11 全部处于"已改但未被现有测试覆盖"的状态。这几条正是 `docs/reviews/2026-09-04-independent-code-review.md:134` 点名的、被独立评审确认为项目强项的安全边界。
- **正确性回归**：fork 引入了 kevinburke 的 `?` 与 tab bug，必须在迁移中一并修掉，否则是净负向。
- **收益为负**：净拥有代码量翻倍，安全语义不变或变差，唯一收益是"用了库"这个形式。

**结论：不建议执行。**

---

## 9. 来源清单

**wmux 仓库（绝对路径）**
- `/Users/waterlens/Projects/wmux/internal/sshconfig/sshconfig.go`
- `/Users/waterlens/Projects/wmux/internal/sshconfig/parser.go`
- `/Users/waterlens/Projects/wmux/internal/sshconfig/sshconfig_test.go`
- `/Users/waterlens/Projects/wmux/internal/api/ssh_config.go`
- `/Users/waterlens/Projects/wmux/internal/api/ssh_config_test.go`
- `/Users/waterlens/Projects/wmux/docs/architecture.md:76-84`（"OpenSSH config discovery"）
- `/Users/waterlens/Projects/wmux/docs/reviews/2026-09-04-independent-code-review.md:15,134,155`
- `/Users/waterlens/Projects/wmux/docs/reviews/2026-09-05-overengineering-style/03-store-transcript-security-config-sshx-sshconfig.md:282-337,432-447,510-514,549-551`
- `/Users/waterlens/Projects/wmux/go.mod`

**kevinburke/ssh_config**（2026-09-05 核实）
- https://github.com/kevinburke/ssh_config
- https://api.github.com/repos/kevinburke/ssh_config （`pushed_at` 2026-05-04，`NOASSERTION`，25 open issues，472 stars）
- https://api.github.com/repos/kevinburke/ssh_config/tags （v1.6.0 / v1.5.0 / v1.4.0 / v1.3 / v1.2.0）
- https://raw.githubusercontent.com/kevinburke/ssh_config/master/CHANGELOG.md （v1.5 2026-02-14 加 Match，v1.6 2026-02-16，v1.7 未发布）
- https://raw.githubusercontent.com/kevinburke/ssh_config/master/config.go （`NewInclude` :748-792 贪婪解析；`maxRecurseDepth = 5` :724；`NewPattern` :491-523，`?`→`.?` 在 :508；`Host.Matches` :550-567；`homedir()` :63-70）
- https://raw.githubusercontent.com/kevinburke/ssh_config/master/parser.go （`parseMatch`，`case "exec"` 硬错误、`default:` 硬错误；`strings.Split(val.val, " ")` 的 tab bug）
- https://raw.githubusercontent.com/kevinburke/ssh_config/master/LICENSE
- https://raw.githubusercontent.com/kevinburke/ssh_config/master/go.mod （无 require）
- https://pkg.go.dev/github.com/kevinburke/ssh_config?tab=importedby （526 known importers）

**trzsz/ssh_config**（2026-09-05 核实）
- https://api.github.com/repos/trzsz/ssh_config （fork of kevinburke，`pushed_at` 2026-03-29，4 stars，无 tag）
- https://api.github.com/repos/trzsz/ssh_config/commits （最近提交 "skip unsupported Match blocks for better OpenSSH compatibility"）
- https://raw.githubusercontent.com/trzsz/ssh_config/main/parser.go （`skipUntilNextBlock` :175-196；`case "exec": return p.skipUntilNextBlock` :252-256）
- https://raw.githubusercontent.com/trzsz/ssh_config/main/config.go （`maxRecurseDepth = 5` :806；"parsed greedily" :829）

**k0sproject/rig/v2/sshconfig**（2026-09-05 核实）
- https://pkg.go.dev/github.com/k0sproject/rig/v2/sshconfig
- https://api.github.com/repos/k0sproject/rig （Apache-2.0，`pushed_at` 2026-09-02）
- https://raw.githubusercontent.com/k0sproject/rig/main/sshconfig/parser.go （包文档 "Match exec commands are executed directly"；`defaultExecutor.Run` → `exec.Command` :158-166；`WithExecutor` :224-230；include 注释 :297-298）
- https://raw.githubusercontent.com/k0sproject/rig/main/sshconfig/set.go （`matchesExec` → `s.executor.Run` :1633；`net.LookupHost` :1808 / `net.LookupCNAME` :1812）
- https://raw.githubusercontent.com/k0sproject/rig/main/sshconfig/tree.go （`parseTree` 的 `os.Open(match)`；`circular include directive` 检测；路径穿越守卫）
- https://raw.githubusercontent.com/k0sproject/rig/main/go.mod （7 直接 + ~19 间接依赖）

**其他**
- https://api.github.com/repos/mikkeloscar/sshconfig （GPL-3.0，`pushed_at` 2025-03-13，61 stars）
- https://raw.githubusercontent.com/mikkeloscar/sshconfig/master/parser.go （无 Match、无 `%` token、无否定模式）
- https://api.github.com/repos/shuLhan/share （BSD-3-Clause，描述含 `[Deprecated]`，`pushed_at` 2024-03-09，2 stars）
- https://api.github.com/repos/petems/go-sshconfig （MIT，`pushed_at` 2024-12-01，2 stars）
- https://github.com/tg123/sshpiper （sshd 反向代理，非解析库）
- https://github.com/golang/go/issues/18781 （x/crypto/ssh 长期未提供 ssh_config 解析）
- https://api.osv.dev/v1/query （对上述 4 个 Go 包查询均返回 `{}`，无已知漏洞）

**本地对照**
- `ssh -V` → `OpenSSH_10.3p1, LibreSSL 3.3.6`
- `ssh -G -F <conf> <host>` 用于 §5.3 的三组真值验证
- 实测探针：会话临时目录中的独立 Go module（未保留，未修改 wmux 仓库）
