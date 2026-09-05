# 决策备忘：wmux 是否应把终端组件从 xterm.js 换成 ghostty-web

- **核实日期**：2026-09-05（所有版本、提交、issue 状态均为当日通过 npm registry API / GitHub API / 网页抓取实测）
- **仓库**：`/Users/waterlens/Projects/wmux`，`main` @ `4fb3e9f`
- **结论先行**：**现在不换**。做一次受控实验并持续观察；同时先把 xterm 侧几个便宜的性能/正确性开关做掉。理由见第五、六节。

---

## 一、问题

wmux 前端终端基于 `@xterm/xterm` 6.0.0 及 5 个 addon。是否应改用 ghostty-web（Ghostty 的 VT 核心经 WebAssembly 移植到浏览器）？收益、代价、值不值得。

需要先澄清一个常见混淆——**"ghostty 上网页"现在有三条不同的路线**，它们的成熟度和 API 差异极大：

| 项目 | 维护方 | 定位 | 是否 xterm.js API 兼容 |
|---|---|---|---|
| **`ghostty-web`**（本备忘的主角） | Coder（`coder-bot`，`ash+bot@coder.com`） | 把 libghostty-vt 编译成 wasm，**自带** canvas 渲染器 + 输入层，宣称可 drop-in 替换 xterm.js | 宣称兼容（实际部分兼容，见第四节） |
| **官方 `ghostty-vt.wasm`** | Ghostty 项目（mitchellh） | 只有 VT 核心（解析 + buffer + render state），**不含渲染器/输入层** | 无 API，是 C/wasm ABI |
| **`@wterm/ghostty`** | Vercel Labs | 自研 web 终端框架，可选把 libghostty 当 core | 明确不提供 xterm.js API 兼容 |

wmux 若要"换掉 xterm.js"，唯一能减少工作量的候选是 `coder/ghostty-web`。

---

## 二、wmux 对 xterm.js 的实际依赖清单

来源：`client/src/components/TerminalView.tsx`、`client/src/terminalProtocol.ts`、`client/src/terminalFonts.ts`、`client/src/components/TerminalView.test.tsx`、`scripts/browser-smoke.mjs`、`client/src/styles.css`、`package.json`、`internal/api/middleware.go`。

### 2.1 构造选项（`TerminalView.tsx:156-173`）

```
allowProposedApi: true          allowTransparency: true       convertEol: false
disableStdin: true              drawBoldTextInBrightColors    fontFamily
fontWeight: '400'               fontWeightBold: '600'         letterSpacing: 0
lineHeight: 1.18                macOptionIsMeta: true         minimumContrastRatio: 4.5
rescaleOverlappingGlyphs: true  rightClickSelectsWord: true   scrollOnUserInput: true
```

运行时可变选项（`TerminalView.tsx:205-213`，跟随 `TerminalPreferences`）：`fontSize`(默认 14)、`cursorStyle`(block)、`cursorBlink`(true)、`scrollback`(**10000**)，以及被当作读写门闸反复切换的 `disableStdin`（`TerminalView.tsx:117`、`:259`、`:266`）。

### 2.2 关键 API 语义

- **`write(data, callback)` 的"解析完成"回调**——这是最硬的依赖。`ReplayBarrier`（`terminalProtocol.ts:129-172`）为每个回放帧调 `trackReplayWrite()`，只有当 **`replay_end` 已到 且 全部 pending write 的回调都已触发** 才 `tryOpen()`，然后才解除 `disableStdin` 并允许 `canSendInput()`。这道闸门的目的写在 `docs/architecture.md:58`：防止历史输出里的 DSR/DA 查询在当前 PTY 里生成回复。xterm.js 的契约明确写在 typings 里："data is processed asynchronously… callback fires when the data was processed by the parser"。
- `onData(...)` → `sendInputRef`；`onBinary(...)` → `sendBinaryRef` + `encodeTerminalBinaryFrames`（`terminalProtocol.ts:71-88`，把每个 char code 按 8 bit 截断，语义即"xterm binary event 保留原始字节"，`docs/architecture.md:49` 也如此描述）。
- `terminal.modes.applicationCursorKeysMode`（`TerminalView.tsx:522`）→ 移动端方向键行编码 `encodeCursorKey`。
- `terminal.unicode.activeVersion = '11'`（`TerminalView.tsx:180`，**赋值**，非只读）。
- `focus()`（8 处）、`clear()`、`getSelection()`、`paste()`（走 bracketed paste）、`dispose()`、`cols`/`rows`、`loadAddon()`、`open(mount)`。

### 2.3 Addon

| Addon | 用途 | 代码位置 |
|---|---|---|
| `@xterm/addon-fit` | `fit()` + ResizeObserver 驱动，`fit` 后向 ws 发 `resize` | `TerminalView.tsx:135-152` |
| `@xterm/addon-unicode11` | Unicode 11 宽度表 | `:179-180` |
| `@xterm/addon-web-links` | 自定义 handler，`window.open(uri,'_blank','noopener,noreferrer')` | `:181-185` |
| `@xterm/addon-webgl` | **动态 import**，`onContextLoss` → `dispose()` 回落 DOM 渲染 | `:60-70` |
| `@xterm/addon-web-fonts` | `loadFonts()`，逐族 `allSettled` 加载后才 `terminal.open()`，避免缓存 fallback 字形度量 | `terminalFonts.ts:1-32` |

### 2.4 DOM / CSS / 测试脚手架依赖

- **CSS**：`styles.css:1409-1416` 依赖 `.xterm`、`.xterm-screen`、`.xterm-viewport` 三个 xterm 生成的类名。
- **单元测试**：`TerminalView.test.tsx`（581 行）用 `vi.mock` 替换 5 个 xterm 模块，`FakeTerminal` 手工建模 `writeCallbacks` 队列（`:56`、`:68`）、`options`、`modes`、`unicode`；有两个测试专门验证 WebGL 加载与 context loss 回退（`:520-547`）。
- **浏览器冒烟**（`browser-smoke.mjs`，807 行）：**强依赖 xterm DOM 渲染器的文本节点**——
  - `.terminal-view.is-active .xterm`（判定 open 时机，`:375`）
  - `.xterm-helper-textarea`（等待就绪 + 聚焦输入，`:384`、`:451`）
  - `.xterm-rows > div` 文本过滤（校验命令输出、粘贴、**回放泄漏探针 REPLAY_SAFE/REPLAY_LEAK**，`:456`、`:515-518`、`:547-558`）
  - 为此脚本 **刻意用 `--disable-webgl` 启动 Chromium**（`:110-112`）以走 DOM 渲染路径。
