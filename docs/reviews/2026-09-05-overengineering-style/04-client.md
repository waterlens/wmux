# wmux 前端审查：过度工程与代码风格

审查对象：`client/src/` 全部文件 + `client/index.html`、`client/public/sw.js`、`vite.config.ts`、`tsconfig.*.json`、`eslint.config.js`（约 8.9k 行，含 CSS 2519 行与测试 1608 行）。
对照阅读：`internal/api/types.go`、`internal/api/websocket.go`、`internal/api/sessions.go`、`internal/store/models.go`（仅用于核对前后端协议是否漂移）。

本次**只报过度工程与代码风格**，不涉及正确性、安全、性能。`pnpm lint` 与 `pnpm typecheck` 均通过（0 error / 0 warning），下面所有问题都是工具查不出来的设计问题。

整体判断：这份前端代码的工程完成度明显高于同类个人项目——可访问性、焦点管理、重连状态机、replay 屏障都做得扎实。问题集中在三类：**（a）与后端协议重复维护的一层类型**、**（b）为了让测试能写出来而生造的注入点和断言**、**（c）TerminalView / Workspace 里 state 与 ref 的镜像蔓延**。

---

## 高

### 1. `types.ts` 与 `api.ts` 的 zod schema 是同一份契约的两份手写副本

- 类别：过度工程 / 风格
- 位置：`client/src/types.ts:1-52`、`client/src/types.ts:65-79` ↔ `client/src/api.ts:6-67`

`StatusResponse`/`User`/`Host`/`Session`/`SSHConfigCandidate`/`SSHConfigDiscovery` 六个类型，每一个都在 `types.ts` 手写一遍、在 `api.ts` 用 zod 再写一遍。字段名、可选性、字面量联合全部逐字重复：

```ts
// types.ts:30
export type SessionStatus = 'connecting' | 'running' | 'reconnecting' | 'detached' | 'exited' | 'error';
// api.ts:58
status: z.enum(['connecting', 'running', 'reconnecting', 'detached', 'exited', 'error']),
```

```ts
// types.ts:22
fingerprint?: string | undefined;
// api.ts:25
fingerprint: z.string().optional(),
```

为什么是问题：一份契约两处维护，任何一处漏改就是静默漂移——而且 zod 默认 strip 未知字段，schema 落后于类型时不会报错，只会在运行时把字段吃掉。这不是"看起来严谨"，而是**把编译器能免费提供的一致性保证换成了人工纪律**。

建议：只保留 schema，类型用 `z.infer` 推导。`exactOptionalPropertyTypes: true` 下 zod 4 的 `.optional()` 推出的正是 `field?: T | undefined`，与现有手写类型一致。因为 `api.ts` 已经从 `types.ts` 导入 `HostInput`/`SessionInput`（请求体类型，没有 schema），为避免循环依赖，把 schema 定义搬进 `types.ts`（或新建 `contracts.ts`）：

```ts
// types.ts
export const sessionSchema = z.object({ /* ... */ });
export type Session = z.infer<typeof sessionSchema>;
export type SessionStatus = Session['status'];
```

`api.ts` 只 import schema。这一改能删掉约 45 行重复定义。

---

### 2. `Workspace` 自己维护的 `generations` 计数，与服务端已经下发的 `session.generation` 完全重复

- 类别：过度工程
- 位置：`client/src/components/Workspace.tsx:86`、`:230`、`:384`

```ts
const [generations, setGenerations] = useState<Record<string, number>>({});
// ...
setGenerations((current) => ({ ...current, [session.id]: (current[session.id] ?? 0) + 1 }));
// ...
key={`${session.id}:${generations[session.id] ?? 0}`}
```

这个 state 唯一的作用是重启后强制 remount `TerminalView`。但后端在 `internal/api/sessions.go:196-200` 的 `restartSession` 里已经 `BeginSessionRestart` 递增了 generation，并在同一个响应里回传（`internal/store/models.go:13` `Generation int \`json:"generation"\``，无 omitempty，必定下发）；客户端 `restartSession` 拿到的 `updated` 就带着新 generation，紧接着 `updateSession(updated)` 已经把它写进 `sessions` 了（`Workspace.tsx:228-231`）。

为什么是问题：同一个事实（"这是第几次执行"）在客户端和服务端各存一份，客户端那份还是权威数据的影子。而且 `Session.generation` 在 `types.ts:44` / `api.ts:59` 定义了、在 `api.test.ts:203-207` 专门测了，却在 UI 里一次都没读过——说明当初想到了但没接上。

建议：删掉 `generations` state、`setGenerations` 调用，直接用服务端字段：

```tsx
key={`${session.id}:${session.generation ?? 0}`}
```

顺带获得一个能力：别的设备重启会话时，5 秒轮询会带回新 generation，本端自动 remount（目前只能靠 socket 的 `disconnect reason=restarted` 分支绕回来）。

---

### 3. `TerminalView.tsx` 765 行里有 6 组 state/ref 镜像，连接层和视图层耦死在一个函数体里

- 类别：过度工程 / 风格
- 位置：`client/src/components/TerminalView.tsx:91-125`（ref/state 声明与两个镜像 effect）、`:250-480`（230 行的连接 effect）

组件体里同时存在这些成对变量：

| ref | state | 位置 |
| --- | --- | --- |
| `writerRef` | `writer` | 101 / 111 |
| `ctrlRef` | `ctrl` | 102 / 112 |
| `altRef` | `alt` | 103 / 113 |
| `liveStatusRef` | `liveStatus` | 100 / 110 |
| `activeRef` | prop `active` | 99（+ effect 119-121） |
| `preferencesRef` | prop `preferences` | 108（+ effect 123-125） |

外加 4 个"命令式句柄"ref：`sendInputRef`/`sendBinaryRef`/`sendExactInputRef`/`connectRef`（104-107），它们在 effect 里被赋值、在 cleanup 里被重置成 noop（470-473）。

为什么是问题：镜像本身在这个架构下**是必要的**（socket 回调是长生命周期闭包，读不到最新的 state），但需要 6 组镜像这件事本身就是信号——**连接管理根本不该住在组件函数体里**。目前 `connect()`、`resetReplay()`、`finishReplay()`、`canSendInput()`、`sendFrames()`、`clearOneShotModifiers()` 全部定义在一个 useEffect 内部（254-465），它们既读 ref 也调 setState，无法单独阅读也无法单独测试——这正是 `TerminalView.test.tsx` 需要 170 行假 WebSocket / 假 xterm 脚手架的根因。

