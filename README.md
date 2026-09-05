# wmux

wmux 是一个面向个人自托管场景的 Web 终端：在浏览器中管理本机 shell 和多台 SSH 主机，并让多个终端 session 在关闭页面、切换设备或网络中断后继续运行。

## 功能

- 本机 PTY 与多 SSH 主机统一管理
- 多 session、浏览器标签和断线输出补发
- `tmux` / `screen` 持久化；不可用时可降级为直接 PTY
- 单写多读：同一 session 可在多个设备打开，并显式接管输入权
- SSH 密码、加密私钥及 `SSH_AUTH_SOCK` 三种认证方式
- 发现运行用户的 OpenSSH config，并由管理员显式导入候选主机
- SSH host key 指纹探测、人工信任及变更拒绝
- SQLite 元数据、加密凭据与有上限的终端历史文件
- 首次初始化、密码登录、会话过期及登录限流
- 桌面、平板和手机响应式界面，支持 PWA 安装
- 默认使用清爽的白色应用界面；终端画布保留 xterm/程序自己的 ANSI 配色
- tmux 鼠标模式、OSC 8 链接、Nerd Font 图标与 Unicode 11 字符宽度
- 单个 Go 二进制部署，内嵌完整 Web UI

## 快速开始

需要 Go 1.26+、Node.js 22+、pnpm，以及 `tmux` 或 `screen`。前端只在构建时需要 Node.js。

```bash
pnpm install
pnpm build
WMUX_DATA_DIR="$PWD/.wmux" ./dist/wmux
```

默认监听 `127.0.0.1:8787`。打开 <http://127.0.0.1:8787>，首次访问时创建管理员账户。

原生运行最适合“本机终端”：shell 与 wmux 运行在同一台宿主机，并可使用当前用户的 `SSH_AUTH_SOCK`。服务启动用户必须有权访问希望操作的文件。

## Docker

```bash
docker compose up -d --build
```

镜像内置 `tmux`，数据保存在 `wmux-data` volume。注意：容器中的“本机”是容器本身，不是 Docker 宿主机；重启整个容器会保留会话元数据和历史，但会结束容器内的本机进程。若希望控制宿主机并让本机会话跨 wmux 服务重启继续运行，推荐在宿主机原生运行 wmux，或把宿主机作为 SSH 主机添加。

## 从 OpenSSH config 导入主机

wmux 可以只读发现其运行系统账户的 `~/.ssh/config`；也可通过 `WMUX_SSH_CONFIG` 指向另一份配置文件。这里的 `~`、`%d` 和相对 `Include` 都以该账户记录的 home 为准，不受服务进程的 `HOME` 环境变量影响。发现结果不会自动写入数据库或连接网络，只有登录后的管理员显式选择别名并导入时，wmux 才会重新解析该别名并创建主机记录。

导入只复制别名、地址、端口和用户名，认证方式固定为 SSH agent。`IdentityFile` 只会显示“已配置”的提示，其路径和文件内容不会进入 API 或数据库；导入也不会读取私钥、复制密码、探测网络或自动信任 host key。首次连接仍需可用的 agent 或之后手工配置凭据，并通过 wmux 独立的指纹探测与确认流程。`ProxyJump`、`ProxyCommand` 等会改变连接路径或执行外部命令的配置目前不支持，相应候选不会被导入。

解析遵循 OpenSSH 的大小写敏感 `Host` 匹配、通配/否定规则、首值优先和 Include 顺序。为避免 discovery 执行本地命令，首版只安全应用 `Match all`，其他 `Match` 条件均忽略；条件 `Include` 会在解析已知别名时按需读取，但其中新声明的别名不会额外出现在候选列表中。

Docker 部署不要挂载整个宿主机 `~/.ssh`，否则 Web 本地 shell 也可能读取其中的私钥和 `known_hosts`。若需发现配置，只分别只读挂载主 config 以及它实际引用的配置片段，例如 compose 文件中的注释示例；不要挂载 `IdentityFile`、私钥或 `known_hosts`。容器以 UID `10001` 运行，宿主文件及其父目录必须允许该 UID 读取和遍历。

## 安全地远程访问

推荐通过 WireGuard、Tailscale 或其他私有网络访问。远程地址仍应在 wmux 前配置 HTTPS：除 `localhost` 等浏览器认可的本机来源外，工具栏的复制/粘贴依赖 Secure Context，普通私网 IP 的 HTTP 页面无法获得浏览器剪贴板权限。若需要域名访问，请设置：

```env
WMUX_HOST=127.0.0.1
WMUX_PUBLIC_URL=https://terminal.example.com
WMUX_COOKIE_SECURE=true
WMUX_TRUST_PROXY=true
```

仓库的 [`deploy/Caddyfile.example`](deploy/Caddyfile.example) 提供了最小 Caddy 配置。直接键盘输入和浏览器原生粘贴不依赖工具栏的 Async Clipboard API，但远程访问仍不应使用明文 HTTP，也不要将未启用 TLS 的 wmux 端口直接暴露到公网。

