# wmux

wmux 是一个自托管的 Web 终端：在浏览器里使用本机 shell 和多台 SSH 主机，终端 session 在关闭页面、切换设备或断网后继续运行。单个 Go 二进制，内嵌完整 Web UI。

## 功能

- 本机 PTY 与多台 SSH 主机统一管理，支持密码、私钥和 `SSH_AUTH_SOCK` 认证
- 基于 `tmux` / `screen` 的持久化 session，可跨浏览器断线和 wmux 重启重新 attach
- 同一 session 可在多个设备打开并显式接管输入权，断线后补发缺失的输出
- SSH host key 指纹探测与人工信任，可从 OpenSSH config 导入主机
- 密码登录与会话过期；连续输错三次密码后锁定登录一小时（重启服务解除），凭据用主密钥加密后存入 SQLite
- 桌面、平板和手机响应式界面，可安装为 PWA
- 八种等宽字体可选，终端宽度可固定列数（窄屏幕自动缩小字号）
- 手机上触摸滑动即可滚动 tmux、vim 等程序的历史
- tmux 鼠标模式、OSC 8 链接、Nerd Font 图标、Unicode 11 字符宽度与 24 位色

## 安装

### 安装脚本（Linux / macOS）

```bash
curl -fsSL https://raw.githubusercontent.com/waterlens/wmux/main/scripts/install.sh | sudo bash
```

脚本把最新 Release 的二进制安装到 `/usr/local/bin/wmux`。在 Linux 上它还会安装、启动 systemd 服务 `wmux`：配置文件为 `/etc/wmux/wmux.env`，数据目录为 `/var/lib/wmux/data`。安装完成后打开 <http://127.0.0.1:8787>，首次访问时创建管理员账户。

服务以运行安装脚本的账号（`sudo` 之前的那个账号）身份运行，不会创建单独的系统用户：浏览器里的“本机终端”得到的就是你自己的 shell、dotfiles、tmux 会话和 `SSH_AUTH_SOCK`。要改用另一个已存在的账号：

```bash
curl -fsSLO https://raw.githubusercontent.com/waterlens/wmux/main/scripts/install.sh
sudo bash install.sh install --user alice
```

其它常用命令：

```bash
sudo bash install.sh upgrade                                # 升级到最新版本
sudo bash install.sh install -v v0.2.0                      # 安装指定版本
bash install.sh check                                       # 查看已安装版本与最新版本
sudo bash install.sh uninstall                              # 卸载，数据与用户会分别询问
bash install.sh install --no-systemd --prefix ~/.local/bin  # 只安装二进制，不需要 root
bash install.sh help
```

macOS 上脚本只安装二进制。

### 从源码构建

需要 Go 1.26+、Node.js 24+、pnpm，以及 `tmux` 或 `screen`。Node.js 只在构建时需要。

```bash
pnpm install
pnpm build
WMUX_DATA_DIR="$PWD/.wmux" ./bin/wmux
```

默认监听 `127.0.0.1:8787`。

### Docker

```bash
docker compose up -d --build
```

镜像内置 `tmux`，数据保存在 `wmux-data` volume。容器中的“本机”是容器本身：重启容器会保留会话元数据与历史，但会结束容器内运行的进程。要操作宿主机，请在宿主机原生运行 wmux，或把宿主机作为 SSH 主机添加。

### systemd 用户服务

不使用安装脚本时，可以把 [`deploy/wmux.service.example`](deploy/wmux.service.example) 复制为 `~/.config/systemd/user/wmux.service`，需要的环境变量写入 `~/.config/wmux/wmux.env`（模板见 [`deploy/wmux.env.example`](deploy/wmux.env.example)），然后：

```bash
systemctl --user enable --now wmux
loginctl enable-linger "$USER"   # 退出登录后也保持运行
```

## 配置

wmux 通过环境变量配置。安装脚本部署的服务把它们写在 `/etc/wmux/wmux.env`，修改后执行 `systemctl restart wmux`。

| 环境变量             | 默认值                | 说明                                              |
| -------------------- | --------------------- | ------------------------------------------------- |
| `WMUX_HOST`          | `127.0.0.1`           | 监听地址                                          |
| `WMUX_PORT`          | `8787`                | 监听端口                                          |
| `WMUX_DATA_DIR`      | `.wmux`               | SQLite、主密钥和历史目录                          |
| `WMUX_PUBLIC_URL`    | 空                    | 浏览器访问的规范 origin，用于 CSRF/WebSocket 校验 |
| `WMUX_COOKIE_SECURE` | 随 Public URL         | 是否只通过 HTTPS 发送登录 Cookie                  |
| `WMUX_TRUST_PROXY`   | `false`               | 是否信任反向代理提供的协议及客户端地址            |
| `WMUX_SESSION_TTL`   | `168h`                | 登录有效期                                        |
| `WMUX_LOG_LEVEL`     | `info`                | 日志级别：`debug`、`info`、`warn` 或 `error`      |
| `WMUX_SSH_CONFIG`    | 空（`~/.ssh/config`） | 用于只读发现候选主机的 OpenSSH config 路径        |