- **CSP**（`internal/api/middleware.go:23`）：`default-src 'self'; script-src 'self'; …; connect-src 'self' ws: wss:; …`。**无 `'wasm-unsafe-eval'`，`connect-src` 无 `data:`**。
- **多实例挂载**：`Workspace.tsx:383` 对 `openSessions` 全部 `map` 渲染 `TerminalView`，只有一个 `active`。即同时存在 N 个终端实例。

### 2.5 现有构建体积（`internal/webui/dist/assets/`，实测）

| 产物 | raw | gzip | brotli |
|---|---|---|---|
| `TerminalView-*.js`（xterm + fit + unicode11 + web-links + web-fonts + 组件） | 378,471 | 98,165 | 77,993 |
| `addon-webgl-*.js`（独立动态 chunk） | 113,385 | 30,606 | 26,701 |
| **终端相关合计** | **491,856** | **128,771** | **104,694** |

---

## 三、ghostty-web 现状（核实于 2026-09-05）

### 3.1 身份与许可

- 仓库 <https://github.com/coder/ghostty-web>，描述 "Ghostty for the web with xterm.js API compatibility"，**MIT**，2763 stars / 171 forks / **55 open issues**，唯一 npm 维护者 `coder-bot <ash+bot@coder.com>`。
- npm 包名 `ghostty-web`：<https://www.npmjs.com/package/ghostty-web>（registry 元数据实测）
  - **最新稳定版 `0.4.0`，发布于 2025-12-09**
  - 最新预发布 `0.4.0-next.20.g1858a59`，2026-06-28
  - 共 81 个版本，`time.modified` = 2026-06-28；**距今近 10 个月无稳定发版**
  - `unpackedSize` 2,229,160 B / 9 个文件，**零运行时依赖**

### 3.2 维护活跃度 —— 主要风险点

- 最近一次提交：**2026-06-28**（`ci: fix release please notes setup (#183)`）。此前提交高度断续：2026-01-07 → 2026-02-19 → 2026-02-24 → 2026-06-26 三段，之后归零。
- **29 个 open PR 无人合并**，其中包括：
  - `#182 chore(release): 0.5.0-rc.0`（2026-06-28，发版 PR 自己卡住）
  - `#191 perf(renderer): read the viewport once per frame`（2026-09-01，说明当前渲染器**每帧多次读 viewport**）
  - `#190 fix(ime): draw the composition on the cursor, not in the corner`（2026-08-25）
  - `#181 feat: add preserveScrollOnWrite option`、`#162 WIP: Upgrade to Ghostty 1.3`
- `#156 ghostty-web roadmap`（2026-04-19）——外部贡献者直接问"这个仓库还打算往前走吗，最近没什么动静"，**至今 0 条回复**。<https://github.com/coder/ghostty-web/issues/156>
- `#137 Request: publish new npm release (v0.5.0?)`（2026-03-12）未处理；2026-08-03 有人补充实测：**已发布的 0.4.0 里 OSC 8 超链接根本不可用**——`ghostty-vt.wasm` 导出的 77 个函数没有一个与 hyperlink 相关，`getHyperlinkUri()` 硬编码 `return null`，所有 cell 的 `hyperlink_id` 都是 1，因此内置的 `OSC8LinkProvider` 永远不会 emit。<https://github.com/coder/ghostty-web/issues/137>
- 2025-12 HN 首发帖里，Coder 的 Kyle Carbs 自述定位："**We spent little time on performance so far, this is more of a POC** that will hopefully become a drop-in replacement for xterm.js over time."；mitchellh 当场指出其"逐行抓 viewport"的做法"probably pretty slow"。<https://news.ycombinator.com/item?id=46110842>
- 近期新开 issue（#186–#192，2026-07 ~ 09）全部 **0 回复**。

### 3.3 渲染方式：**只有 Canvas 2D**

- `dist/index.d.ts` 中 `Terminal.renderer?: CanvasRenderer`；打包产物里 `getContext(...)` 只出现 `getContext("2d")` 两次，**没有任何 WebGL/WebGPU 代码**。
- `#155 [Feature] WebGL Renderer` 仍 open。评论里 `NimbleMarkets` 的 fork 有 `nm-webgpu` / `nm-kitty-meow` 分支实现了 WebGPU + WebGL 雏形，作者自述"没怎么 review，只做了冒烟测试，LLM 辅助写的"，且**未合并进上游**。<https://github.com/coder/ghostty-web/issues/155>
- 渲染循环（打包产物实测）是**无条件 rAF 全量重绘**：
  ```js
  startRenderLoop() { const A = () => { if (!this.isDisposed && this.isOpen) {
      this.renderer.render(this.wasmTerm, !1, this.viewportY, this, this.scrollbarOpacity);
      ... this.animationFrameId = requestAnimationFrame(A); } }; A(); }
  ```
  **没有 dirty 检查**——只要 `isOpen`，每个实例每帧都跑一遍 render。

### 3.4 `write()` 的实际实现（打包产物实测，**最关键的差异**）