## 配置

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `WMUX_HOST` | `127.0.0.1` | 监听地址 |
| `WMUX_PORT` | `8787` | 监听端口 |
| `WMUX_DATA_DIR` | `.wmux` | SQLite、主密钥和历史目录 |
| `WMUX_PUBLIC_URL` | 空 | 浏览器访问的规范 origin，用于 CSRF/WebSocket 校验 |
| `WMUX_COOKIE_SECURE` | 随 Public URL | 是否只通过 HTTPS 发送登录 Cookie |
| `WMUX_TRUST_PROXY` | `false` | 是否信任反向代理提供的协议及客户端地址 |
| `WMUX_SESSION_TTL` | `168h` | 登录有效期 |
| `WMUX_LOG_LEVEL` | `info` | 日志级别：`debug`、`info`、`warn` 或 `error` |
| `WMUX_SSH_CONFIG` | 空（`~/.ssh/config`） | 用于只读发现候选主机的 OpenSSH config 路径 |

数据目录权限会被收紧到 `0700`，其中 `master.key` 为 `0600`。SSH 密码、私钥和 passphrase 使用该主密钥通过 AES-256-GCM 加密。备份时必须同时保存数据库和主密钥；丢失主密钥后加密凭据无法恢复。

同一数据目录同一时间只允许一个 wmux 进程使用，以免多个进程同时写终端历史。未设置 `WMUX_PUBLIC_URL` 时，浏览器写请求只接受通过字面 IP 地址或 `localhost` 访问；若使用自定义域名（包括内网 DNS 名），请显式设置规范 Public URL。

## Session 持久化

本机和 SSH session 的 `auto` 模式依次选择：

1. `tmux`
2. `screen`
3. 直接 PTY

前两种在浏览器断开和 wmux 服务重启后均可重新 attach。wmux 使用自己的 tmux/screen 配置与服务端点，不会出现在用户默认的 tmux server 中，也不会继承用户的状态栏或快捷键配置。隔离的 tmux server 会启用鼠标模式，使滚轮由 tmux/Vim 等 TUI 正确处理，而不是在 shell 中退化成方向键。直接 PTY 可以跨浏览器断线继续，但 wmux 进程退出时会结束。界面会显示实际使用的 backend，避免把降级 session 误认为完全持久化。

## 终端协议边界

wmux 支持普通 UTF-8/ANSI 终端流、OSC 8 链接以及分片传输。历史输出重放与实时输出之间有显式边界；客户端在重放完成前不会把终端查询回复或用户输入写回当前 PTY。

OSC 52 剪贴板写入、Kitty/iTerm2 文件传输、桌面通知、SIXEL/inline image、Kitty graphics/keyboard 和任意 tmux passthrough 当前有意保持关闭。这些协议能从远端程序触发浏览器或系统副作用，必须先定义用户授权、活动标签页、多客户端选择、限流及文件大小/名称校验，不能仅靠打开 passthrough 安全实现。

内嵌 Symbols Nerd Font Mono 的来源、版本、哈希和许可见 [`client/THIRD_PARTY_NOTICES.md`](client/THIRD_PARTY_NOTICES.md)，运行中的 wmux 也可从“设置 → 关于”打开同一许可说明。

仓库中的 [`deploy/wmux.service.example`](deploy/wmux.service.example) 是 systemd 用户服务示例。它使用 `KillMode=process`，让 systemd 重启 wmux 时不清理同一 cgroup 中已经分离的 tmux/screen 进程。安装后执行 `systemctl --user enable --now wmux`；若希望退出登录后服务仍运行，再执行 `loginctl enable-linger "$USER"`。

远程 SSH session 在目标主机上运行独立的 `tmux`/`screen` session；SSH 网络中断后 wmux 会重新连接并 attach。因此持久化不依赖一条永久存活的 TCP 连接。

重连、wmux 服务重启后的恢复都只会 attach 已有的 tmux/screen session，不会再次执行启动命令；后台 session 已不存在时会话显示为“已退出”，只有显式“重启”才会开始新的一次执行（会话的 `generation` 加一）。删除会话时 wmux 会先通过独立的控制连接结束后台 session；主机不可达时仍会删除本地记录和历史，并提示远端 session 可能仍在运行。登出、修改密码或登录过期后，已打开的终端连接会在数秒内被服务端关闭。

每个 session 内都会设置 `WMUX_SESSION_ID`（当前会话 ID）和 `COLORTERM=truecolor`；隔离的 tmux server 同时开启 24 位色。远端主机上执行的所有脚本都通过 `/bin/sh -c` 运行，登录 shell 为 fish、csh 等非 POSIX shell 的主机同样可用。

终端历史（`WMUX_DATA_DIR/recordings`）以明文 JSONL 保存，只受数据目录的 `0700` 权限保护；其中会包含终端里显示过的一切内容。请把数据目录当作敏感数据备份与清理。

## 开发

```bash
pnpm dev       # Go API :8787 + Vite :5173
pnpm typecheck
pnpm lint
pnpm test      # 前端单测 + Go 单测
pnpm build
pnpm test:browser # 自建临时实例，走真实浏览器流程并自动清理测试数据
WMUX_TMUX_INTEGRATION=1 go test ./internal/terminal -run TestTmuxSessionSurvivesManagerRestore
WMUX_SCREEN_INTEGRATION=1 go test ./internal/terminal -run TestScreenSessionSurvivesManagerClose
```

浏览器验收还需要 `python3` 和 `tmux`；它会覆盖冷缓存字体加载、超过 128 KiB 的 Unicode bracketed paste、历史查询回放隔离、移动布局及会话终止。

详细的数据流、实时协议和安全模型见 [`docs/architecture.md`](docs/architecture.md)。