- 备份时同时保存数据库和 `master.key`：SSH 密码、私钥和 passphrase 用主密钥加密，丢失主密钥后无法恢复。
- 终端历史（`recordings/`）以明文保存在数据目录中，包含终端里显示过的一切内容。数据目录权限为 `0700`，请当作敏感数据备份和清理。
- 同一数据目录同时只能由一个 wmux 进程使用。
- 未设置 `WMUX_PUBLIC_URL` 时只能通过 IP 地址或 `localhost` 访问；使用域名（包括内网 DNS 名）时必须设置它。

## 通过域名访问

建议放在 WireGuard、Tailscale 等私有网络内，并用反向代理提供 HTTPS：工具栏的复制/粘贴需要 HTTPS（`localhost` 除外），未启用 TLS 的端口也不应直接暴露到公网。

```env
WMUX_HOST=127.0.0.1
WMUX_PUBLIC_URL=https://terminal.example.com
WMUX_COOKIE_SECURE=true
WMUX_TRUST_PROXY=true
```

Caddy 配置示例见 [`deploy/Caddyfile.example`](deploy/Caddyfile.example)。

## 从 OpenSSH config 导入主机

主机管理页面会列出运行账户的 `~/.ssh/config`（或 `WMUX_SSH_CONFIG` 指定文件）中的主机，供管理员选择导入。导入只复制别名、地址、端口和用户名，认证方式为 SSH agent；不会读取私钥或 `known_hosts`，也不会自动信任 host key，首次连接仍需探测并确认指纹。使用 `ProxyJump` / `ProxyCommand` 的主机暂不支持导入；除 `Match all` 外的 `Match` 块会被忽略。

Docker 部署时只以只读方式挂载 config 及其引用的片段（见 `compose.yaml` 中的注释），不要挂载整个 `~/.ssh`。容器以 UID `10001` 运行，挂载的文件需允许该 UID 读取。

## Session 持久化

`auto` 模式依次尝试 `tmux`、`screen` 和直接 PTY。前两种可在浏览器断开和 wmux 重启后重新 attach，直接 PTY 只能跨浏览器断线。wmux 使用独立的 tmux/screen 配置与 server，不会出现在你自己的 tmux 中，也不继承你的状态栏和快捷键；界面会显示每个 session 实际使用的方式。

重连和 wmux 重启后只会 attach 已有的 session，不会重新执行启动命令；后台 session 已不存在时显示为“已退出”，需要显式“重启”。删除 session 会先结束对应的后台 session；主机不可达时仍会删除本地记录，并提示远端 session 可能仍在运行。登出、修改密码或登录过期后，已打开的终端会在数秒内断开。

每个 session 内会设置 `WMUX_SESSION_ID` 和 `COLORTERM=truecolor`。SSH 主机的登录 shell 可以是 fish、csh 等非 POSIX shell。

## 限制

- OSC 52 剪贴板写入、终端文件传输、桌面通知、SIXEL / Kitty 图形、Kitty 键盘协议和 tmux passthrough 目前关闭。
- 内嵌字体与组件的许可见 [`client/public/third-party-notices.txt`](client/public/third-party-notices.txt)，“设置 → 关于”中也可打开。

## 开发

```bash
pnpm dev          # Go API :8787 + Vite :5173
pnpm test         # format:check、lint、typecheck、vitest、go test -race、go vet
pnpm build        # 构建 Web UI 并编译 bin/wmux
pnpm exec playwright install chromium   # 首次运行浏览器验收前执行一次
pnpm test:browser # 浏览器验收，需要 bin/wmux、python3 和 tmux
WMUX_TMUX_INTEGRATION=1 go test ./internal/terminal -run TestTmuxSessionSurvivesManagerRestore
WMUX_SCREEN_INTEGRATION=1 go test ./internal/terminal -run TestScreenSessionSurvivesManagerClose
```

### 发布

```bash
git tag v0.2.0
pnpm release                      # 构建四个平台的归档和 SHA256SUMS 到 release/
sh scripts/release.sh --publish   # 需要 gh CLI，上传到该 tag 的 GitHub Release
```

架构、实时协议与安全模型见 [`docs/architecture.md`](docs/architecture.md)。