```js
writeInternal(A, B) {
  this.wasmTerm.write(A);                     // ← 同步进 wasm 解析，无分块、无让出
  this.processTerminalResponses();            // ← 只读 1 条 response
  ... A.includes("\x07") && this.bellEmitter.fire();
  this.linkDetector?.invalidateCache();
  this.viewportY !== 0 && this.scrollToBottom();   // ← 每次写都强制滚到底
  typeof A == "string" && A.includes("\x1B]") && this.checkForTitleChange(A);
  B && requestAnimationFrame(B);              // ← 回调走 rAF
}
```
三点差异，全部对 wmux 不利：
1. **无 back-pressure**：xterm.js 的 `WriteBuffer` 会分块解析并让出事件循环；ghostty-web 在调用栈上一次性解析完。wmux 回放上限 16 MiB（`cmd/wmux/main.go:88-92`），意味着回放期间主线程会被连续同步占满。
2. **回调是 `requestAnimationFrame`**：后台标签页 / 手机锁屏时 rAF 被挂起 → `ReplayBarrier` 永不 open → `disableStdin` 一直为 true。回到前台才恢复。
3. `this.viewportY !== 0 && this.scrollToBottom()`：**用户往上翻 scrollback 时，任何一帧新输出都会把他拽回底部**。对应未修的 `#127`（<https://github.com/coder/ghostty-web/issues/127>）和未合并的 PR #181。xterm 的 `scrollOnUserInput` 没有对应物。
4. `processTerminalResponses()` 每次 write 只 `readResponse()` **一条**。一个写入块里含多个设备查询（DA1/DA2/DSR 连发，tmux/neovim 握手常见）时，回复会被漏掉或滞后。

### 3.5 API 覆盖面（从 `dist/index.d.ts` 实测）

`ITerminalOptions` 全集只有 12 个字段：
```
cols rows cursorBlink cursorStyle theme scrollback fontSize fontFamily
allowTransparency convertEol disableStdin smoothScrollDuration
```
事件全集：`onData onResize onBell onSelectionChange onKey onTitleChange onScroll onRender onCursorMove`。**没有 `onBinary`、没有 `onWriteParsed`、没有 `modes`**。`unicode` 只是 `IUnicodeVersionProvider { readonly activeVersion: string }`（**只读**）。

`options` 是 `new Proxy(...)`，`set` 陷阱调 `handleOptionChange`，因此运行时改 `fontSize`/`cursorStyle`/`cursorBlink`/`disableStdin` **有效**。`disableStdin` 在 `InputHandler` 回调处生效（`this.options.disableStdin || this.dataEmitter.fire(E)`）、在 `paste()`/`input()` 处生效；但 **`processTerminalResponses()` 直接 `dataEmitter.fire()`，绕过 `disableStdin`**（wmux 应用层的 `canSendInput()` 仍能兜住，属于纵深防御生效）。

`ITerminalAddon` 形状是 `activate(terminal: ITerminalCore)`，其中 `ITerminalCore` 只有 `cols/rows/element/textarea` —— **与 xterm addon 的 `activate(terminal: Terminal)` + `_core` 内部访问不兼容，现有 addon 一个都装不上**。包内自带 `FitAddon`、`UrlRegexProvider`、`OSC8LinkProvider`、`LinkDetector`。

### 3.6 VT 能力

- **强项（来自 libghostty 本体）**：Unicode grapheme cluster 与宽字符（原生，不需要 addon）、真彩色、XTPUSHSGR/XTPOPSGR（README 明确对比 xterm.js 的 <https://github.com/xtermjs/xterm.js/issues/2570>）、复杂文字（天城文/阿拉伯文）的两遍渲染（背景/文字分离，避免左伸变音符被下一格背景盖住）。
- **Kitty 键盘协议**：`KeyEncoder` + `KittyKeyFlags` 存在。但 mitchellh 本人在 xterm.js #5686 里说明：libghostty-vt 依赖 embedder 提供键盘布局信息，"**I haven't tried to implement the embedder side of this in the browser so it probably just isn't possible**"，建议在浏览器里直接关掉。<https://github.com/xtermjs/xterm.js/issues/5686>
- **图像协议**：包内 `sixel` 0 次命中，`Kitty` 1 次（键盘相关）。**0.4.0 无 Sixel、无 Kitty graphics**。（对比：xterm.js 的 `addon-image` 支持 Sixel/IIP，且 2026-08 刚合入 WEBP/AVIF：<https://github.com/xtermjs/xterm.js/pull/6125>）
- **OSC 8**：解析层有，但如 3.2 所述**在 0.4.0 发布版里彻底失效**。

### 3.7 输入法与移动端

- 打包产物里有 `compositionstart/update/end`、`isComposing`、`textarea`、`autocapitalize`、`autocorrect` —— 基础 IME 框架存在。
- 但已知未修问题：`#119 Korean (Hangul) IME input not working`、`#97 CJK Input position issue`、PR `#190 fix(ime): draw the composition on the cursor, not in the corner`（未合并）。对中文用户是直接可感的退化。
- HN 帖记录：iOS Safari 最初连软键盘都弹不出来，后由社区 PR 修复。

### 3.8 无障碍

`screenReaderMode` 0 次命中，无 aria-live。`#187 Screen-reader accessibility: terminal output has no accessible representation (xterm.js screenReaderMode as reference)`（2026-08-01，0 回复）。**canvas 渲染 ⇒ 屏幕阅读器完全读不到终端内容**。

### 3.9 其它已知未修问题（全部 open、0 回复）

| Issue | 内容 | 对 wmux 的相关性 |
|---|---|---|
| `#192`(2026-09-04) | `attachCustomKeyEventHandler` 返回值语义与 xterm 相反，**静默吞掉全部输入** | 高（若将来要用） |
| `#189`(2026-08-18) | render 里抛一次异常 → 渲染循环**永久停止**且无法重启 | 高 |
| `#188`(2026-08-01) | wasm 经 `data:` URI fetch 加载，被 `connect-src 'self'` 挡；`init()` 不提供 wasm 路径覆盖 | **高（wmux 有严格 CSP）** |
| `#186`(2026-07-12) | 超链接 arena 耗尽（`StringAllocOutOfMemory`）会让 write 路径空转而非降级 | 高 |
| `#141`(2026-03-15) | 处理过多码点 grapheme 后 **dispose 终端导致 WASM 内存损坏** | **高（wmux 每次关会话都 dispose）** |
| `#140`(2026-03-13) | `scrollbackLimit` 文档说是行数，WASM 当字节数解释 | 高（wmux 传 10000） |
| `#139`(2026-03-13) | 视口跨 page 时 `getViewport()` 返回损坏数据 | 高 |
| `#153`(2026-04-13) | `ESC k <text> ESC \`（screen/tmux 标题序列）payload 泄漏为可见文本 | **高（wmux 默认 tmux 后端）** |
| `#125`(2026-02-12) | "theme changes after open() are not yet fully supported" | 中（wmux 有 light/dark/system 偏好） |
| `#184`(2026-07-11) | Powerline prompt 渲染出条带/涂抹 | 中 |
| `#126`(2026-02-13) | `──` 等制表符之间有缝隙 | 中（tmux 边框） |

