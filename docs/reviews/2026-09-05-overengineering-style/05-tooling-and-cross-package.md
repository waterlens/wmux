# wmux 代码审查：工具链/脚本/部署/文档 + 跨包重复与一致性

审查范围仅限两类问题：**A. 过度工程**、**B. 不当代码风格**。正确性 bug、安全漏洞、性能问题不在范围内（已由 `docs/reviews/2026-09-04-independent-code-review.md` 覆盖）。

审查基线：`main` @ `4fb3e9f`，工作区干净。`gofmt -l` 无输出，`go vet ./...` 通过。

---

## 第一部分：工具链、脚本、部署、文档

### T-01　`browser-smoke.mjs` 手写重造了 `@playwright/test` 的整套基础设施
**严重程度：高　类别：过度工程**
**位置：`scripts/browser-smoke.mjs:645-807`（163 行）、`scripts/browser-smoke.mjs:37-94`（58 行）、`package.json:57`**

`package.json:57` 依赖的是 `playwright-core`（只有浏览器驱动，没有测试运行器），于是脚本自己手写了运行器该提供的一切：

| 手写实现 | `@playwright/test` 现成能力 |
| --- | --- |
| `findChrome()` `:744-763`，硬编码 7 个可执行文件路径 | `channel: 'chrome'` / `playwright install chromium` |
| `availablePort()` `:765-776` | `webServer.port` |
| `waitForHealth()` `:778-791`，自己写 10 秒轮询 | `webServer.url` + `webServer.timeout` |
| `stopChild()` `:793-802`，自己写 SIGTERM→5s→SIGKILL | `webServer` 自动收敛子进程 |
| `:645-649` 顶层 `finally` 做全局清理 | fixture / `globalTeardown` |
| **50 处** `throw new Error(...)`（`grep -c` 实测） | `expect(...)` 带自动重试 |

没有重试断言的直接后果是脚本里出现 4 处固定 `sleep`：`:215`、`:224`（各 150ms）、`:460`（250ms）、`:476`（200ms）。这是"缺少 auto-retrying assertion"的典型症状，也是这类脚本最常见的不稳定源。

另外整个脚本是**一个扁平的顶层 `try` 块**，没有用例隔离：任何一处 `throw` 都会中止后面全部检查，无法知道还有多少问题。

**为什么是问题**：约 220/807 行是纯基础设施，且质量必然不如上游实现。这些代码需要长期维护，却没有带来 `@playwright/test` 之外的任何能力。

**建议**：换成 `@playwright/test`（`playwright.config.ts` 写 `webServer: { command: './dist/wmux', url: '...', env: {...} }`），把每个当前的 `if (...) throw` 改成 `await expect(...)`，把顶层流程拆成 3-4 个 `test()`。`:744-802` 四个辅助函数可整体删除。

---

### T-02　`browser-smoke.mjs` 与 vitest 单测逐字重复覆盖
**严重程度：高　类别：过度工程**

多条冒烟断言是 vitest 单测里**同一个字符串、同一个语义**的复制，只是换成了在真浏览器里跑（慢 100 倍，且需要真实二进制 + tmux + python3）：

| `scripts/browser-smoke.mjs` | 已被覆盖于 |
| --- | --- |
| `:176-183` 断言不存在 `'主机密钥校验始终开启'`、`'wmux 不会静默接受新指纹…'` | `client/src/components/HostManager.test.tsx:73-74` |
| `:188-195` 断言不存在 `'凭据由 wmux 服务加密保管。'`、`'支持 OpenSSH PEM 格式'`、`/保存后还需要探测并确认主机指纹/` | `client/src/components/HostManager.test.tsx:78-80` |
| `:197-200` 新建主机默认私钥认证 | `client/src/components/HostManager.test.tsx:60` |
| `:160-163` 侧栏无数字计数器 | `client/src/components/Sidebar.test.tsx:85-87` |
| `:257-258` ProxyJump 候选禁用 + `'暂不支持 ProxyJump'` 标签 | `client/src/components/SSHConfigImport.test.tsx:87-89` |
| `:275-278` 无元信息候选不渲染空的 `.ssh-config-candidate__meta` | `client/src/components/SSHConfigImport.test.tsx:94-110` |
| `:288-289` 导入不触发 probe | `client/src/components/SSHConfigImport.test.tsx:63-64` |
| `:360-365` 新建会话对话框文案 | `client/src/components/SessionDialog.test.tsx:26` |
| `:336-337` 嵌套确认框只暴露一个 `dialog` role | `client/src/components/UI.test.tsx:85` |
| `:571-576` 结束会话确认文案（`'将结束进程并删除终端历史。'` / 无 `'这个操作无法撤销。'`） | `client/src/components/Workspace.test.tsx:105-106` |
| `:227-249` SSH config discovery 的 JSON 形状（IdentityFile 不外泄、ProxyJump 标记） | `client/src/api.test.ts:95,151` + `internal/api/ssh_config_test.go` |
| `:613-619` 删除后无残留会话 | `internal/api/lifecycle_test.go` / `internal/api/sessions_test.go` |

**为什么是问题**：这些是**纯 DOM/文案断言**，jsdom 里跑就够，放进浏览器冒烟里只是让 CI 慢、让失败定位变难，而且同一句文案改动要改两处。

**建议**：冒烟脚本只保留 jsdom 无法验证的部分——真实 xterm + 真实 PTY/tmux 往返（`:451-456`）、大尺寸 bracketed paste（`:479-522`）、reload 后的重放隔离（`:524-564`）、WebGL 关闭后的 DOM renderer 路径（`:109-112`）、真实 webfont 网络时序（`:119-133`、`:424-440`）、`/api/status` 的构建版本注入（`:96-107`）。其余全部删掉，交给 vitest。粗估可从 807 行降到 250 行以内。

---

### T-03　`assertDialogLayout` 把 CSS 设计令牌当断言值硬编码
**严重程度：中　类别：过度工程**
**位置：`scripts/browser-smoke.mjs:651-742`（92 行）**

这个函数在浏览器里读 `getComputedStyle`，然后拿硬编码像素值做断言：`:712` `actionHeight = mobile ? 44 : 36`、`:721` `height > 36.5`、`:727` `titleSize < 16 || titleSize > 18 || descriptionSize !== 14`、`:729` `headerBorder/footerBorder` 必须为 0、`:730` footer 背景必须透明；调用处还有 `:205` `height > 60.5`、`:334`/`:579` `maxHeight: 190`、`:343` `maxHeight: 220`。

而 `client/src/styles.css:50,52` 已经把这些定义成设计令牌：`--control-sm: 2.25rem`（=36px）、`--control-touch: 2.75rem`（=44px），字号走 `--text-*`。冒烟脚本**复制的是这些令牌在 16px 根字号下的计算值，且没有任何注释指回 CSS**。

另外 `:689-691` 已经用 `elementFromPoint` 判定关闭按钮可点击，`:734` 又用 `click({ trial: true })` 再判定一次——同一件事两种写法。

**为什么是问题**：调一次 `--control-sm` 就会让冒烟测试以 `actions have incorrect desktop/mobile height` 失败，而失败信息完全指不到 CSS。这是把视觉回归测试用 200 行手写像素断言实现，维护成本远高于收益。