建议（**不建议**按 UI 区块拆组件，那只会增加 prop drilling）：把传输层抽成一个不依赖 React 的类，放进 `terminalConnection.ts`：

```ts
export class TerminalConnection {
  constructor(
    private sessionId: string,
    private sink: {
      onOutput(data: Uint8Array, isReplay: boolean, done?: () => void): void;
      onStatus(status: LiveStatus): void;
      onWriter(writer: boolean | null): void;
      onError(message: string): void;
      onReplayEnd(): void;
    },
  ) {}
  connect(): void {}
  send(text: string): void {}
  sendBinary(value: string): void {}
  resize(cols: number, rows: number): void {}
  takeControl(): void {}
  close(): void {}
}
```

组件里只剩 `const connection = useRef<TerminalConnection>()` 和一组 setState 回调，`liveStatusRef`/`writerRef`/`sendXxxRef`/`connectRef` 全部消失（连接自己持有这些状态），`ctrlRef`/`altRef` 可以留在组件里通过 `connection.send(applyTerminalModifiers(...))` 传入。组件体应能降到 350 行左右，测试也能直接对 `TerminalConnection` 断言而不必渲染组件。

这条工作量最大，如果不做，至少应把 254-465 那 210 行搬出 useEffect（保留在模块作用域，用参数传 ref），让 effect 只剩生命周期编排。

---

### 4. 单元测试用正则匹配 `styles.css` 源码文本，甚至断言 CSS 规则的**源码顺序**

- 类别：风格 / 过度工程
- 位置：`client/src/components/Sidebar.test.tsx:9`、`:101-106`；`client/src/components/TerminalView.test.tsx:11`、`:285-288`

```ts
const styles = readFileSync('client/src/styles.css', 'utf8');
// ...
const componentRule = styles.indexOf('.icon-button,');
const hiddenMobileRule = styles.indexOf('.mobile-only {');
expect(hiddenMobileRule).toBeGreaterThan(componentRule);
```

```ts
expect(styles).toMatch(
  /\.terminate-session-button:hover\s*\{[^}]*background:\s*var\(--danger-soft\)[^}]*color:\s*var\(--danger-hover\)/s,
);
```

为什么是问题：三重问题叠加。(1) 断言的是**样式表源码的字符串形态**，不是任何用户可见行为——把 `background` 和 `color` 换个书写顺序就红，抽出一个 CSS 变量也红。(2) `indexOf` 比较把 CSS 的**书写顺序**冻结成契约，任何一次 CSS 重排都会误报，而 jsdom 根本没加载这份样式表，顺序对渲染没有任何影响。(3) `readFileSync('client/src/styles.css')` 用的是相对 cwd 的路径，测试只能从仓库根目录跑。这些断言属于视觉设计范畴，用单测的形式伪装成行为测试，是"为了看起来严谨"而付出的纯负债。

建议：删除 `Sidebar.test.tsx:96-107` 整个用例和 `TerminalView.test.tsx:285-288`，连同两个文件顶部的 `readFileSync`/`node:fs` 导入。`desktop-only`/`mobile-only` 的类名归属已经由同一用例的 `classList.contains` 断言覆盖（`Sidebar.test.tsx:98-99`），那部分留下即可。真要防止样式回归，属于 `scripts/browser-smoke.mjs` 的职责。

---

### 5. 在 `setState` updater 内部调用另外两个 `setState`

- 类别：风格
- 位置：`client/src/components/Workspace.tsx:151-156`（轮询）、`:188-200`（`closeTab`）

```ts
setOpenIds((current) => {
  const index = current.indexOf(id);
  const next = current.filter((item) => item !== id);
  setActiveId((active) => {           // ← 在 updater 里派发另一个 setState
    if (active !== id) return active;
    const replacement = next[Math.min(index, next.length - 1)] ?? null;
    if (!replacement) setCurrentView('home');   // ← 再套一层
    return replacement;
  });
  return next;
});
```

为什么是问题：React 明确要求 updater 是纯函数；StrictMode 开发模式会重复调用 updater 以暴露副作用，这里的三层嵌套刚好是它要抓的模式。`main.tsx:17` 确实开了 `<StrictMode>`。它现在没出错只是因为内层 setState 恰好幂等，但这是运气不是设计，而且这段代码需要读三遍才能看懂控制流。

建议：`closeTab` 不需要 `useCallback([])`（它只传给同文件里的普通 `<button onClick>`，没有 memo 化的消费者），直接读 state 即可：

```ts
function closeTab(id: string) {
  const index = openIds.indexOf(id);
  const next = openIds.filter((item) => item !== id);
  setOpenIds(next);
  if (activeId !== id) return;
  const replacement = next[Math.min(index, next.length - 1)] ?? null;
  setActiveId(replacement);
  if (!replacement) setCurrentView('home');
}
```

轮询回调（141-160）同理：`nextSessions` 已经在手上，直接用 `openIds`/`activeId` 的当前值算出 `next`，三个 setState 平铺即可（该回调本来就在 `await` 之后，读到的是 effect 挂载时的闭包值——这一点用 `sessionsRef` 或把 effect 依赖调整一下更明确，但至少别嵌套 updater）。

---

## 中

### 6. `terminalFonts.ts` 的两层依赖注入参数只为让测试能写出来而存在

- 类别：过度工程
- 位置：`client/src/terminalFonts.ts:6-32`

```ts
type FontLoader = (families: (string | FontFace)[]) => Promise<FontFace[]>;
export async function resolveTerminalFontFamily(loader: FontLoader = loadFonts): Promise<string>
export async function openTerminalAfterFonts(
  open: (fontFamily: string) => void,
  fit: () => void,
  isCancelled: () => boolean,
  loadFamily: () => Promise<string> = resolveTerminalFontFamily,
): Promise<boolean>
```

已核实（`grep -rn` 全仓库）：`loader` 参数只有 `terminalFonts.test.ts:32` 传过；`loadFamily` 参数只有 `terminalFonts.test.ts:16,47` 传过；`openTerminalAfterFonts` 的 `Promise<boolean>` 返回值在生产代码里被 `void` 丢弃（`TerminalView.tsx:195`），只有测试断言它。`TERMINAL_WEB_FONT_FAMILIES`（第 3 行）导出后只在本模块内部使用。