我实测确认打包产物里 wasm 确实是 `new URL("data:application/wasm;base64,AGFzbQEAAAAB…")`，`Ghostty.load(path)` 有 `loadFromPath` 分支可绕过，但公开的 `init()` 签名是 `init(): Promise<void>`，无参数——需要改用 `Ghostty.load('/ghostty-vt.wasm')` + `new Terminal({ ghostty })` 这条非文档路径。

### 3.10 体积（实测 gzip -9 / brotli -q 11）

| 文件 | raw | gzip | brotli |
|---|---|---|---|
| `dist/ghostty-web.js`（**wasm 已 base64 内联**） | 681,918 | 192,619 | 156,481 |
| `ghostty-vt.wasm`（独立文件，走 `loadFromPath` 时用） | 423,045 | 123,991 | 98,868 |

**对比 wmux 现状**：默认路径下 156 KB brotli vs 现在的 105 KB brotli（含 WebGL addon），**首屏终端 chunk 增大约 +50%**；且 base64 内联意味着 wasm 无法独立长缓存、无法与 JS 分开更新。

---

## 四、逐项能力映射表

图例：✅ 直接可用 ｜ ⚠️ 需改造/降级 ｜ ❌ 缺失

| # | wmux 依赖 | ghostty-web 0.4.0 | 说明 / 来源 |
|---|---|---|---|
| 1 | `allowProposedApi: true` | ❌ | 无此概念，`ITerminalOptions` 无该字段（`dist/index.d.ts`） |
| 2 | `allowTransparency: true` | ✅ | 有该选项 |
| 3 | `convertEol: false` | ✅ | 有 |
| 4 | `disableStdin` + 运行时切换 | ✅ | Proxy set 生效；`InputHandler`/`paste`/`input` 三处拦截。但设备回复 `processTerminalResponses` 绕过它（实测） |
| 5 | `drawBoldTextInBrightColors` | ❌ | 无 |
| 6 | `fontFamily` / `fontSize`（运行时） | ✅ | `handleFontChange` + `remeasureFont()` |
| 7 | `fontWeight` / `fontWeightBold` | ❌ | 无。粗体只能靠 canvas `font` 字符串，无法指定 400/600 |
| 8 | `letterSpacing` / `lineHeight: 1.18` | ❌ | 无。行高由字体度量决定，wmux 的排版会变 |
| 9 | `macOptionIsMeta: true` | ❌ | Terminal 选项无；底层 `KeyEncoderOption.ALT_ESC_PREFIX` 存在但未从 Terminal 暴露 |
| 10 | `minimumContrastRatio: 4.5` | ❌ | 无。**WCAG AA 对比度保障直接消失**（xterm typings 文档：4.5 = WCAG AA） |
| 11 | `rescaleOverlappingGlyphs` | ❌ | 无（本来也只在非 DOM 渲染器生效） |
| 12 | `rightClickSelectsWord` | ❌ | 无 |
| 13 | `scrollOnUserInput` / 保持用户滚动位置 | ❌ | 更糟：`writeInternal` **无条件 `scrollToBottom()`**（实测）；issue #127 open，PR #181 未合并 |
| 14 | `scrollback: 10000` | ⚠️ | 选项在，但 `#140`：文档说行、WASM 当字节。10000 可能意味着 ~10 KB 而非 1 万行 |
| 15 | `cursorStyle` / `cursorBlink`（运行时） | ✅ | `handleOptionChange` 覆盖 |
| 16 | **`write(data, cb)` 排干语义（ReplayBarrier）** | ⚠️**核心风险** | 签名相同、回调存在，但语义不同：同步解析无分块 + rAF 回调（实测）。后台标签页 rAF 挂起 → barrier 不开 |
| 17 | `onData` | ✅ | 有 |
| 18 | `onBinary` | ❌ | 事件表无该项。`encodeTerminalBinaryFrames`（`terminalProtocol.ts:71-88`）+ 其单测成为死代码；原本走二进制通道的设备回复会以 UTF-8 字符串走 `onData`，非 ASCII 字节存在被改写的风险 |
| 19 | `modes.applicationCursorKeysMode` | ⚠️ | `Terminal` 无 `modes`。可用 `terminal.wasmTerm?.getMode(1)`（`GhosttyTerminal.getMode(mode, isAnsi?)`，非文档 API，字段类型是 optional） |
| 20 | `unicode.activeVersion = '11'` | ⚠️ | 属性 **`readonly`**，赋值编译不过。功能上不需要（Ghostty 原生做 grapheme），但代码要删 |
| 21 | `FitAddon.fit()` + ResizeObserver | ✅ | 包内自带 `FitAddon`（同名同 `fit()`），改 import 即可。注意它自己也带 `_resizeObserver` + debounce，可能与 wmux 的 ResizeObserver 重复触发 |
| 22 | `Unicode11Addon` | ✅ | 不需要，VT 核心原生处理（能力上是**升级**） |
| 23 | `WebLinksAddon(handler)` | ⚠️ | 无同名 addon；改用 `registerLinkProvider` + 自带 `UrlRegexProvider`。`ILink.activate(event)` 里自行 `window.open`。**OSC 8 在 0.4.0 完全失效**（#137 实测） |
| 24 | `WebglAddon` + `onContextLoss` 降级 | ❌ | 无 WebGL。仅 canvas 2D（实测 `getContext("2d")`）。`attachWebglRenderer`（`TerminalView.tsx:60-70`）及其 2 个单测（`:520-547`）全部删除 |
| 25 | `@xterm/addon-web-fonts` `loadFonts` | ✅ | 独立包，可继续用。但"字体就绪后再 open"的编排要改成对接 `remeasureFont()` |
| 26 | `focus()` / `clear()` / `dispose()` / `getSelection()` / `paste()` / `cols`/`rows` / `open()` | ✅ | 全部存在；`paste()` 走 bracketed paste（`hasBracketedPaste()` 实测） |
| 27 | bracketed paste | ✅ | 同上 |
| 28 | 移动端软键盘 / IME | ⚠️ | 有 composition + textarea，但 `#119`(韩文)、`#97`(CJK 位置)、PR `#190`(合成串画在角落) 全未修 |
| 29 | 无障碍 / 屏幕阅读器 | ❌ | 无 `screenReaderMode`、无 aria-live；canvas 渲染无文本可读（`#187`） |
| 30 | `.xterm` / `.xterm-screen` / `.xterm-viewport` CSS | ❌ | 只有一个裸 `<canvas>`。`styles.css:1409-1416` 要重写 |
| 31 | `.xterm-helper-textarea`（smoke 聚焦） | ⚠️ | `Terminal.textarea?: HTMLTextAreaElement` 存在，但类名不同，选择器要换 |
| 32 | `.xterm-rows > div` 文本断言（smoke 6 处） | ❌ | **canvas 无文本 DOM**。必须改为经 `buffer.active.getLine(y).translateToString()`（该 API 存在）并新增 window 级测试 hook |
| 33 | `vi.mock('@xterm/*')` 单测脚手架 | ⚠️ | mock 目标从 5 个模块变 1 个；`FakeTerminal` 的 write 回调需改成 rAF 时序；`unicode`/`modes` 假对象要改 |
| 34 | CSP `script-src 'self'` | ❌ | Chrome 下 `WebAssembly.instantiate` 需要 `'wasm-unsafe-eval'`（实测产物用的是 `WebAssembly.instantiate`） |
| 35 | CSP `connect-src 'self' ws: wss:` | ❌ | wasm 走 `fetch("data:application/wasm;base64,…")`（实测）→ 需加 `data:`，或改走非文档的 `Ghostty.load(path)` |
| 36 | 多实例（N 个 open session 同时挂载） | ❌ | 每个实例一条**无条件 rAF 全量重绘**循环（实测）。xterm 只在脏时重绘。手机上是电量/CPU 直接退化 |
| 37 | tmux 后端 | ⚠️ | `#153`：`ESC k …` 标题序列 payload 会当可见文本打出来 |
| 38 | 会话关闭 `dispose()` | ⚠️ | `#141`：处理过 grapheme 后 dispose 有 WASM 内存损坏 |
| 39 | Sixel / Kitty graphics | ❌ | 无（xterm 侧 wmux 目前也没装 `addon-image`，属平手） |
| 40 | Kitty 键盘协议 | ⚠️ | 编码器存在，但浏览器缺布局信息，mitchellh 判断"probably just isn't possible"（xterm.js #5686） |