**建议**：删掉 `:712-732` 的像素/字号/边框/背景断言，只保留真正的"布局塌了"级检查——`:714` 面板不溢出视口、`:716` 关闭按钮在面板内且可点击、`:723` 表单不需要内部滚动。想要像素级保证就上 `@playwright/test` 的 `toHaveScreenshot()`。

---

### T-04　`WMUX_BROWSER_URL` 外部实例模式：无人使用的配置项，却把脚本控制流劈成两半
**严重程度：中　类别：过度工程**
**位置：`scripts/browser-smoke.mjs:24, 38-94, 102, 114, 119, 226, 369, 424, 479, 647`**

脚本支持两种模式：自建临时实例，或通过 `WMUX_BROWSER_URL` 连到外部实例。后者导致 **7 处 `if (ownedServer)` 分支**，其中 `:226-311`（SSH config 导入全流程，85 行）和 `:479-565`（paste + 重放隔离，86 行）整段被包住，`:369` 甚至连持久化模式都要 `ownedServer ? 'tmux' : 'none'` 分叉。

实测：`WMUX_BROWSER_URL`、`WMUX_BROWSER_BINARY`、`WMUX_BROWSER_OUTPUT_DIR`、`WMUX_BROWSER_USERNAME`、`WMUX_BROWSER_PASSWORD`、`CHROME_PATH` 这 6 个环境变量在 `README.md`、`docs/`、`.github/workflows/ci.yml`、`.env.example`、`package.json` 中**一次都没出现**。

**为什么是问题**：一个从未被使用、也没有文档的模式，让脚本一半的断言处于"可能不执行"状态。读者无法判断某段代码在 CI 里到底跑没跑。

**建议**：删掉 `WMUX_BROWSER_URL` 分支，脚本总是自建实例。7 个 `if (ownedServer)` 全部消失，`:369` 直接写 `'tmux'`。用户名/密码常量化即可（`:10-11`）。

---

### T-05　19 张截图在 CI 里不可达
**严重程度：中　类别：过度工程**
**位置：`scripts/browser-smoke.mjs:15-18, 141, 151, 174, 212, 220, 255, 297, 303, 319, 329, 338, 345, 351, 371, 458, 477, 581, 596, 605`、`.github/workflows/ci.yml:41`**

`outputDir` 默认是 `mkdtemp(tmpdir())`（`:16`），路径**只在成功时**通过 `:627` 的 JSON 打印出来。CI（`ci.yml:41`）既不设 `WMUX_BROWSER_OUTPUT_DIR`，也没有 `actions/upload-artifact` 步骤。

**为什么是问题**：截图唯一有用的时刻是失败时，而恰恰失败时脚本在 `:620` 或更早就 `throw` 了，路径永远不会被打印，临时目录随 runner 销毁。19 次 `fullPage: true` 截图是纯开销。

**建议**：二选一——(a) 删掉全部 `page.screenshot`；(b) 保留但把 `outputDir` 固定成 `<repo>/.playwright-output`，并在 `ci.yml` 加一步 `if: failure()` 的 `upload-artifact`。当前状态是两头不靠。

---

### T-06　`longFixtureName` 自检断言了一件不可能为假的事
**严重程度：低　类别：过度工程**
**位置：`scripts/browser-smoke.mjs:14, 33-35`**

```js
const longFixtureName = `wmux-${'x'.repeat(69)}`;
if (longFixtureName.length !== 74 || !/^[\x20-\x7e]+$/.test(longFixtureName)) {
  throw new Error(`long-name fixture must be exactly 74 printable ASCII characters: ...`);
}
```

上一行的两个字面量已经决定了长度必然是 `5 + 69 = 74`、字符必然是可打印 ASCII。而且 `74` 这个数字没有任何解释（`internal/api/types.go:147` 的名称上限是 80）。

**建议**：删掉 `:33-35`，在 `:14` 加一句注释说明"取略小于 80 字符上限的长度以验证长名称截断"。

---

### T-07　`modalChecks` 收集的度量值没有任何消费者
**严重程度：低　类别：过度工程**
**位置：`scripts/browser-smoke.mjs:31, 208, 216, 294, 299, 326, 332, 340, 347, 370, 577, 587, 593, 601, 637`**

13 处 `modalChecks.xxx = await assertDialogLayout(...)` 把返回的 `{height, width, body, buttonHeights, closeWidth}` 攒起来，最后在 `:637` 塞进成功时打印的 JSON。`assertDialogLayout` 内部已经在 `:733` 抛出所有失败，返回值不参与任何后续判断；CI 也不解析 stdout。

**建议**：让 `assertDialogLayout` 返回 `void`，删掉 `modalChecks` 及 `:622-644` 的 JSON 报告（`ok/screenshots/chrome/mobileOverflow/...` 同理没有消费者），改成一行 `console.log('browser smoke passed')`。

---

### T-08　`pnpm-workspace.yaml` 的 `allowBuilds` 里 3/4 的包不在依赖树中
**严重程度：中　类别：过度工程（死配置）**
**位置：`pnpm-workspace.yaml:1-5`**

```yaml
allowBuilds:
  cpu-features: false
  esbuild: true
  node-pty: false
  ssh2: false
```

实测 `pnpm-lock.yaml` 中 `cpu-features`、`node-pty`、`ssh2` 出现次数均为 **0**（只有 `esbuild` 命中 95 次）。这三个显然是早期 Node 版 PTY/SSH 原型的残留——而 wmux 的 PTY 和 SSH 全在 Go 侧（`github.com/creack/pty`、`golang.org/x/crypto/ssh`）。

**建议**：只保留 `esbuild: true`。

---

### T-09　三份互不同步的"配置面"文档，且 `.env.example` 无人消费
**严重程度：中　类别：风格（文档与代码不一致）**
**位置：`.env.example`、`compose.yaml:11-19`、`README.md:68-78`**

wmux 一共读 10 个 `WMUX_*` 变量（实测 `internal/config/config.go` 8 个 + `cmd/wmux/main.go:28` 的 `WMUX_LOG_LEVEL` + 会话内注入的 `WMUX_SESSION_ID`）。三处描述各不相同：

- `README.md:68-78` 表格：8 个（完整，事实上的源）
- `compose.yaml:11-19`：5 个 + 3 个注释掉的
- `.env.example`：7 个，**缺 `WMUX_SESSION_TTL` 和 `WMUX_SSH_CONFIG`**

更关键的是 `.env.example` 描述的 `.env` 文件**没有任何东西会读它**：`go.mod` 没有 dotenv 依赖，`compose.yaml` 没有 `env_file:`，Vite 只把 `VITE_` 前缀的变量给浏览器、不会传给 Go 进程，`package.json:13` 的 `dev:server` 就是裸 `go run`。`README.md` 全文没提过 `.env`。而 `deploy/wmux.service.example:11` 指向的是**另一个路径** `%h/.config/wmux/wmux.env`。

**建议**：删掉 `.env.example`（README 表格已经是完整文档），或者把它改名为 `deploy/wmux.env.example` 并在 systemd 示例和 README 里明确引用。

---

### T-10　`dist` 这个名字指两样东西
**严重程度：中　类别：风格（仓库布局）**
**位置：`internal/webui/dist/`（提交进 git，29 个文件 / 2.4 MB）、`/dist/`（gitignore，Go 二进制输出）**