为什么是问题：`openTerminalAfterFonts` 的函数体是 6 行，其中 4 行是把三个回调按顺序调一遍——它没有封装任何领域知识，只是把 `TerminalView` 的一段 `.then()` 挪了个位置，然后为了能被单测再开两个注入口。更讽刺的是 `TerminalView.test.tsx:109` 已经用 `vi.mock('@xterm/addon-web-fonts')` 直接替换了 `loadFonts`——同一件事有了两套 mock 机制。

建议：删掉 `openTerminalAfterFonts` 和它的整个测试文件的第 1、3 个用例，`loader` 参数也一并去掉；`resolveTerminalFontFamily` 保留（它有真正的逻辑：per-family `allSettled` 降级），`TERMINAL_WEB_FONT_FAMILIES` 改为非 export。调用点内联成：

```ts
void resolveTerminalFontFamily().then((fontFamily) => {
  if (disposed) return;
  terminal.options.fontFamily = fontFamily;
  // ... open / addons / observers
  fit();
  if (activeRef.current) terminal.focus();
});
```

`terminalFonts.test.ts` 只保留 "falls back per family" 那个用例（它测的是真逻辑）。

---

### 7. `UI.tsx` 的 `hasContent()`：递归遍历 Fragment，只为兼容一处可以在源头修掉的 `''`

- 类别：过度工程
- 位置：`client/src/components/UI.tsx:99-106`，消费点 `:241-242`；被 `UI.test.tsx:12-37` 专门测试

```ts
function hasContent(content: ReactNode): boolean {
  return Children.toArray(content).some((child) => {
    if (isValidElement<{ children?: ReactNode }>(child) && child.type === Fragment) {
      return hasContent(child.props.children);
    }
    return child !== '';
  });
}
```

为什么是问题：这个递归的存在有一个具体原因——`ConfirmDialog.tsx:390` 写的是 `{error && (<div .../>)}`，当 `error` 是 `''` 时表达式求值为 `''`（不是 `false`），`Children.toArray` 不会把空字符串滤掉，于是 `.modal__body` 会渲染成一个空盒子。但**递归进 Fragment 那一支在生产代码里永远走不到**：全部 7 个 `<Modal>` 调用点的 `footer` Fragment 都恒定含有内容（`ConfirmDialog:379-388`、`SessionDialog:73-82,216-225`、`HostManager:268-277,378-387`、`SSHConfigImport:71`），能构造出空 Fragment 的只有 `UI.test.tsx:19` 这一个测试自己。也就是说：一个通用递归 + 一个专门测试，是为了合理化对一个 `''` 的处理。

建议：在源头去掉 `''`，然后这个函数就消失了：

```tsx
// ConfirmDialog / HostManager 里
{error ? <div className="form-error" role="alert">{error}</div> : null}
```

```tsx
// UI.tsx
{Children.count(children) > 0 && <div className="modal__body">{children}</div>}
{Children.count(footer) > 0 && <footer className="modal__footer">{footer}</footer>}
```

同时删除 `UI.test.tsx:18-28`（专测空 Fragment 的那两次 rerender），保留 12-17 和 30-36。

---

### 8. `parseControlMessage` / `normalizeLiveStatus` 里为服务端从不发送的字段和状态写的兼容分支

- 类别：过度工程 / 死代码
- 位置：`client/src/terminalProtocol.ts:187-194`、`:163-176`

```ts
const writer =
  typeof record.writer === 'boolean' ? record.writer
  : typeof record.isWriter === 'boolean' ? record.isWriter
  : typeof record.writable === 'boolean' ? record.writable
  : undefined;
```

已核实：`internal/api/websocket.go:41` 的 `socketEvent` 只有 `Writer *bool \`json:"writer,omitempty"\``，全仓库没有 `isWriter` / `writable` 的 JSON 输出。同理 `normalizeLiveStatus:173-174` 把 `'disconnected'`/`'terminated'`/`'stopped'` 映射为 `reconnecting`/`exited`，但 `internal/api/websocket.go:337-351` 的 `publicTerminalState` 只可能返回 `connecting`/`running`/`reconnecting`/`error`/`exited`——三个 legacy 值一个都发不出来（`'detached'` 也发不出来，它只存在于 REST 的 `store.SessionStatusDetached`）。

为什么是问题：前后端**同仓同版本发布**（Go 二进制内嵌 `internal/webui/dist`），不存在需要兼容的旧服务端。这些分支是为不可能发生的情形写的防御代码，还被 `terminalProtocol.test.ts:108,115,101-102` 固化成"契约"，让人以为真有别名协议。

建议：`writer` 简化为 `typeof record.writer === 'boolean' ? record.writer : undefined`；`normalizeLiveStatus` 删掉 173-174 两行（`'detached'` 可留可删，留着不花成本）。相应删掉 `terminalProtocol.test.ts:115` 和 `:101-102` 的两条断言，`:108` 里的 `"isWriter":false` 改成 `"writer":false`。

---

### 9. `LiveStatus` 的 `'offline'` 分支永远走不到

- 类别：死代码
- 位置：`client/src/terminalProtocol.ts:1`、`client/src/components/TerminalView.tsx:79`、`:705`

`updateStatus()` 的全部 6 个调用点（`TerminalView.tsx:310,341,355,401,428,440`）只可能传入 `'connecting'`/`'reconnecting'`/`'error'` 或 `normalizeLiveStatus()` 的返回值，而后者永远不返回 `'offline'`。于是 `statusText.offline = '已断开'`（79 行）和 `liveStatus === 'offline'`（705 行）都是死分支。

建议：从 `LiveStatus` 联合里删掉 `'offline'`，删掉这两处；TypeScript 会自动确认没有遗漏。

---

### 10. `Session` 类型/schema 里 5 个字段从不读取，`schemas` 导出只为一个测试而存在

- 类别：过度工程 / 死代码
- 位置：`client/src/types.ts:41-51`、`client/src/api.ts:56-66`、`client/src/api.ts:219-226`、`client/src/api.test.ts:203-207`

`grep -rn` 全仓库确认：`session.backendName`、`session.exitCode`、`session.error`、`session.generation`、`session.cols`、`session.rows` 在任何 `.tsx` 里都没有读取点（只有 test fixture 里在填值）。其中 `backendName` **服务端明确擦掉了**：

```go
// internal/api/sessions.go
func publicSession(session store.Session) store.Session {
    session.BackendName = ""   // omitempty → 永远不下发
```

所以客户端为一个保证不存在的字段维护了类型 + schema 两处定义。

更值得删的是 `api.ts:219-226`：