**统计：40 项中 ✅ 11、⚠️ 13、❌ 16。**

---

## 五、收益与代价分析

### 5.1 性能：宣传数字与 wmux 实际瓶颈不是同一层

2026-08-14 mitchellh 在 xterm.js `#5686` 贴出对最新 xterm.js 的实测对比（<https://github.com/xtermjs/xterm.js/issues/5686>），结论是 libghostty 在 IO 吞吐、列 reflow、渲染准备三项上都"a lot a lot faster"。但必须看清测试口径（他自己写明的）：

- **IO 吞吐**：`ghostty_terminal_vt_write` vs `Terminal.write(Uint8Array)`，测的是 **wasm C API 裸调用**，不是 ghostty-web。
- **渲染**：两边都用 **xterm.js 自己的 WebGL 渲染器**，只比较"从终端状态到 render-ready"的准备耗时。**完全没有测 ghostty-web 的 canvas 2D 渲染器。**
- 他自己列出的注意事项：结果是图片；xterm.js 方差极大（ASCII 全屏渲染波动达 8x），他**取了 xterm 最快的 20%**，即"对 xterm 已经算乐观"；Ghostty bundle 未压缩比 xterm 大 5x。

也就是说：**"ghostty 比 xterm 快"这句话在 VT 解析与状态维护层为真，在 ghostty-web 这个具体产品的渲染层没有任何证据，反而有反证**——mitchellh 在 HN 首发帖当场指出其逐行抓 viewport"probably pretty slow"；上游至今还挂着 `#191 perf(renderer): read the viewport once per frame` 的未合并 PR。

对 wmux 的三个具体场景：

| 场景 | 现状（xterm 6.0.0 + WebglAddon） | 换 ghostty-web 后 |
|---|---|---|
| `yes` / `cat` 大文件 | WebGL 渲染 + WriteBuffer 分块让出 | VT 解析更快（净收益），但渲染回到 canvas 2D 且每帧全量重绘（净损失），**方向不确定** |
| `htop` 高刷新 | 同上 | 同上 |
| **16 MiB 历史回放** | `write` 分块解析、事件循环可让出、UI 有"正在同步历史输出"能动 | **同步解析整块、主线程无让出**；且回调走 rAF，后台标签页永不完成 → **明确退化** |
| 手机端多会话同时打开 | 非活动终端不重绘 | **N 条 60fps rAF 循环**，明确退化 |

### 5.2 正确性：有得有失，但"失"更贴近 wmux 的日常

- **得**：grapheme cluster / RTL / 复杂文字、XTPUSHSGR/XTPOPSGR、VT 一致性总体更接近原生 Ghostty。
- **失（且都命中 wmux 的默认路径）**：tmux 标题序列泄漏为可见文本（`#153`）、tmux 边框 `──` 有缝（`#126`）、Powerline prompt 条带（`#184`）、OSC 8 完全失效（`#137`）、主题运行时切换不完整（`#125`）、`scrollback` 单位歧义（`#140`）、翻 scrollback 被强制拽回底部、dispose 后 WASM 内存损坏（`#141`）、渲染循环抛一次就永久停（`#189`）。