- `internal/webui/dist/` = Vite 构建产物，被 `internal/webui/embed.go:8` 的 `//go:embed all:dist` 嵌入。
- `/dist/` = `scripts/build-server.sh:20-23` 的 Go 二进制输出目录，被 `.gitignore:4` 忽略，`package.json:18` 的 `start` 指向它。

结果是各种忽略文件里出现语义模糊的 `dist`：`.prettierignore:1-2` 两个都列了，`eslint.config.js:9` 两个都列了，`.dockerignore:3` **只列了顶层的那个**。读者无法从 `.dockerignore` 的 `dist` 一行判断它指哪个。

**建议**：把 Go 输出目录改名为 `bin/`（改 `scripts/build-server.sh:20,23`、`package.json:18`、`.gitignore:4`、`.dockerignore:3`、`.prettierignore:1`、`eslint.config.js:9`），`dist` 一词专属于前端产物。

**关于"把构建产物提交进 git"本身**：这个取舍我认为**成立**——`//go:embed all:dist` 要求目录在编译期存在，提交后 `go build ./cmd/wmux` 和 `go install .../cmd/wmux@latest` 无需 Node 工具链即可工作，对个人自托管场景是实打实的收益。`.gitattributes:1` 标了 `linguist-generated=true` 也做得对。

但**缺一道防线**：`ci.yml:36` 跑了 `pnpm build`（会重新生成 `internal/webui/dist`），却**没有** `git diff --exit-code internal/webui/dist` 检查。也就是说，如果有人改了 `client/src` 却忘了提交重建的 dist，CI 全绿而发布的二进制里是旧 UI。目前靠人工纪律维持（实测最近一次 `client/src` 变更和 `internal/webui/dist` 变更同在 `4fb3e9f`，纪律是守住了的）。

**建议**：在 `ci.yml:36` 后加一步
```yaml
- name: Committed web assets are up to date
  run: git diff --exit-code -- internal/webui/dist
```

---

### T-11　两条构建路径重复且不一致
**严重程度：中　类别：风格（重复代码）**
**位置：`Dockerfile:20-22` vs `scripts/build-server.sh:21-23`**

```
Dockerfile:20-22    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X ...version.Version=${VERSION} -X ...version.Commit=${COMMIT}"
build-server.sh:21  go build -trimpath -ldflags "-X ...version.Version=$version -X ...version.Commit=$commit"
```

`github.com/waterlens/wmux/internal/version.Version` 这条长路径在仓库里写了 4 次（两处各 2 次）。两条路径的产物还不同：Docker 带 `-s -w` 和 `CGO_ENABLED=0`，本地构建不带。

**建议**：Dockerfile 改成 `RUN sh scripts/build-server.sh`（脚本已经能从 `WMUX_VERSION`/`WMUX_COMMIT` 取值，见 `build-server.sh:8-9`），把 `-s -w` / `CGO_ENABLED=0` 也收进脚本，只留一处 ldflags。

---

### T-12　版本号在 `compose.yaml` 里被二次硬编码
**严重程度：低　类别：风格（文档/配置与代码不一致）**
**位置：`compose.yaml:6` vs `package.json:3`**

`compose.yaml:6` 写 `VERSION: ${WMUX_VERSION:-0.1.0}`，`package.json:3` 写 `"version": "0.1.0"`，而 `scripts/build-server.sh:7` 是从 `package.json` 读的。改版本号时 `compose.yaml` 会静默落后。

**建议**：`compose.yaml:6` 的 fallback 改成 `dev`（与 `internal/version/version.go:6` 的默认一致），把 `package.json` 作为唯一版本源。

---

### T-13　Node 版本在四处各说各话
**严重程度：低　类别：风格（文档与代码不一致）**

- `README.md:23`：`Node.js 22+`
- `package.json:9`：`"node": ">=22.13.0"`
- `Dockerfile:3`：`node:24-bookworm-slim`
- `.github/workflows/ci.yml:21`：`node-version: 24`

真正被验证过的只有 24（CI + Docker），22.13 从未被测试。

**建议**：统一到 24（`engines.node: ">=24"`，README 改成 `Node.js 24+`），或者在 CI 加一个 22.13 的矩阵项来兑现承诺。

---

### T-14　`test:all` 定义了却无人引用，CI 与 npm scripts 各写一遍流程
**严重程度：低　类别：风格（脚本结构混乱）**
**位置：`package.json:23, 26`、`.github/workflows/ci.yml:29-41`**

`package.json:26` 的 `"test:all": "pnpm test && pnpm build && pnpm test:browser"` 在整个仓库（含 README、CI、脚本）中**只出现在它自己的定义处**。

CI 没有用 `pnpm test`，而是把 `lint`/`typecheck`/`test:unit` 逐条重列（`ci.yml:31-33`），并把 `test:go` 换成 `go test -race`（`ci.yml:34`）。于是 `package.json:23` 的 `test` 和 CI 实际跑的东西是两套会各自漂移的定义。

**建议**：把 `test` 改成 `pnpm lint && pnpm typecheck && pnpm test:unit && go test -race ./... && go vet ./...`，CI 的 `:31-35` 五行合并成 `- run: pnpm test`；或者删掉 `test:all`，承认 CI 是唯一流程定义。

---

### T-15　格式化覆盖面靠手写路径列表维持，多个文件不受管
**严重程度：低　类别：风格**
**位置：`package.json:20-21`、`.prettierignore`**

```
prettier --write client scripts vite.config.ts package.json tsconfig.base.json tsconfig.client.json
```

未被覆盖的仓库根文件：`eslint.config.js`（JS 源码）、`.prettierrc.json`、`compose.yaml`、`.github/workflows/ci.yml`、`Dockerfile`。

反过来，`.prettierignore` 的 4 条里有 3 条是**够不着的**：显式路径列表根本不会遍历到 `dist`、`internal/webui/dist`、`pnpm-lock.yaml`。

**建议**：`package.json:20-21` 改成 `prettier --write .`，让 `.prettierignore` 真正发挥作用（并给它补上 `internal/webui/dist` 已有的条目即可）。

---

### T-16　`Dockerfile` 缩进 tab/空格混用
**严重程度：低　类别：风格**
**位置：`Dockerfile:26`**

`:26` 以 **tab** 开头（`	&& apt-get install ...`），`:27-29` 用 4 个空格。`.editorconfig:3-8` 对 `[*]` 规定 `indent_style = space` / `indent_size = 2`，而 Dockerfile 又不在 prettier 覆盖范围内（见 T-15），所以没有任何工具会发现。

---

### T-17　`.dockerignore` 没有排除 `internal/webui/dist`
**严重程度：低　类别：过度工程（多余的构建开销）**
**位置：`.dockerignore:3`、`Dockerfile:16-17`**

Docker 的忽略语法中裸 `dist` 只匹配上下文根的 `dist`，所以 2.4 MB 的 `internal/webui/dist` 会进构建上下文、被 `Dockerfile:16` 的 `COPY . .` 写进层，然后立刻被 `:17` 的 `COPY --from=web` 覆盖。

**建议**：`.dockerignore` 加一行 `internal/webui/dist`。

---

### T-18　`.gitignore` / `.dockerignore` 中的过时与投机条目
**严重程度：低　类别：过度工程（死配置）**
**位置：`.gitignore:6-7`、`.dockerignore:5`**