```ts
export const schemas = { statusSchema, userSchema, hostSchema, sshConfigCandidateSchema, sshConfigDiscoverySchema, sessionSchema };
```

这个导出的**唯一**消费者是 `api.test.ts:204-206`，测的又恰好是从没被读过的 `generation` 字段的整数性。也就是：为了测一个没人用的字段，把 6 个内部 schema 全部公开了。

建议：
- 删掉 `backendName`（服务端保证不发）。
- `exitCode` / `error` / `cols` / `rows` / `generation`：若采纳第 2 条（用 `generation` 做 remount key），`generation` 就活了；其余四个如果短期没有 UI 计划，一并删掉——保留它们不会带来"契约完整性"，zod 本来就 strip 未知字段。
- 删掉 `schemas` 导出和 `api.test.ts:203-207` 这个用例（`sessionFixture` 也就跟着不需要了）。若采纳第 1 条改用 `z.infer`，schema 本来就要 export，这条自然消解。

---

### 11. `Workspace` 里 9 个 `useCallback` / 2 个 `useMemo`，其中只有 1 个是必需的

- 类别：过度工程
- 位置：`client/src/components/Workspace.tsx:90,96,173,181,188,202,206,212,222,247`

`notify`（90）**必须**稳定：它进了 `TerminalView` 那个 230 行连接 effect 的依赖数组（`TerminalView.tsx:480`），不稳定就会每次渲染重连 WebSocket。这一点值得加注释。

其余全部没有价值：`Sidebar`、`HostManager`、`TerminalView`、`SessionDialog`、`SettingsDialog` 没有一个是 `React.memo` 包装的；`TerminalView` 的其他 props（`onRestart`/`onTerminate`/`restarting`）都不在任何依赖数组里。`openSessions` 的 `useMemo`（173-179）产出的数组只在 JSX 里遍历一次，缓存它不省任何东西，反而让人误以为它很贵。

为什么是问题：这些 `useCallback` 互相依赖形成了一条自我维持的链（`handleCreated` 依赖 `openSession`、`restartSession` 依赖 `openSession`+`updateSession`、`requestRestart` 依赖 `restartSession`），一旦要改其中一个就得追整条链的依赖数组——这是纯粹的记账成本，换来的收益是零。

建议：只保留 `notify` 的 `useCallback` 并注明原因；`dismissToast`、`openSession`、`closeTab`、`updateSession`、`createSession`、`handleCreated`、`requestRestart`、`openSessions`、`restartSession` 全部退化为普通函数/普通表达式。`Sidebar.tsx:68-90` 的两个 `useMemo`（`filtered`/`groups`）和 `SessionDialog.tsx:27-28` 的 `selectedHost`/`trustedHosts` 同理——后者尤其讽刺：一次 `hosts.find()` 被 memo 了，而每次渲染都跑 while 循环扫全部会话名的 `availableDefaultName()`（`SessionDialog.tsx:30-37`，在 `:137` 作为 placeholder 调用）反倒没有。

---

### 12. 对话框的挂载约定在同一个仓库里有两套，且互相冲突

- 类别：风格
- 位置：`Workspace.tsx:409-421,444-457`、`HostManager.tsx:233-249` vs `Workspace.tsx:425-443`、`HostManager.tsx:251-294`

两种写法并存：

```tsx
{newSessionOpen && <SessionDialog open ... />}      // 条件挂载 + 恒为 true 的 open
{settingsOpen && <SettingsDialog open ... />}
{editorOpen && <HostEditor open ... />}

<ConfirmDialog open={Boolean(restartTarget)} ... />  // 常驻 + open 控制
<Modal open={Boolean(probe)} ... />
```

第一种里的 `open` prop 恒等于 `true`，是纯噪音；两种写法的卸载语义还不一样（前者卸载后表单 state 清空，后者保留），但代码里看不出这是有意选择。

最矛盾的是 `RenameSessionDialog`：

```tsx
// Workspace.tsx:422-424
{renameTarget && <RenameSessionDialog session={renameTarget} ... />}
// SessionDialog.tsx:184
type RenameProps = { session: Session | null; ... };
// SessionDialog.tsx:190, 196, 211
const [name, setName] = useState(session?.name ?? '');
if (!session || !name.trim()) return;
<Modal open={Boolean(session)} ...>
```

调用点保证了 `session` 非 null，组件却按 nullable 写了三处防御。

建议：统一为"条件挂载 + 组件内部不再有 `open` prop"（表单类对话框）和"常驻 + `open`"（确认框，因为要保留关闭动画/焦点归还）二选一，并在 `Modal` 的注释里写清楚。`RenameSessionDialog` 的 prop 收紧为 `session: Session`，删掉 190/196/211 三处 null 处理。

---

### 13. `AppState` 存整份 `StatusResponse`，但其中两个字段只写不读，还要为它伪造对象

- 类别：过度工程 / 风格
- 位置：`client/src/App.tsx:10-15`、`:45-53`、`:89`

```ts
type AppState =
  | { phase: 'setup'; status: StatusResponse }
  | { phase: 'login'; status: StatusResponse }
  | { phase: 'workspace'; status: StatusResponse; user: User; hosts: Host[]; sessions: Session[] }
  | ...;
```

`state.status` 的全部读取点是 `:76`、`:78`、`:85`（`.version`）、`:86`（`.commit`）。`authenticated` 与 `setupRequired` 存进 state 之后**从未被读过**，却要在两处被重新写一遍：

```ts
status: 'status' in current
  ? { ...current.status, authenticated: false, setupRequired: false }
  : { authenticated: false, setupRequired: false, version: '' },   // 伪造一个空 status
```

```ts
setState({ phase: 'login', status: { ...state.status, authenticated: false } });
```

为什么是问题：为了让类型对齐而伪造一个 `version: ''` 的假响应，读者会误以为这些字段有意义。`'status' in current` 这种基于属性存在性的窄化，也比 phase 判别式更难读。

建议：state 只存真正需要的东西：

```ts
type AppState =
  | { phase: 'loading' }
  | { phase: 'setup' | 'login'; version: string }
  | { phase: 'workspace'; version: string; commit?: string | undefined; user: User; hosts: Host[]; sessions: Session[] }
  | { phase: 'error'; message: string };
```

`authExpired` 变成 `setState((current) => ({ phase: 'login', version: 'version' in current ? current.version : '' }))`；`setup`/`login` 两个分支还能合并，`:75-78` 也随之从两个 `else if` 收成一个。