wmux 用户 100% 的时间在 tmux + neovim + shell 里，不在天城文和 XTPUSHSGR 里。**收益落在长尾，代价落在主路径。**

### 5.3 包体积与首屏

+50% brotli（105 KB → 156 KB），且 wasm base64 内联到 JS 里，无法独立缓存。走 `loadFromPath` 分离可以改善缓存，但那是非文档路径，还要自己处理 PWA service worker precache 清单。**对一个手机也要用的自托管应用是净负面。**

mitchellh 自己的体积表（xterm.js `#5686`，2026-08-15）也印证这个方向：

| Build | Bytes | Brotli |
|---|---|---|
| libghostty default (all features) | 876,500 | 218,309 |
| libghostty web interactive | 661,119 | 168,994 |
| libghostty read-only viewer | 537,441 | 132,858 |
| libghostty bare VT core | 515,422 | 125,756 |
| **xterm.js browser bundle（含渲染器）** | **488,663** | **99,311** |
| @xterm/headless | 182,672 | 39,651 |

即：**libghostty 光 VT 核心（不含渲染器和输入层）就已经比 xterm.js 的完整浏览器包还大**。

### 5.4 维护风险 —— 这是决定性因素

- **单一维护方 + 已经停摆**：最后提交 2026-06-28（2 个月余），29 个 PR 无人 review（含发版 PR 自己），最近 7 个 issue 零回复，roadmap 追问 5 个月无人应答，稳定版停在 2025-12-09。
- **官方自述是 POC**，不是产品。
- **API 不稳定**：`0.x`，且 `#192` 这种"返回值语义与 xterm 相反"的兼容性 bug 说明"xterm 兼容"没有被系统性验证。
- **反向对照，xterm.js 侧非常健康**：仓库 2026-09-02 仍有提交，21k stars，6.0.0（2025-12-22）为当前 stable，6.1.0-beta 持续发到 2026-08-30（beta.304），2026-08 刚为 image addon 加了 WEBP/AVIF。

> 注意一个反向风险：xterm.js `#6106`（2026-08-14 开，**未关闭**）报告 master 上因近期（作者称多为 AI 驱动的）改动出现高负载下的严重性能回退，Firefox 慢约 3x，热点在 `BufferLine._copySparseMapsFrom`、`_invalidateStringCache`、`DecorationLineCache._handleBufferLinesTrim`。<https://github.com/xtermjs/xterm.js/issues/6106>
> **我在 wmux 已安装的 `@xterm/xterm@6.0.0` 里逐一 grep 了这三个符号，全部为 0 次命中** —— 即 **wmux 当前不受该回退影响**。这条的操作含义是：**不要盲目跟进 6.1 beta**。

### 5.5 迁移工作量估算

| 文件 | 现有行数 | 预估改动 | 内容 |
|---|---|---|---|
| `client/src/components/TerminalView.tsx` | 765 | ~130 行 | 删 `attachWebglRenderer`；构造选项从 15 项砍到 8 项；`unicode`/`modes` 改写；`onBinary` 移除；`WebLinksAddon` → `registerLinkProvider`；`init()`/`Ghostty.load()` 生命周期；字体就绪编排改接 `remeasureFont` |
| `client/src/terminalFonts.ts` | 32 | ~20 行 | 围绕 `remeasureFont()` 重排 |
| `client/src/terminalProtocol.ts` | 208 | ~40 行 | `encodeTerminalBinaryFrames` 退役或降级为死代码；`encodeCursorKey` 的 mode 来源改 `wasmTerm.getMode(1)`；`ReplayBarrier` 需重新论证 rAF 时序 |
| `client/src/components/TerminalView.test.tsx` | 581 | ~120 行 | mock 目标合并；`FakeTerminal` write 回调改 rAF 建模；删 2 个 WebGL 测试；`options` Proxy 行为建模 |
| `scripts/browser-smoke.mjs` | 807 | ~150 行 | **6 处 `.xterm-rows` 文本断言全部失效**，需新增 window 级 buffer 文本 hook；`.xterm-helper-textarea` 选择器换；`--disable-webgl` 前提消失；**回放泄漏探针 REPLAY_SAFE/REPLAY_LEAK 的断言方式要重做** |
| `client/src/styles.css` | 6 行相关 | ~15 行 | 3 个 xterm 类名 → canvas 布局 |
| `internal/api/middleware.go` | 1 行 | 1 行 | CSP 加 `'wasm-unsafe-eval'`（+ 视方案加 `data:`） |
| `internal/webui` / PWA | — | 少量 | wasm MIME、长缓存、service worker precache |

**合计约 450–600 行改动、7–8 个文件，其中浏览器验收测试的策略需要重新设计（不是改选择器那么简单）。**保守估计 3–5 个专注工作日 + 一段真机（iOS Safari / Android Chrome）回归尾巴。而这一切换来的是一个上游已停更 2 个月的 0.4.0。

### 5.6 折中方案

1. **先把 xterm 侧的便宜账做掉**（成本近乎为零）：
   - `docs/reviews/2026-09-04-independent-code-review.md:87-91` 的 **M3 已经修完**（`package.json` 有 `@xterm/addon-webgl@^0.19.0`，`TerminalView.tsx:60-70` 有 `onContextLoss` 降级）。所以"xterm 用 DOM 渲染器所以慢"这个前提**已经不成立**，不能再拿它当换引擎的理由。
   - 但 **该 review 提出的另外两点尚未处理**：`minimumContrastRatio: 4.5`（会在每个 cell 上动态改前景色）与 `allowTransparency: true`（禁用 WebGL 的 alpha 快路径）。wmux 的 `.terminal-canvas` 本来就有不透明的 `var(--terminal-bg)`，`allowTransparency` 大概率可以直接关掉。
   - M7（attach 时在会话互斥锁内从磁盘回放，`internal/terminal/attachment.go:31-56`）未修 —— **这才是"新设备打开长会话时其它设备明显卡顿"的真正来源，且完全在服务端，与前端引擎无关**。