- `/client/dist/`（`.gitignore:7`）：`vite.config.ts:8` 的 `outDir` 是 `../internal/webui/dist`，仓库里没有任何东西写 `client/dist`。是 outDir 改动前的残留。
- `coverage/`（`.gitignore:6` + `.dockerignore:5`）：没有任何覆盖率工具——`package.json` 无 `@vitest/coverage-*` 依赖，无 `--coverage` 调用，CI 也没有。

**建议**：两条都删。

---

### T-19　`.editorconfig` 里有一段 `[Makefile]` 规则，但仓库没有 Makefile
**严重程度：低　类别：过度工程（死配置）**
**位置：`.editorconfig:15-16`**

`git ls-files` 与 `ls Makefile` 均确认不存在。

---

### T-20　`client/public/icon-192.svg` 无人引用，却被打进二进制
**严重程度：低　类别：过度工程（死资源）**
**位置：`client/public/icon-192.svg`、`internal/webui/dist/icon-192.svg`、`internal/webui/handler.go:29`**

全仓库检索（`*.html` / `*.ts` / `*.tsx` / `*.webmanifest` / `*.js` / `*.json`，排除 node_modules）**零引用**：
- `client/public/manifest.webmanifest:14-33` 只列了 `icon-192.png`、`icon-512.png`、`icon-512.svg`
- `client/public/sw.js:2-9` 的 `SHELL` 只列了 `icon-192.png`、`icon-512.png`、`icon-512.svg`、`apple-touch-icon.png`
- `client/index.html` 用的是内联 `data:` favicon

文件同时存在于源目录和提交的 dist 目录，被 `//go:embed` 打进二进制。

**建议**：删除 `client/public/icon-192.svg`（并在下次构建时从 dist 消失）。

---

### T-21　同一份法律声明维护了两个手工副本
**严重程度：低　类别：风格（重复内容）**
**位置：`client/THIRD_PARTY_NOTICES.md`（40 行）vs `client/public/third-party-notices.txt`（51 行）**

两份都完整包含 Nerd Fonts MIT 全文、同一个 WOFF2 SHA-256（`b9e4e681…`）、同一组上游链接，但**措辞和排版完全不同**，只能人工同步。消费方也分开：`README.md:100` 链 `.md`，`client/src/components/SettingsDialog.tsx:337` 链 `/third-party-notices.txt`。

（`client/public/third-party-notices.txt` 与 `internal/webui/dist/third-party-notices.txt` 二进制一致，那是 Vite 自动复制的，没问题。）

**建议**：只留 `.txt` 一份作为事实来源，`README.md:100` 直接链到 `client/public/third-party-notices.txt`，删掉 `.md`。

---

### T-22　Service Worker 的缓存机制换不来离线能力
**严重程度：低　类别：过度工程**
**位置：`client/public/sw.js:1-9, 43-75`**

`SHELL`（`:2-9`）只含 6 个 shell 资源；`:66` 的 `SHELL.includes(url.pathname)` 决定了 **`/assets/*` 的 JS/CSS bundle 永远不进缓存**。所以 `:61` 的离线兜底会返回一个缓存的 `/`，而这个 HTML 引用的 `/assets/index-*.js` 离线时必然 404 —— 离线路径事实上不工作。

`sw.js` 真正兑现的只有"可安装"（PWA 只需要存在 fetch handler）和一点导航加速。`:1` 的 `CACHE_NAME = 'wmux-shell-v6'` 还是纯手工维护的版本号，仓库里没有任何地方说明什么时候要 bump（已经到 v6 了）。

**建议**：要么把 `/assets/` 也纳入缓存（它们已经是内容哈希 + `immutable`，见 `internal/webui/handler.go:26-28`，非常适合 cache-first），让离线真正可用；要么把 `sw.js` 精简成只保留 install/activate/最小 fetch handler，不再假装有离线能力。当前是"有全套缓存代码但没有缓存收益"。

---

## 第二部分：跨包重复与一致性

### X-01　两套 ID 生成器，产出两种 ID 格式
**严重程度：中　类别：过度工程 + 风格（重复代码）**
**位置：`internal/store/store.go:139-145` vs `internal/api/types.go:176-182`**

```go
// internal/store/store.go:139
func newID() (string, error) {
    raw := make([]byte, 16)
    if _, err := rand.Read(raw); err != nil { return "", fmt.Errorf("generate id: %w", err) }
    return hex.EncodeToString(raw), nil
}

// internal/api/types.go:176
func newID(prefix string) (string, error) {
    value := make([]byte, 16)
    if _, err := rand.Read(value); err != nil { return "", fmt.Errorf("generate ID: %w", err) }
    return prefix + "_" + hex.EncodeToString(value), nil
}
```

函数体除前缀外完全相同，两条错误信息只差 `id` / `ID` 的大小写。

后果是 ID 格式在系统内不统一：
- host：`internal/store/hosts.go:17-23` 生成，裸 32 位十六进制
- 登录会话：`internal/store/auth_sessions.go:14,28`，裸十六进制
- 终端会话：`internal/api/sessions.go:44` 生成 `ses_<32hex>`，导致 `internal/store/sessions.go:25-31` 的兜底分支在生产路径上是死代码
- WebSocket 客户端：`internal/api/websocket.go:79`，`client_<32hex>`

**建议**：只保留一份带前缀的实现（放 `internal/store`，导出 `store.NewID(prefix string)`），`internal/api` 复用它；给 host 和 auth session 也加上 `host_` / `auth_` 前缀（需要一次迁移，或者接受历史 ID 无前缀）。

---

### X-02　SSH `host:port` 拼装散落在三个包
**严重程度：中　类别：风格（重复代码）**

| 位置 | 实现 |
| --- | --- |
| `internal/api/types.go:184-186` | `net.JoinHostPort(strings.Trim(address, "[]"), fmt.Sprint(port))` |
| `internal/app/runtime_repository.go:80` | `net.JoinHostPort(strings.Trim(host.Address, "[]"), strconv.Itoa(host.Port))` —— 同一表达式内联 |
| `internal/terminal/ssh.go:646-657` | `sshAddress(address string)`：反向操作，补默认端口 22 |

前两处逻辑完全相同（一个抽了函数一个没抽，且 `fmt.Sprint` vs `strconv.Itoa` 也不一致）；第三处同名但语义相反，读代码时极易混淆。

**建议**：在 `internal/sshx`（已经是 SSH 公共小工具包）导出一个 `sshx.Address(host string, port int) string`，三处统一调用；`internal/terminal/ssh.go:646` 的补端口逻辑改名为 `withDefaultPort` 以示区分。

---

### X-03　`internal/sshx` 是唯一用中文写错误信息的非 API 包
**严重程度：中　类别：风格（错误信息不一致）**

按包统计 `fmt.Errorf` / `errors.New` 中含中文的比例（已排除 `_test.go`）：

```
internal/api        15/17   ← 面向用户，合理
internal/sshx       14/15   ← 异常
internal/app         0/1
internal/config      0/31
internal/security    0/30
internal/sshconfig   0/44
internal/store       0/91
internal/terminal    0/75
internal/transcript  0/35
```

`internal/sshx/sshx.go:46,66,68,76,92,100,106,113,117,126,138,147,151,155` 全部是中文用户文案。这打破了整个代码库"底层包用英文技术错误、`internal/api` 负责翻译成中文用户文案"的清晰分层。