---

### 14. `notify` 的 tone 联合类型在三个组件里各手抄了一遍

- 类别：风格
- 位置：`client/src/components/TerminalView.tsx:50`、`client/src/components/HostManager.tsx:27`、`client/src/components/SSHConfigImport.tsx:9`，权威定义在 `client/src/types.ts:98-102`

```ts
notify: (message: string, tone?: 'success' | 'error' | 'info') => void;   // ×3
```

而 `Workspace.tsx:90` 自己用的是正确写法 `tone: Toast['tone'] = 'info'`。四处，三种写法（其中三处还漏了默认值语义）。

建议：在 `types.ts` 加一行，四处统一引用：

```ts
export type Notify = (message: string, tone?: Toast['tone']) => void;
```

---

### 15. `styles.css` 的分区与实际 DOM 结构脱节：`.manager-view` 的定位规则被 350 行外的另一条完全推翻

- 类别：风格
- 位置：`client/src/styles.css:1137-1145`、`:1146-1150`、`:1492-1496`、`:1497-1502`

```css
/* 1137 —— "Tabs and dashboard" 区 */
.dashboard,
.manager-view { position: absolute; inset: 0; overflow-y: auto; overscroll-behavior: contain; padding: var(--space-6); background: var(--surface); }
.dashboard { display: grid; align-content: start; gap: var(--space-6); }

/* 1492 —— "Host management" 区，355 行之后 */
.manager-view { display: grid; align-content: start; gap: var(--space-6); }   /* 与 1146 逐字重复 */
.workspace-main > .manager-view { position: relative; inset: auto; min-height: 0; flex: 1; }  /* 推翻 1139 */
```

`HostManager` 在 `Workspace.tsx:399-406` **永远**渲染为 `.workspace-main` 的直接子元素，所以 1139 行给 `.manager-view` 的 `position: absolute; inset: 0` 是 100% 死代码，1492 那三行又和 1146 完全重复。

同一个文件里 `.about-*` 也被切成两段：1937-1953（`.about-mark`/`.about-meta`）→ 1954-1992（`.password-change`/`.settings-account`）→ 1993-2040（`.about-mark` 又来一次 + `.about-stats` + `.about-meta` 又来一次）。`.host-avatar` 同样分散在 1229 和 1523。

为什么是问题：2519 行单文件本身对这个项目规模是**合理**的，但按 "组件区块 + 注释分节" 组织的前提是每个选择器只属于一个区块。目前的状态是：改一处样式要先 grep 确认没有第二处，而且有一处已经在跟自己打架。

建议：
- 把 `.manager-view` 从 1138 的选择器组里移除；删掉 1492-1496（与 1146 重复）；`.workspace-main > .manager-view` 简化为 `.manager-view { display:grid; align-content:start; gap:var(--space-6); min-height:0; flex:1; overflow-y:auto; overscroll-behavior:contain; padding:var(--space-6); background:var(--surface) }`。
- 把 1993-2040 的 `.about-*` 并回 1937-1953，让 Settings 区内部按 `settings-layout → settings-nav → setting-row → switch → password-change → about-*` 的顺序排一次。

---

### 16. 终端浮层里的硬编码十六进制颜色，与全文件的 token 化风格不一致

- 类别：风格
- 位置：`client/src/styles.css:1426-1441`（`.read-only-banner`）、`:1452-1467`（`.terminal-state-card`）、`:1476-1484`（`.terminal-loader`）、`:233,236`（`.button--danger`）

```css
.read-only-banner { border: 1px solid #5c6670; background: rgb(20 25 28 / 94%); color: #f4f7f8; }
.read-only-banner button { border: 1px solid #83909a; background: #303940; color: #ffffff; }
.terminal-state-card { border: 1px solid #465057; background: rgb(21 26 29 / 96%); color: #f3f6f7; }
.terminal-state-card p { color: #b9c2c7; }
.terminal-state-card__icon { color: #ffb762; }
.terminal-loader { color: #b7c0c5; }
.terminal-loader > span { border: 2px solid #4d575e; border-top-color: #ffffff; }
```

全文件其余 2400 行严格使用 `var(--*)`，只有这 12 个值是裸色。

需要说明：**这些浮层浮在恒为黑的终端画布上（`--terminal-bg: #000000`），不随主题切换是正确的设计**，问题不在于"不用主题色"，而在于"不用变量"。现在改一处浮层色调要在三个规则块里手动配平七个近似灰。

建议：在 `:root` 里补一组终端专用 token（不进 `[data-theme='dark']` 块，从而保持恒定），例如：

```css
--terminal-overlay-bg: rgb(20 25 28 / 94%);
--terminal-overlay-border: #5c6670;
--terminal-overlay-text: #f4f7f8;
--terminal-overlay-muted: #b9c2c7;
--terminal-overlay-warn: #ffb762;
```

`.button--danger` 的 `color: #ffffff` / `:root[data-theme='dark'] .button--danger { color: #2b0907 }`（233/236）也应当变成 `--danger-contrast` token，与 `--accent-contrast` 的既有模式对齐。

---

### 17. `HostEditor` 构造 credentials 的三层嵌套三元，其中一支是恒真表达式

- 类别：风格
- 位置：`client/src/components/HostManager.tsx:348-359`

```ts
const credentials =
  authType === 'password'
    ? password ? { password } : {}
    : authType === 'privateKey'
      ? {
          ...(privateKey.trim() ? { privateKey: privateKey.trim() } : {}),
          ...(privateKey.trim() ? { passphrase } : passphrase ? { passphrase } : {}),
        }
      : {};
const input = { ...base, ...credentials };
```

第 356 行 `privateKey.trim() ? { passphrase } : passphrase ? { passphrase } : {}` 等价于 `(privateKey.trim() || passphrase) ? { passphrase } : {}`——同一个结果写了两遍分支。整段读起来需要在脑子里展开三层。

建议：改成早返回式的构造：

```ts
const input: Partial<HostInput> & HostInput = { ...base };
if (authType === 'password' && password) input.password = password;
if (authType === 'privateKey') {
  const key = privateKey.trim();
  if (key) input.privateKey = key;
  if (key || passphrase) input.passphrase = passphrase;
}
```

（`exactOptionalPropertyTypes` 下条件赋值比条件展开更直白。）

---

### 18. `TerminalView.test.tsx` 有两个用例只在验证 mock 自身的行为