2. **可切换渲染后端实验**：不做整体替换，先在 `TerminalView` 与终端实现之间抽一层薄接口（`open/write/onData/resize/focus/dispose/getSelection` 约 10 个方法），用一个 URL 参数或偏好开关选择后端。这样实验成本可控、可随时回滚，也为将来接官方 libghostty wasm 或 xterm.js 自己吸收 libghostty 留了口子。
3. **等生态收敛**：xterm.js 维护者 Tyriar 已在 `#5686` 主动探索采纳 libghostty（2026-02 开，2026-09-03 仍在更新），mitchellh 明确表示愿意把 xterm.js 维护者拉进 libghostty 做维护者。若这条路走通，wmux **零迁移成本**就能拿到 libghostty 的解析速度。（阻力也真实存在：核心维护者 jerch 明确反对，理由是失去可 hook 的 DFA parser、把 VT 外包给外部维护者、大块 wasm 不利于安全审计，并主张"用定向 wasm 替换热点即可，parser 的 wasm 化约 4x 提升"。）

---

## 六、建议与后续动作

### 建议：**现在不换。** 采取「先修 xterm 侧便宜账 + 建立观察清单 + 一个时间盒受控实验」。

三条理由，按权重排序：

1. **触发这次讨论的性能问题（review M3）已经修完了。** WebGL 渲染器已在，剩下的钝痛（16 MiB 回放卡顿）根因在服务端 M7（锁内回放）与 `write` 分块策略，换引擎不但不解决，`writeInternal` 的同步无分块解析还会让它**更差**。
2. **ghostty-web 的强项与 wmux 的痛点不重叠，弱项恰好重叠。** 它强在 VT 解析（wmux 不是瓶颈），弱在渲染器（wmux 的瓶颈）、体积（wmux 是手机端 PWA）、无障碍、CSP 兼容、多实例开销。
3. **上游停摆是硬约束。** 一个 2 个月无提交、29 个 PR 无人 review、发版 PR 自己都卡住、roadmap 追问 5 个月无回音、自述 POC 的 0.4.0，不适合承载一个需要长期运行的自托管终端的核心。

### 立即动作（本周，与 ghostty 无关的净收益）

- [ ] 关掉 `allowTransparency: true`（`TerminalView.tsx:159`）——`.terminal-canvas` 已有不透明背景，可让 WebGL 走忽略 alpha 的快路径（xterm.js #5335 即为此）。改完跑 `pnpm test:all`。
- [ ] 量化 `minimumContrastRatio: 4.5` 的代价：先在真机上 A/B 一次 `yes` + `htop`，若 FPS 差异显著，考虑降到 1 并改为在 CSS/主题层保证对比度。
- [ ] **修 M7**：`internal/terminal/attachment.go:31-56` 把 `s.log.Replay` 移出 `s.mu`，锁内只登记 newest + 注册订阅者。这是回放卡顿的最大单点收益。
- [ ] 加一个可复现的性能基线脚本（`yes`、`ls -lR --color=always /usr`、16 MiB 回放三档），记录：write→回调总耗时、回放期间最长主线程阻塞、稳态 FPS、`performance.memory`。**没有这个基线，之后任何"换引擎有没有用"的讨论都是空谈。**
- [ ] 锁定 `@xterm/xterm@6.0.0`，**不要跟进 6.1 beta**，直到 `xtermjs/xterm.js#6106` 关闭。

### 观察清单与触发条件（建议每季度复查一次）

**换到 ghostty-web 的触发条件（需同时满足 A + B + 至少两条 C）：**

- **A. 恢复维护**：`coder/ghostty-web` 发布 ≥ 0.5.0 稳定版，且近 90 天有持续提交、open PR 数从 29 降到个位数。
- **B. 渲染器达标**：合并 WebGL/WebGPU 渲染器（`#155`），或至少实现脏区重绘 + 无输出时暂停 rAF。
- **C. 阻断项清零**（至少 2 条，其中 `#141`/`#188` 必须在内）：
  - `#141` dispose 后 WASM 内存损坏 —— **一票否决项**
  - `#188` CSP / wasm 路径可覆盖 —— **一票否决项**
  - `#153` tmux `ESC k` 标题序列泄漏
  - `#127`/PR #181 保持滚动位置
  - `#140` scrollback 单位
  - `#119`/`#97`/PR #190 CJK/韩文 IME
  - `#187` 屏幕阅读器可访问表示
  - `write()` 提供分块/让出，或提供不依赖 rAF 的解析完成信号

**更值得下注的两条替代路线（同样纳入观察）：**

- **xterm.js 采纳 libghostty**（`xtermjs/xterm.js#5686`）：若走通，wmux **零迁移**获得同等解析性能。这是目前性价比最高的可能性。观察信号：该 issue 出现 milestone / 实验分支合入 master / Tyriar 与 jerch 达成一致。
- **`vercel-labs/wterm` + `@wterm/ghostty`**（Apache-2.0，3432 stars，**2026-09-04 刚发 0.5.0，提交非常密集**，已有 Kitty keyboard #120、Kitty graphics #123、React/Vue/Svelte 绑定）：维护活跃度远高于 ghostty-web，但**是 DOM 渲染器且不提供 xterm.js API 兼容**，迁移成本比 ghostty-web 更高。若 ghostty-web 继续死，而 wterm 补上 canvas/WebGL 渲染，它会成为更现实的候选。<https://github.com/vercel-labs/wterm> / <https://wterm.dev/ghostty>

### 若要做受控实验（建议排到修完 M7 与基线脚本之后，时间盒 2 天）

**步骤**

1. 在 `client/src/components/` 下抽一个 `TerminalEngine` 接口（约 10 个方法：`open/write(data,cb)/onData/onResize/resize/focus/clear/getSelection/paste/dispose` + `cols/rows`），先用 xterm 实现它，确认 `pnpm test:all` 全绿——**这一步本身就是净收益，不依赖实验结论**。
2. 加 `?engine=ghostty` 开关 + 第二个实现（不进默认路径、不进 PWA precache）。
3. 用 `Ghostty.load('/ghostty-vt.wasm')` 走分离 wasm，避免 base64 内联；同步给 CSP 加 `'wasm-unsafe-eval'`（**仅在实验分支**）。
4. 跑步骤 0 的基线脚本，在 桌面 Chrome / iOS Safari / Android Chrome 三端各测一遍。
5. 手工核对 5 个"主路径正确性"场景：tmux 分屏边框、tmux 会话标题（验 `#153`）、neovim 里中文与 emoji、Powerline prompt、翻 scrollback 时持续输出。