更直接的证据是**同一句中文在两个包里各写了一遍**：
- `internal/api/types.go:134`　`errors.New("不支持的 SSH 认证方式")`
- `internal/sshx/sshx.go:155`　`errors.New("不支持的 SSH 认证方式")`

另外 `internal/sshx/sshx.go:92` 用全角冒号和逗号（`"SSH host key 已变化：期望 %s，实际 %s"`），而同文件 `:106`、`:113` 用半角冒号（`"SSH 验证失败: %w"`）——同一文件内标点风格都不统一。

**建议**：把 `internal/sshx` 的 14 条改成英文技术信息（如 `sshx: dial host: %w`），中文文案统一在 `internal/api/hosts.go` 的 `upstreamError` 里按错误码产出——那里本来就已经在做这件事（`internal/api/hosts.go:168,192`）。

---

### X-04　错误信息前缀约定分裂成两派
**严重程度：中　类别：风格（错误处理不一致）**

统计"以 `<包名>: ` 开头的错误 / 全部错误"：

```
internal/transcript  35/35   全部带前缀
internal/terminal    72/75   全部带前缀
internal/sshconfig   26/44   全部带前缀（其余在 parser.go 内层，由外层统一包装）
internal/store        4/91   只有 4 个哨兵 var 带前缀
internal/security     4/30   只有 4 个哨兵 var 带前缀
internal/config       1/31   只有 1 个哨兵 var 带前缀
internal/api          0/17
internal/app          0/1
internal/sshx         0/15
```

两种约定并存：
- **A 派**（transcript / terminal / sshconfig）：所有错误都带包名前缀
- **B 派**（store / security / config）：只有导出的哨兵 `var Err*` 带前缀，包装错误不带

同一个包里也不一致，例如 `internal/security/security.go:39-42` 是 `"security: AES-256 key must be exactly 32 bytes"`，而 `:53,60,63,69,74,...` 是 `"master key path is empty"`、`"create master key directory: %w"`。

**为什么是问题**：错误冒泡到 `cmd/wmux/main.go:35` 的 `logger.Error("wmux stopped", "error", err)` 时，一半错误能看出来源包，一半看不出来。

**建议**：选 A 派（Go 生态更常见，且已经是多数包的做法），把 `store`/`security`/`config`/`app`/`sshx` 的包装错误补上前缀。

---

### X-05　10 处 `fmt.Errorf` 没有格式动词
**严重程度：低　类别：风格**

`go vet` 不报，但这些应该是 `errors.New`：

```
internal/config/config.go:50   fmt.Errorf("config lookup function is nil")
internal/config/config.go:55   fmt.Errorf("WMUX_HOST must not be empty")
internal/config/config.go:63   fmt.Errorf("WMUX_PORT must be between 1 and 65535")
internal/config/config.go:83   fmt.Errorf("WMUX_PUBLIC_URL must be an absolute URL")
internal/config/config.go:86   fmt.Errorf("WMUX_PUBLIC_URL must use http or https")
internal/config/config.go:105  fmt.Errorf("WMUX_SESSION_TTL must be positive")
internal/config/config.go:131  fmt.Errorf("configuration contains an empty data path")
internal/config/lock.go:28     fmt.Errorf("data directory is empty")
internal/security/security.go:53   fmt.Errorf("master key path is empty")
internal/security/security.go:123  fmt.Errorf("master key must be a regular file and not a symlink")
```

这两个包同时也在用 `errors.New`（`internal/config/lock.go:12`、`internal/security/security.go:39-42`），所以是包内自相矛盾。

---

### X-06　"backend" 一词在代码库里有三个互不相干的含义
**严重程度：中　类别：风格（命名不一致）**

| 含义 | 位置 |
| --- | --- |
| ① PTY / SSH channel 这个 io 流 | `internal/terminal/backend.go:19` `type backend interface`；`internal/terminal/types.go:50` `ErrUnavailable = errors.New("terminal: backend is unavailable")` |
| ② 持久化种类（tmux/screen/none） | `internal/store/models.go:71` `Session.Backend`；SQL 列 `backend`（`internal/store/migrations.go:52`）；`internal/api/websocket.go:39` `socketEvent.Backend`（在 `:327` 由 `status.Persistence` 填充）；`client/src/types.ts:41` `Session.backend` |
| ③ tmux/screen 的会话名 | `internal/terminal/backend.go:154,168` `backendName`/`BackendName`；`internal/store/models.go:72` `Session.BackendName`；`internal/terminal/types.go:53` `ErrBackendMissing` |

同一个概念②在 `internal/terminal` 里叫 `Persistence`（`internal/terminal/types.go:12`），跨过 `internal/app/runtime_repository.go:113-115` 的边界后就改叫 `Backend`。读 `docs/architecture.md:29`（"backend identifiers"）时无法判断指的是哪一个。

**建议**：② 统一叫 `persistence`（DB 列可保留 `backend` 但加注释；wire 字段可保留兼容）；③ 改名为 `muxSessionName` / `MuxSessionName`（`ErrBackendMissing` → `ErrMuxSessionMissing`）；① 保持 `backend`，它是唯一名副其实的。

---

### X-07　前端存在两套互相冲突的会话状态词表
**严重程度：中　类别：风格（重复代码 + 用户可见不一致）**
**位置：`client/src/sessionStatus.ts:3-10` vs `client/src/components/TerminalView.tsx:72-80`**

| state | `sessionStatus.ts`（侧栏/工作区用） | `TerminalView.tsx`（终端工具栏用） |
| --- | --- | --- |
| connecting | 启动中 | 正在连接 |
| running | 运行中 | **已连接** |
| reconnecting | 重连中 | 正在重连 |
| detached | 已分离 | 已分离 |
| exited | 已结束 | 已退出 |
| error | 异常 | 连接错误 |
| offline | —— | 已断开 |

6 个状态里 5 个措辞不同。同一时刻，`client/src/components/Sidebar.tsx:177` 显示"运行中"，而 `TerminalView` 的工具栏显示"已连接"。

`sessionStatus.ts` 是明确的共享模块（被 `Workspace.tsx:5` 和 `Sidebar.tsx:18` 引用），`TerminalView.tsx` 却在组件内联了一份竞争实现。

**建议**：把 `statusText` 合并进 `client/src/sessionStatus.ts`，用一份词表 + 一个可选的"终端视角"重载（如果确实需要 running→"已连接"的语境差异，就显式命名为 `liveStatusLabel` 并放在同一文件里，让差异一眼可见）。

---

### X-08　协议兼容分支为服务端永不产生的取值而存在，且有测试把它钉住
**严重程度：中　类别：过度工程**

**(a) writer 字段的三个别名**——`client/src/terminalProtocol.ts:187-194`：
```ts
const writer = typeof record.writer === 'boolean' ? record.writer
  : typeof record.isWriter === 'boolean' ? record.isWriter
  : typeof record.writable === 'boolean' ? record.writable : undefined;
```
服务端只发 `writer`（`internal/api/websocket.go:41` `Writer *bool \`json:"writer,omitempty"\``）。全仓库 Go 侧检索 `isWriter` / `writable`：**0 命中**。