- 类别：过度工程
- 位置：`client/src/components/TerminalView.test.tsx:79-94`（`FakeWebglAddon`）、`:520-533`、`:535-559`

```ts
it('uses the WebGL renderer when available and drops it when the context is lost', async () => {
  // ...
  addon.contextLoss?.();
  expect(addon.dispose).toHaveBeenCalledOnce();
});
```

`FakeWebglAddon` 是测试自己写的类，`contextLoss` 是测试自己保存的回调，`dispose` 是测试自己造的 spy。被测代码 `attachWebglRenderer`（`TerminalView.tsx:53-63`）只有 8 行且全是三方 API 调用。第二个用例 "falls back to the DOM renderer" 里真正有价值的部分（WebGL 挂载失败后输入仍可发送）与 `:325-378` 那个用例完全重叠。

为什么是问题：170 行脚手架（`:15-97` 的 xterm 假类族 + `:119-173` 的假 WebSocket/ResizeObserver）本身是必要恶——因为 TerminalView 把传输层和渲染层焊在一起（见第 3 条）。但 `FakeWebglAddon` 这一支是**纯增量负担**：它只服务于两个自证的用例。

建议：删掉 `:79-94` 的 `FakeWebglAddon`、`:114` 的 `vi.mock('@xterm/addon-webgl')`、`:226-227` 的 reset、以及 `:520-533` 和 `:535-559` 两个用例。文件从 581 行降到约 480 行。若采纳第 3 条把连接层抽出，这个文件还能再瘦一半。

---

### 19. 一批测试断言的是"某段已被删除的旧文案不存在"，而不是任何现有行为

- 类别：过度工程
- 位置：`HostManager.test.tsx:73-74,78-80`、`Workspace.test.tsx:106-107`、`UI.test.tsx:236`、`SessionDialog.test.tsx:29-31`、`Sidebar.test.tsx:83-84`

```ts
expect(screen.queryByText('主机密钥校验始终开启')).toBeNull();
expect(screen.queryByText(/不会静默接受新指纹/)).toBeNull();
expect(screen.queryByText('凭据由 wmux 服务加密保管。')).toBeNull();
expect(screen.queryByText('支持 OpenSSH PEM 格式')).toBeNull();
expect(screen.queryByText(/保存后还需要探测并确认主机指纹/)).toBeNull();
expect(screen.queryByText('这个操作无法撤销。')).toBeNull();
expect(screen.queryByText(/若只想隐藏终端/)).toBeNull();
expect(screen.queryByText('进程将在浏览器关闭后继续运行。')).toBeNull();
```

`grep -rn` 确认：这些字符串在 `client/src` 里**不存在于任何非测试文件**。它们记录的是某一次"文案精简"评审的决定，不是组件契约。`Sidebar.test.tsx:83-84` 的 `queryByText('2')` / `queryByText('1')` 更极端——断言页面上不出现字符 "2" 和 "1"，任何新增的数字（比如端口、行数）都会误伤。

为什么是问题：这类断言即使把组件整个删掉也会通过；它们只增加读测试的噪音和改文案时的假阴性风险。用例名（"keeps create/edit chrome concise while preserving field validation"）描述的是评审意图而非行为，进一步放大了误导。

建议：删掉上述纯否定断言，保留同一用例里的正向断言（`HostManager.test.tsx:77,82-83`、`Workspace.test.tsx:104-105`、`SessionDialog.test.tsx:32,35-36` 等）。用例名改成描述行为，例如 `HostManager.test.tsx:70` → `it('validates required fields before submitting')`。`SessionDialog.test.tsx:26-47` 精简后就是"persistence=none 时显示提示、其它值不显示"，四组重复的三重断言可以压成两组。

---

## 低

### 20. `encodeTerminalTextFrames` / `encodeTerminalBinaryFrames` 的 `maxFrameBytes` 参数与它的防御性校验从未被使用

- 类别：过度工程
- 位置：`client/src/terminalProtocol.ts:26-31`、`:41-42`、`:62-63`

`grep -rn` 确认三个调用点（`TerminalView.tsx:458,461,464`）和全部测试都用默认值。`assertUsableFrameSize` 是对一个只有一种取值的常量做的运行时校验；第 53 行 `if (!read) throw new Error('Unable to encode...')` 在该守卫之后同样不可达。

建议：删掉参数、`assertUsableFrameSize`、第 53 行的 throw，函数体直接用 `MAX_INPUT_FRAME_BYTES - 1`。

### 21. `api.ts` 的三处小噪音

- 类别：过度工程 / 风格
- 位置：`client/src/api.ts:146-148`、`:107-112`、`:92,99`

1. `function body(value: unknown): string { return JSON.stringify(value); }` —— 给内置函数换个名字，8 处调用没有一处因此更清晰。删掉，直接 `JSON.stringify(...)`。
2. `request(path, schema, init, redirectOnUnauthorized = true)` —— 第 4 个位置布尔参数，调用点是 `request('/api/setup', userSchema, {...}, false)`，读者必须回到定义才知道 `false` 是什么。改成 `{ silent401?: boolean }` 选项对象或单独一个 `requestPublic()`。
3. `ApiError.details` 在 `:99` 赋值、`:136,141` 传入，全仓库**没有任何读取点**。要么删，要么在 `errorMessage`/开发模式日志里真的用起来。

### 22. `Modal` 里的 `symbol` 身份标识和两份 focusable 选择器字符串

- 类别：过度工程 / 风格
- 位置：`client/src/components/UI.tsx:87`、`:134`、`:211`；`:111` 与 `:184-186`

`modalStack` 的元素是 `{ instance: symbol; layer: HTMLDivElement }`，而除了 211 行以外的全部比较用的都是 entry 的对象身份（`modalStack.at(-1) !== entry`）。211 行的 `modalStack.at(-1)?.instance === instanceRef.current` 可以直接写成 `modalStack.at(-1)?.layer === layerRef.current`，然后 `instance` 字段和 `instanceRef` 一起删掉。

另外 `focusableElements()`（111 行）和自动聚焦查询（184-186 行）是两条各自硬编码、内容不同的选择器串，改可聚焦元素规则时容易只改一处。至少把公共部分提成一个模块级常量。

### 23. 只为测试而存在的可选 prop 默认值

- 类别：过度工程
- 位置：`client/src/components/Sidebar.tsx:22`、`:38`、`:58`

```ts
const EMPTY_RESTARTING: ReadonlySet<string> = new Set();
// ...
restartingIds?: ReadonlySet<string> | undefined;
restartingIds = EMPTY_RESTARTING,
```