**判定标准（全部满足才继续推进，否则记录结论、保留接口层、丢弃实现）**

- 16 MiB 回放的**主线程最长连续阻塞** ≤ xterm 基线的 1.2 倍，且回放期间 UI 仍能响应点击；
- `yes` / `htop` 稳态 FPS ≥ xterm + WebGL 基线的 0.9 倍；
- 3 个终端同时打开时的空闲 CPU 占用 ≤ xterm 基线的 1.5 倍；
- 5 个正确性场景零回归；
- iOS Safari + Android Chrome 上中文输入法可用（合成串位置正确）；
- 终端 chunk 的 brotli 体积增量 ≤ +30%（当前预估 +50%，达标需要走分离 wasm 且树摇有效）。

---

## 附：来源清单

**ghostty-web**
- 仓库 / README / 星标 / issue 与 PR 状态：<https://github.com/coder/ghostty-web>
- npm 包与版本时间线：<https://www.npmjs.com/package/ghostty-web>（元数据取自 `https://registry.npmjs.org/ghostty-web`）
- `#137` 发版请求 + OSC 8 在 0.4.0 失效的实测：<https://github.com/coder/ghostty-web/issues/137>
- `#155` WebGL 渲染器（含 NimbleMarkets 的 WebGPU fork 说明）：<https://github.com/coder/ghostty-web/issues/155>
- `#156` roadmap 追问（0 回复）：<https://github.com/coder/ghostty-web/issues/156>
- 其余引用的 issue：`#192` <https://github.com/coder/ghostty-web/issues/192>、`#189` <https://github.com/coder/ghostty-web/issues/189>、`#188` <https://github.com/coder/ghostty-web/issues/188>、`#187` <https://github.com/coder/ghostty-web/issues/187>、`#186` <https://github.com/coder/ghostty-web/issues/186>、`#153` <https://github.com/coder/ghostty-web/issues/153>、`#141` <https://github.com/coder/ghostty-web/issues/141>、`#140` <https://github.com/coder/ghostty-web/issues/140>、`#139` <https://github.com/coder/ghostty-web/issues/139>、`#127` <https://github.com/coder/ghostty-web/issues/127>、`#126` <https://github.com/coder/ghostty-web/issues/126>、`#125` <https://github.com/coder/ghostty-web/issues/125>、`#119` <https://github.com/coder/ghostty-web/issues/119>、`#97` <https://github.com/coder/ghostty-web/issues/97>
- 未合并 PR：`#191`/`#190`/`#185`/`#182`/`#181`/`#162` 见 <https://github.com/coder/ghostty-web/pulls>
- HN 首发讨论（Kyle Carbs "this is more of a POC"；mitchellh 指出逐行抓 viewport 慢）：<https://news.ycombinator.com/item?id=46110842>
- 上述所有 API/体积/`write` 实现/渲染循环/`data:` wasm/Proxy options 的结论，均来自本地解包 `ghostty-web@0.4.0` tarball 后对 `dist/index.d.ts` 与 `dist/ghostty-web.js` 的直接检查（可复现：`curl -sL https://registry.npmjs.org/ghostty-web/-/ghostty-web-0.4.0.tgz | tar xz`）

**xterm.js**
- 仓库活跃度（2026-09-02 仍有提交，21k stars）：<https://github.com/xtermjs/xterm.js>
- 6.0.0 发布说明：<https://github.com/xtermjs/xterm.js/releases/tag/6.0.0>
- `#5686 Explore adopting libghostty`（Tyriar 探索、jerch 反对、mitchellh 2026-08-14 基准与 2026-08-15 体积表、Kitty 键盘在浏览器的限制）：<https://github.com/xtermjs/xterm.js/issues/5686>
- `#6106 massive performance degradation`（2026-08-14 开，未关闭；已确认不影响 wmux 使用的 6.0.0）：<https://github.com/xtermjs/xterm.js/issues/6106>
- image addon 的 WEBP/AVIF 支持：<https://github.com/xtermjs/xterm.js/pull/6125>
- npm 版本/时间线：`https://registry.npmjs.org/@xterm/xterm`（latest 6.0.0，beta 6.1.0-beta.304 @ 2026-08-30）

**libghostty / 其它路线**
- 官方签名 wasm 构建（`ghostty-vt.wasm` 1,006,560 B / `ghostty-vt-small.wasm` 746,177 B，资产更新时间 2026-09-04）：<https://github.com/ghostty-org/ghostty/releases/tag/tip>
- libghostty C API 文档：<https://libghostty.tip.ghostty.org/>
- mitchellh 的 libghostty 背景文章：<https://mitchellh.com/writing/libghostty-is-coming>
- mitchellh 关于 wasm 基准的 X 帖（**未能直接抓取，返回 HTTP 402**；其内容以 `#5686` 中他本人贴出的同一份说明为准）：<https://x.com/mitchellh/status/2088378990998524206>
- `vercel-labs/wterm`：<https://github.com/vercel-labs/wterm> ｜ `@wterm/ghostty` 说明：<https://wterm.dev/ghostty>

**wmux 自身**
- `docs/architecture.md:49,58,68`（二进制帧格式、回放屏障设计意图、字体与 Unicode 11）
- `docs/reviews/2026-09-04-independent-code-review.md:87-91`（M3 渲染器）、`:112-114`（M7 锁内回放）
- 代码：`client/src/components/TerminalView.tsx`、`client/src/terminalProtocol.ts`、`client/src/terminalFonts.ts`、`client/src/components/TerminalView.test.tsx`、`client/src/components/Workspace.tsx:383`、`client/src/styles.css:1409-1416`、`scripts/browser-smoke.mjs`、`internal/api/middleware.go:23`、`package.json`