**(b) 三个"历史" state 值**——`client/src/terminalProtocol.ts:173-174`：
```ts
if (value === 'disconnected') return 'reconnecting';
if (value === 'terminated' || value === 'stopped') return 'exited';
```
`internal/api/websocket.go:337-352` 的 `publicTerminalState` 把所有内部状态映射到 `connecting|running|reconnecting|error|exited` 五个值，`disconnected` / `terminated` 是纯内部枚举（`internal/terminal/types.go:26,30`）从不出线；`stopped` 在 Go 侧**根本不存在**。

而且 `client/src/terminalProtocol.test.ts:100-104, 108, 115` 专门写了测试来锁定这些死分支。

**为什么是问题**：前后端在同一个二进制里发布（`internal/webui/embed.go` 嵌入构建产物），不存在版本错配窗口。这是为不可能发生的情形写的防御代码，还额外用测试固化了它。

**建议**：删掉 `terminalProtocol.ts:173-174` 和 `:188-194` 的别名链（简化为 `typeof record.writer === 'boolean' ? { writer: record.writer } : {}`），一并删掉 `terminalProtocol.test.ts:100-104` 和 `:108,115` 的相应断言。

---

### X-09　协议常量跨语言重复，且 TS 侧退化成魔法数字
**严重程度：中　类别：风格（重复定义 + 魔法数字）**

| 概念 | Go | TypeScript |
| --- | --- | --- |
| 输入帧上限 128 KiB | `internal/api/websocket.go:21` `maxSocketMessage = 128 << 10` | `client/src/terminalProtocol.ts:14` `MAX_INPUT_FRAME_BYTES = 128 * 1024` |
| 客户端输入帧标记 `0x00` | `internal/api/websocket.go:19` `clientInputFrame = byte(0)` | `client/src/terminalProtocol.ts:35` 裸字面量 `packet[0] = 0` |
| 服务端输出帧标记 `0x01` | `internal/api/websocket.go:20` `serverOutputFrame = byte(1)` | `client/src/terminalProtocol.ts:81` 裸字面量 `packet[0] !== 1` |
| 输出帧头 9 字节（1 + 8） | `internal/api/websocket.go:394-397` 裸 `9`；`internal/api/terminal_test.go:68-69` 又一处裸 `9` | `client/src/terminalProtocol.ts:81,82,84` 三处裸 `9` |

两个问题叠加：(1) 常量在两种语言各定义一份，**双方都没有注释指向对方**，只有 `docs/architecture.md:49` 用散文描述；(2) 常量纪律不对称——Go 给帧标记起了名字、TS 用裸数字；9 字节头两边都是裸数字。

**建议**：TS 侧补齐命名常量（`CLIENT_INPUT_FRAME = 0`、`SERVER_OUTPUT_FRAME = 1`、`OUTPUT_FRAME_HEADER_BYTES = 9`），Go 侧补 `outputFrameHeaderBytes = 9`；两边各加一行注释 `// keep in sync with client/src/terminalProtocol.ts`（反之亦然）。

---

### X-10　默认终端尺寸在两个包里硬编码成两个不同的值
**严重程度：中　类别：风格（魔法数字 + 跨包不一致）**

- `internal/store/sessions.go:298-303`：`Cols = 120` / `Rows = 36`
- `internal/terminal/local.go:399-407`：`cols = 80` / `rows = 24`

四个数字都是裸字面量，没有命名常量，两处对"默认终端尺寸"的答案不一致。另有第三处相关约束 `internal/api/types.go:172-174` `validSize`：`20..1000` × `5..500`，同样是裸数字。

（`terminalSize` 本身被 `local.go:45`、`manager.go:367`、`ssh.go:81` 三处复用，包内抽象是对的。）

**建议**：在 `internal/store` 或一个共享的常量位置定义 `DefaultCols` / `DefaultRows`，两个包引用同一对值。

---

### X-11　`reconnectDelay` 重新实现了 `NewManager` 已经保证过的默认值，且该分支不可达
**严重程度：低　类别：过度工程（不可能发生的防御）**
**位置：`internal/terminal/backend.go:172-178` vs `internal/terminal/manager.go:38-43`**

```go
// manager.go:38-43 —— NewManager 已经归一化
if cfg.ReconnectMin <= 0 { cfg.ReconnectMin = 250 * time.Millisecond }
if cfg.ReconnectMax <= 0 { cfg.ReconnectMax = 10 * time.Second }

// backend.go:173-178 —— 又写一遍同样的两个字面量
if minimum <= 0 { minimum = 250 * time.Millisecond }
if maximum <= 0 { maximum = 10 * time.Second }
```

`reconnectDelay` 的**唯一调用点**是 `internal/terminal/manager.go:708`，参数就是 `s.manager.cfg.ReconnectMin/Max`，必然已 > 0。两个 `if` 永远不成立。

**建议**：删掉 `backend.go:173-178`（保留 `:179-181` 的 `maximum < minimum` 归一，那个是真的可能发生）。

---

### X-12　`terminal.IsPermanentStartError` 是死导出
**严重程度：低　类别：过度工程**
**位置：`internal/terminal/backend.go:145-146`**

```go
// IsPermanentStartError reports whether a retry would repeat the same failure.
func IsPermanentStartError(err error) bool { return isPermanentStartError(err) }
```

全仓库（含 `_test.go`）检索：除自身定义外**零引用**。它只是把 `:140-143` 的 `isPermanentStartError` 再导出一次。

对照：同文件 `:168-170` 的 `BackendName` 是同样的一行再导出模式，但它**有**真实使用者（`internal/app/runtime_repository.go:115`、`internal/api/lifecycle_test.go:369,677`），所以是合理的。

**建议**：删掉 `backend.go:145-146`。

---

### X-13　"生成安全的 mux 名称"的清洗逻辑写了三遍，长度上限各不相同
**严重程度：低　类别：风格（重复代码）**

| 位置 | 做法 | 长度上限 |
| --- | --- | --- |
| `cmd/wmux/main.go:157-160` `dataMuxName` | `sha256(dataDir)[:4]` → `wmux-%x` | 无 |
| `internal/terminal/backend.go:59-68` `newLauncher` | 正则替换非法字符 → trim `-` → 截断 | 32 |
| `internal/terminal/backend.go:154-165` `backendName` | 正则替换 → trim `-` → 截断 → 拼 `sha256(id)[:4]` → `wmux-%s-%x` | 40 |

三处都在做"把任意字符串变成安全的 tmux/screen 标识符"，`:60-67` 与 `:155-162` 几乎逐行相同（只差上限 32 vs 40 和空串兜底 `"wmux"` vs `"session"`），`main.go` 的第三处又用了完全不同的 `wmux-` 前缀构造。

**建议**：在 `internal/terminal` 导出一个 `SafeMuxName(value string, limit int) string`，三处共用；`cmd/wmux/main.go:157-160` 改为调用它。

---

### X-14　同一组枚举在跨包时有三级不同的类型纪律
**严重程度：低　类别：风格（命名/类型不一致）**

以持久化模式为例：

| 层 | 写法 |
| --- | --- |
| `internal/terminal/types.go:12-19` | 命名类型 + 常量：`type Persistence string`；`PersistenceAuto Persistence = "auto"` |
| `internal/store/models.go:13-16` | **无类型**字符串常量：`SessionPersistenceAuto = "auto"` |
| `internal/api/types.go:164-168` | 裸字面量 switch：`case "auto", "tmux", "screen", "none":` |
| `client/src/types.ts:29` | `type PersistenceMode = 'auto' \| 'tmux' \| 'screen' \| 'none'` |
| `client/src/api.ts:55` | `z.enum([...])`（第二份） |