唯一的生产调用点 `Workspace.tsx:309` 总是传值；唯一不传的是 `Sidebar.test.tsx:57-76`。

建议：prop 改为必填，测试里传 `restartingIds={new Set()}`，删掉模块级常量。

### 24. `preferences.ts` 的职责混装，`loadPreferences` 校验不一致且魔数跨文件重复

- 类别：风格
- 位置：`client/src/preferences.ts:1-13`；`client/src/components/Workspace.tsx:27-43`；`client/src/components/SettingsDialog.tsx:139-140`、`:212-215`

1. 13 行的 `preferences.ts` 里塞了 `DEFAULT_PREFERENCES` 和一个跟偏好设置毫无关系的 `isMobileLayout()`。后者应归入 `Workspace.tsx` 或一个 `layout.ts`。
2. `loadPreferences` 的四个字段校验方式各不相同：`fontSize` 夹取 11–22，`cursorStyle`/`theme` 白名单但**回退值硬编码为 `'block'`/`'light'` 而非 `DEFAULT_PREFERENCES.cursorStyle`/`.theme`**，`cursorBlink` 只查类型，`scrollback` 完全不校验（任意 number 都接受，包括负数）。
3. `11`/`22` 同时出现在 `Workspace.tsx:33` 和 `SettingsDialog.tsx:139-140` 的 `min`/`max`；`2000/10000/25000/50000` 同时是 `SettingsDialog.tsx:212-215` 的 option 值和 `DEFAULT_PREFERENCES.scrollback` 的邻居。

建议：把范围与允许值收进 `preferences.ts` 一处导出（`FONT_SIZE_RANGE`、`SCROLLBACK_OPTIONS`），`loadPreferences` 统一走 `DEFAULT_PREFERENCES` 回退并给 `scrollback` 加白名单，`SettingsDialog` 从常量渲染 `<option>`。

### 25. `TerminalView` 里 4 处无注释的 `setTimeout(fit, …)` 与一对近乎重复的清修饰键函数

- 类别：风格
- 位置：`TerminalView.tsx:238`、`:243-247`、`:265`、`:370`；`:292-301` 与 `:518-523`

四处定时 `fit()` 各有各的理由（偏好变化后重排、切到活动标签后 30ms、replay 结束后、拿到写权限后），但没有一条注释，`30` 是纯魔数。

同时有两个功能几乎相同的函数分居 effect 内外：

```ts
function clearOneShotModifiers() {            // :292，effect 内部
  if (ctrlRef.current) { ctrlRef.current = false; setCtrl(false); }
  if (altRef.current) { altRef.current = false; setAlt(false); }
}
function clearToolbarModifiers() {            // :518，组件体
  ctrlRef.current = false; altRef.current = false; setCtrl(false); setAlt(false);
}
```

建议：合并为组件体里的一个 `clearModifiers()`（无条件版本即可，`setState` 同值不触发重渲染），effect 通过一个 `clearModifiersRef` 或直接调用它（它不依赖 effect 作用域）；四处 `setTimeout(fit, …)` 统一成一个带注释的 `scheduleFit()`，并把 `30` 换成具名常量或直接用 `requestAnimationFrame`。

### 26. 死 CSS 与死 DOM 属性

- 类别：死代码
- 位置（均已 `grep -rn` 全仓库确认）：

| 项 | 位置 | 说明 |
| --- | --- | --- |
| `.user-avatar--large` | `styles.css:1013-1018` | 没有任何 TSX 使用 |
| `.warning-callout` | `styles.css:523,548-552,555` | 没有任何 TSX 使用 |
| `.info-callout` | `styles.css:521,538-542,553` | 只有 `SessionDialog.test.tsx:37` 断言它**不存在** |
| `.host-menu` | `HostManager.tsx:157` 使用 | CSS 里没有任何规则 |
| `.auth-shell--plain` | `AuthScreen.tsx:54` 使用 | CSS 里没有任何规则 |
| `modal-layer--form` / `modal-layer--settings` | `UI.tsx:208` 生成 | CSS 里没有对应规则（只有 `--confirm` 有） |
| `data-terminal-ready` / `data-replay-complete` | `TerminalView.tsx:668-669` | 与 `:609-610` 重复；消费方（测试与 `scripts/browser-smoke.mjs:386,555`）只读 `.terminal-view` 上的那份 |
| `.desktop-only` | `styles.css:2134` | 只有媒体查询内的规则，没有基础规则；而 `.mobile-only` 有基础规则（`:304`）+ 媒体查询规则（`:2137`），两者不对称 |

建议：全部删除。`.desktop-only` 要么补一条 `.desktop-only { display: inline-flex }` 基础规则与 `.mobile-only` 对称，要么在注释里说明"依赖元素默认 display"——现在的状态是靠 `Sidebar.test.tsx:101-106` 的源码顺序断言把这个不对称冻住了（见第 4 条）。

### 27. 构建配置里的两处冗余

- 类别：过度工程
- 位置：`tsconfig.base.json` / `tsconfig.client.json`；`eslint.config.js:34`

1. `tsconfig.base.json` 的唯一消费者是 `tsconfig.client.json`（`grep` 确认），而它设置的 `"module": "NodeNext"` / `"moduleResolution": "NodeNext"` 被 client 立刻覆盖成 `ESNext`/`Bundler`——两行死配置 + 一层没有第二个消费者的继承。建议合并成单个 `tsconfig.json`，等真的出现第二个 TS 工程再拆。
2. `'@typescript-eslint/no-explicit-any': 'error'` 与 `tseslint.configs.recommended` 中的默认值完全相同（已实测：recommended 里该规则就是 `"error"`），重复声明会让人以为这里做了额外收紧。

### 28. PWA：离线导航回退与每小时更新轮询，对这个产品形态收益接近零

- 类别：过度工程
- 位置：`client/public/sw.js:51-64`、`client/src/pwa.ts:22`、`:32-40`

`sw.js` 的 navigate 分支在网络失败时回退到缓存的 `/`，结果是渲染出一个立刻连不上后端的空壳工作台（`App.tsx` 的 `bootstrap` 会走进 `phase: 'error'`）。wmux 是持久化终端，**离线时没有任何可用功能**，这个回退把"浏览器的离线页"换成了"看起来能用但立刻报错的应用"。`pwa.ts:22` 每小时一次的 `registration.update()` 对一个单人自托管实例也没有实际意义。