会话状态同理散在 `internal/store/models.go:18-23`、`internal/store/sessions.go:336-348`、`internal/terminal/types.go:23-31`、`internal/api/websocket.go:337-352`、`internal/app/runtime_repository.go:91-103`、`client/src/types.ts:30`、`client/src/api.ts:58`、`client/src/terminalProtocol.ts:1` —— **8 处**。

认证方式散在 `internal/api/types.go:123-135`、`internal/store/models.go:6-8`、`internal/app/runtime_repository.go:168-177`、`internal/sshx/sshx.go:122-155`、`client/src/types.ts:13`、`client/src/api.ts:24` —— **6 处**。

分层校验本身可以接受，但"同一个枚举在 Go 侧有三种类型纪律"纯属风格失控。

**建议**：`internal/store/models.go` 至少给这几组常量加命名类型（`type SessionPersistence string` 等）；`internal/api/types.go:164-168` 改用 `store.SessionPersistence*` 常量而不是裸字面量。

---

### X-15　结构化日志字段名 `session` vs `session_id`
**严重程度：低　类别：风格（不一致）**

全部与会话相关的日志用 `"session"`：
`internal/api/sessions.go:150,152,163,237`、`internal/app/runtime_repository.go:132,119`（经 `logError`）、`:205`（经 `logStateChange`）

唯一例外：`internal/api/websocket.go:97` 用 `"session_id"`。

**建议**：改成 `"session"`。

---

### X-16　`WMUX_LOG_LEVEL` 绕过 `internal/config`
**严重程度：低　类别：风格（不一致）**
**位置：`cmd/wmux/main.go:28, 40-53` vs `internal/config/config.go:48-120`**

其余 8 个 `WMUX_*` 变量全部经 `internal/config` 的 `valueOr` / `boolValue` / `intValue` / `durationValue` 统一读取与校验，只有 `WMUX_LOG_LEVEL` 在 `main.go:28` 直接 `os.Getenv`，并在 `main.go:40-53` 单写一个 `parseLogLevel`。这也是为什么它在 `internal/config/config.go` 里检索不到。

后果：`internal/config/config_test.go` 覆盖不到日志级别解析；`config.Config` 结构体不完整地描述了配置面。

**建议**：把 `LogLevel` 移进 `config.Config`（`main.go` 需要先解析配置再建 logger，可以先用一个临时 logger 报配置错误，或让 `parseLogLevel` 留在 main 但从 `cfg.LogLevel` 取值）。

---

### X-17　`internal/api/server.go` 里三种依赖注入风格并存，且其中一种是无用的
**严重程度：中　类别：过度工程**
**位置：`internal/api/server.go:52-72`、`internal/api/hosts.go:162-165, 186-189`**

同一个构造函数里有三套 DI：

1. **可变参数可选覆盖**（`:52` `providers ...sessionSpecProvider`）：只读 `providers[0]`（`:57-59`），多余元素静默丢弃。实测**所有调用点**：`cmd/wmux/main.go:112`（传 1 个）、`auth_test.go:28,100`、`lifecycle_test.go:64`、`ssh_config_test.go:82`、`terminal_test.go:48`（都不传）。也就是说，**没有任何测试用过这个覆盖能力**，而唯一使用者传进来的东西与 `:56` 的默认构造在功能上等价。副作用：生产路径每次 `New` 都会构造一个 `&app.RuntimeRepository{...}` 然后立刻丢弃。

2. **字段默认 + 测试直接赋值**（`:70` `sshConfig: sshconfig.New(...)`）：`internal/api/ssh_config_test.go` 有 15 处 `server.sshConfig = ...`。

3. **字段默认 + handler 内再兜一次底**（`:71` `probeSSH: sshx.Probe`）：`internal/api/hosts.go:162-165` 和 `:186-189` 各写了一遍
   ```go
   probe := s.probeSSH
   if probe == nil { probe = sshx.Probe }
   ```
   而 `:71` 保证了经 `New` 构造的 `Server` 上它永不为 nil。于是 `sshx.Probe` 这个默认值在三个地方各写一次，其中两处是不可达的防御代码。

**建议**：
- 删掉 `:52` 的 `providers ...sessionSpecProvider` 和 `:57-59`，`New` 始终自建 repository；`cmd/wmux/main.go:112` 去掉第 7 个参数。若确实需要共享同一个实例，就把它改成一个**必填**参数。
- 删掉 `internal/api/hosts.go:162-165` 和 `:186-189` 的 nil 兜底，直接用 `s.probeSSH`（与 `sshConfig` 的用法一致）。

---

### X-18　测试夹具在包内与跨包多次重复
**严重程度：低　类别：过度工程（测试脚手架）**

**(a) 开测试库的样板重复 7 次**
`internal/api/auth_test.go:22`、`:95`、`internal/api/terminal_test.go:30`、`internal/api/sessions_test.go:14`、`internal/api/ssh_config_test.go:73`、`internal/api/lifecycle_test.go:42`、`internal/app/runtime_repository_test.go:17`。前 6 处在同一个包里，每处都自带一份 `if err != nil { t.Fatal(err) }` + `t.Cleanup`。

**(b) `internal/api/terminal_test.go:25-53` 整段复制了 `newLiveAPIFixture`**
`internal/api/lifecycle_test.go:39-74` 已有 `newLiveAPIFixtureWithReplayLimit`，`terminal_test.go:29-52` 逐行做了同样的事（开 store → 建 transcript 目录 → discard logger → `bytes.Repeat` key → `RuntimeRepository` → `NewManager` → `api.New` → `httptest.NewServer` → `setupOverHTTP`）。**同一个包内**的完全重复。

**(c) 三份"轮询 manager.Status 直到某状态"**
- `internal/terminal/lifecycle_test.go:1123-1141` `waitState`（1ms ticker，单一目标状态）
- `internal/terminal/manager_test.go:503-520` `waitRunning`（5ms ticker，硬编码 `StateRunning`）—— 等价于 `waitState(t, ctx, m, id, StateRunning)`
- `internal/api/lifecycle_test.go:860-878` `waitForTerminalState`（5ms `time.After`，变参目标）

前两个在同一个包里，是纯粹的重复；第三个跨包，无法直接共用，但可以由 `internal/terminal` 导出一个测试辅助。

**(d) 主密钥长度：命名常量 vs 裸 32**
`internal/app/runtime_repository_test.go:148,229,241` 用 `security.MasterKeySize`（`internal/security/security.go:28`），而 `internal/api/terminal_test.go:40`、`auth_test.go:28,100`、`lifecycle_test.go:51`、`ssh_config_test.go:82` 用裸 `32`。

**建议**：`internal/api` 建一个 `helpers_test.go`，把 (a) 和 (b) 收敛成一个 `newAPIFixture(t, opts)`；`terminal` 包把 `waitRunning` 换成 `waitState(..., StateRunning)`；全部裸 `32` 换成 `security.MasterKeySize`。

---

### X-19　`internal/config/lock_fallback.go` 为一个不受支持的平台而存在
**严重程度：低　类别：过度工程**
**位置：`internal/config/lock_fallback.go:1-25`、`internal/config/lock.go:20, 83-87`**

构建约束是 `!darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd`。实测交叉编译：

```
GOOS=windows  → 编译通过
GOOS=solaris  → 失败（modernc.org/libc 不支持）
GOOS=aix      → 失败
GOOS=plan9    → 失败
```

所以这个 fallback 唯一覆盖的平台是 **Windows**——而 wmux 的核心功能在 Windows 上完全不可用（`github.com/creack/pty` 在 Windows 上运行期直接报不支持，`internal/terminal/local.go:87` 的 `pty.StartWithSize` 是必经路径），README、Dockerfile（debian）、`deploy/wmux.service.example`（systemd）也从未提及 Windows，CI 也不构建它。

连带效应：`DataDirLock.removeOnClose` 字段（`lock.go:20`）只被 `lock_fallback.go:22` 设置过，`lock.go:83-87` 的整个分支在受支持平台上是死代码。

**建议**：把 `lock_unix.go:1` 的构建标签简化为 `//go:build unix`，删除 `lock_fallback.go` 与 `removeOnClose` 字段及其 `Close()` 分支。非 Unix 平台本来就构建不出可用的 wmux。

---

### X-20　前端头像首字母渲染重复，且无障碍属性不一致
**严重程度：低　类别：风格（重复代码）**
**位置：`client/src/components/Sidebar.tsx:251` vs `client/src/components/SettingsDialog.tsx:234-236`**

```tsx
// Sidebar.tsx:251
<span className="user-avatar">{user.username.slice(0, 1).toUpperCase()}</span>

// SettingsDialog.tsx:234-236
<span className="user-avatar" aria-hidden="true">
  {user.username.slice(0, 1).toUpperCase()}
</span>
```

复制粘贴的结果是 `aria-hidden` 只加了一处——同一个纯装饰元素在两个地方对辅助技术呈现不同。`client/src/components/UI.tsx` 已经是共享 UI 原语模块（导出 Button/Field/Input/Select/Textarea/Modal/ActionMenu/ConfirmDialog/EmptyState/ToastStack）。

**建议**：在 `UI.tsx` 加一个 `export function UserAvatar({ username })`，两处共用，`aria-hidden="true"` 内置。

---

### X-21　`client/src/api.ts` 的若干小问题
**严重程度：低　类别：过度工程 + 风格**

**(a) 死错误码**：`client/src/api.ts:207` 的 `terminal_config` 在整个仓库（Go/TS/mjs/md，排除 node_modules 与 dist）**只出现在这一行**。服务端实际发出的 23 个错误码里没有它。删除。

**(b) `body()` 是 `JSON.stringify` 的无意义别名**：`client/src/api.ts:146-148` 定义 `function body(value: unknown): string { return JSON.stringify(value); }`，被调用 9 次。既没有加任何逻辑，函数名 `body` 又与 `RequestInit.body` 混淆。直接用 `JSON.stringify` 更清楚。

**(c) `schemas` 导出只为测试而存在**：`client/src/api.ts:219-226` 导出 6 个 schema，唯一消费者是 `client/src/api.test.ts:204-206`，而且只用了其中的 `sessionSchema`。为测试扩大生产模块的 API 面。建议改成只导出 `sessionSchema`，或让测试从内部路径导入。

---

## 看起来复杂但其实合理，不建议改

1. **把 `internal/webui/dist/` 提交进 git**（29 文件 / 2.4 MB）。`//go:embed all:dist` 要求编译期目录存在；提交后 `go build ./cmd/wmux` 与 `go install .../cmd/wmux@latest` 完全不需要 Node 工具链，对"单二进制自托管"这个产品定位是核心收益。`.gitattributes:1` 的 `linguist-generated=true` 也处理到位。（唯一缺口见 T-10 建议的 CI 检查。）

2. **`internal/app` 只有一个文件**。实测 `internal/terminal/manager.go:1-13` 的 import 里没有 `internal/store`——这是刻意的：`terminal` 不认识产品存储，`app.RuntimeRepository` 是唯一的适配层。为了守住这条依赖边界，单文件包完全值得。

3. **`internal/version` 单独成包**（8 行）。`-ldflags -X` 需要一个包路径作为注入目标，而 `cmd/wmux/main.go:122`、`internal/api/server.go:117`、`internal/api/auth.go:29-30` 三处都要读它，所以它不能待在 `main` 里，也不适合塞进 `internal/config`（那会让配置包承担构建元数据职责）。合理。
   （唯一多余的是 `internal/version/version_test.go` —— 它断言正上方两行字面量初始化的 `var` 非空，属于无信息量的测试，但只有 11 行，删不删无所谓。）

4. **`sshx` / `sshconfig` / `terminal/ssh.go` 的三分边界**。职责划得很清楚：`internal/sshx/sshx.go:1-3` 的包注释明确写了"短生命周期的主机管理操作，交互式终端连接在 internal/terminal"；`sshconfig` 只做只读的 OpenSSH 配置解析、完全不建网络连接。三者依赖方向也是单向的。这个切分是对的。

5. **`internal/terminal` / `store` / `transcript` / `config` / `security` / `sshconfig` 完全不打日志**，只返回错误，日志集中在 `cmd/wmux`、`internal/api`、`internal/app` 三个上层。这是很干净的分层，值得保持。

6. **`browser-smoke.mjs` 的 python3 探针**（`:486-511` bracketed paste、`:524-542` 重放隔离、`:804-807` base64 包装）。这两件事——真实 PTY 的 `\x1b[200~` 分帧完整性、reload 后历史 `CSI 5n` 回复是否泄漏进当前 PTY——**无法**用 jsdom 验证，必须要真实终端往返。base64 包装也确实是把多行 Python 塞进单行 shell 命令的合理做法。`:484` 那句 `if (pasteBytes <= 128 * 1024)` 断言也合理：字节数不是从 `12_000` 这个字面量一眼可见的，这个检查在有人调小重复次数时会真的触发。

7. **`terminal.Config` 的 `TmuxPath` / `ScreenPath`** 生产代码从不设置，只被 `internal/terminal/local_test.go` 和 `manager_test.go` 用于注入。这是标准的测试可替换性设计，不是冗余配置项。

8. **`internal/webui/handler.go:24-34` 的 Cache-Control 分支**。`assets/` 走 `immutable`、`index.html`/`sw.js`/manifest/图标走 `no-cache`，这是内容哈希构建 + PWA 的正确缓存策略，不能简化。

9. **`eslint.config.js:31-45` 给 `client/public/sw.js` 和 `scripts/**/*.mjs` 单独配 globals**。Service Worker 和 Node 脚本的全局对象确实和浏览器不同，这三段配置每段只有几行，没有冗余。

10. **`internal/security` 这个包名**虽然宽泛，但包注释（`security.go:1-3`）已经明确界定为"密钥管理、口令哈希、会话令牌，独立于 HTTP 与存储"，316 行内容也确实只有这三件事。改名为 `crypto` 反而会和标准库冲突。保持现状。

11. **`compose.yaml:22-27` 那段关于 SSH config 挂载的长注释**。它警告了一个真实且不显然的风险（挂整个 `~/.ssh` 会让 Web 本地 shell 读到私钥），与 `README.md:51` 呼应。注释长但每一句都在传递必要信息。