另外 `activateUpdate`（32-40）每次调用都新增一个 `controllerchange` 监听器，`reloading` 标志是每次调用独立的局部变量，重复点击会叠加监听。

值得保留的是"有新版本 → 提示重载"这条链路（`App.tsx:97-113`），长驻 PWA 标签页确实需要它。

建议：`sw.js` 只保留 SHELL 资源的 stale-while-revalidate（66-75 行）与 `SKIP_WAITING`，删掉 navigate 分支；`pwa.ts` 删掉 `setInterval`（`updatefound` 事件已经覆盖了正常的更新发现路径），`reloading` 提到模块作用域。

### 29. `persistenceLabel` 的参数类型被放宽成 `string`，丧失穷尽性检查

- 类别：风格
- 位置：`client/src/sessionStatus.ts:23-36`，调用点 `Sidebar.tsx:179`

```ts
export function persistenceLabel(value: PersistenceMode | string | undefined): string {
  switch (value) { /* ... */ default: return ''; }
}
```

`| string` 一加，`PersistenceMode` 就形同虚设，`default: return ''` 成了必需的兜底。根因是 `Session.backend` 被声明为 `backend?: string`（`types.ts:41`）而不是 `backend?: PersistenceMode`——服务端 `internal/store/models.go:10` 的 `Backend` 也只可能是那四个值。

建议：`types.ts` 与 `api.ts` 里把 `backend` 收紧为 `PersistenceMode`（`z.enum(['auto','tmux','screen','none']).optional()`），然后 `persistenceLabel(value: PersistenceMode | undefined)`，`default` 分支可以换成 `satisfies never` 式的穷尽检查。

### 30. `ErrorBoundary` 的空 `componentDidCatch`

- 类别：过度工程
- 位置：`client/src/components/ErrorBoundary.tsx:14-16`

```ts
componentDidCatch(): void {
  // Rendering errors are intentionally contained; server logs never receive terminal content.
}
```

`getDerivedStateFromError` 已经足够让边界工作，React 不要求实现 `componentDidCatch`。这是一个只为承载注释而存在的空方法。

建议：删掉方法，把这句注释挪到 class 上方。

---

## 看起来复杂但其实合理，不建议改

以下几处在快速阅读时容易被误判为过度工程，但我核对后认为是必要的：

1. **`ReplayBarrier`（`terminalProtocol.ts:117-161`）连同 `generation` 计数器。** 它解决的是一个真实且不平凡的竞态：xterm 的 `write(data, callback)` 是异步落盘的，必须等所有 replay 写入的回调都排干、且服务端 `replay_end` 也到了，才能放开 stdin；`generation` 用来作废重连前那一批还没回调的写入。这段逻辑值得单独成类、值得那两个单测（`terminalProtocol.test.ts:70-96`）。

2. **`TerminalView` 每个 socket 监听器开头的 `if (socketRef.current !== socket) return;`（`:318,324,393,446`）。** 重连时旧 socket 的事件仍可能到达，这是必要的身份检查，不是防御性冗余。

3. **重连状态机的 `shouldReconnectRef` / `reconnectAttemptsRef` / `reconnectTimerRef` 三件套（`:96-98`）以及退避公式 `Math.min(8000, 500 * 2 ** Math.min(attempts, 4))`。** 这里的每个变量都对应一个真实分支（1008 永久关闭、exited 不重连、手动重连清零计数）。

4. **`fit` 用 `useCallback([])` + `activeRef` 而不是直接读 `active`。** `fit` 被 `ResizeObserver` 捕获（`:209`），必须身份稳定，因此只能通过 ref 读取最新的 `active`。这是必要的镜像（第 3 条建议重构连接层，但这一对应当保留）。

5. **`const [replayBarrier] = useState(() => new ReplayBarrier())`（`:109`）。** 这是 React 官方推荐的稳定实例惰性初始化写法，比 `useRef` + null 检查更干净。

6. **`Workspace` 轮询里的 `requestId` 乱序保护（`:140,143,147`）和 `notify` 的 `useCallback`（`:90`）。** 前者防的是慢请求覆盖新结果（已有对应测试 `Workspace.test.tsx:163-192`），后者是 `TerminalView` 连接 effect 依赖数组的必要条件。

7. **`api.ts` 的 zod 运行时校验本身。** 自托管场景下浏览器缓存的前端与服务端版本可能不一致，`invalid_response` 分支（`:139-142`）是真会触发的。我在第 1、10 条里建议的是消除**重复定义**和删掉**无人读取的字段**，不是取消校验。

8. **`Modal` 的 `modalStack` + `inert` + 焦点归还（`UI.tsx:87-97,143-202`）。** 嵌套对话框在生产里真实存在（`SettingsDialog` → `ConfirmDialog`，`HostManager` → `HostEditor` → `ConfirmDialog`），栈式管理和 `inert` 是必需的；`UI.test.tsx:85-121` 是这套代码里最有价值的测试之一。

9. **`isMobileLayout()` 通过 `--mobile-layout` CSS 变量读断点（`preferences.ts:11-12` + `styles.css:2131-2133`）。** 看起来绕，但它避免了 `780px` 这个断点在 CSS 和 JS 两处各写一遍。这是正确的单一真相源做法（只是函数放错了文件，见第 24 条）。

10. **`attachWebglRenderer` 的动态 `import()` + try/catch（`TerminalView.tsx:53-63`）。** WebGL addon 有 200KB 量级且在无 WebGL2 环境会抛，动态导入 + 静默降级是恰当的。（第 18 条建议删的是它的**测试**，不是它本身。）

11. **`encodeTerminalTextFrames` 用 `encoder.encodeInto` 按码点边界分帧（`terminalProtocol.ts:50-56`）。** 服务端 `maxSocketMessage = 128 << 10`（`internal/api/websocket.go:21`）是硬限制，大段粘贴必须切分且不能切断 UTF-8 码点。逻辑本身必要（要删的只是那个从未使用的 `maxFrameBytes` 参数）。

12. **`Dashboard` 定义在 `Workspace.tsx:463-540` 内。** 单一使用点的私有子组件放在同一文件是合理的，不需要为它新开文件。

13. **`terminal.options.disableStdin` 在四处被同步（`:131,263,272,350`）。** 写权限、replay 状态、连接状态三个正交条件共同决定它，分散赋值确实不优雅，但把它收进 `TerminalConnection`（第 3 条）之后自然会收敛；单独为它加抽象层不划算。
