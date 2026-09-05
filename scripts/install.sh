#!/usr/bin/env bash
# wmux install / upgrade / uninstall script.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/waterlens/wmux/main/scripts/install.sh | sudo bash
#   bash install.sh [action] [options]
#
# Actions:
#   install    (default) Install or reinstall wmux; on Linux also installs a systemd service
#   upgrade    Upgrade to the latest (or specified) version and restart the service
#   uninstall  Remove wmux, its service and optionally its data
#   check      Show installed vs latest version
#   show-unit  Print the systemd unit and environment file this script installs
#   help       Print this help message
#
# Options:
#   -v, --version VERSION   Install a specific version (e.g. v0.2.0)
#   -f, --file PATH         Install from a local release archive (.tar.gz) or binary
#   -r, --repo OWNER/REPO   GitHub repository (default: waterlens/wmux)
#   -u, --user NAME         Existing account the service runs as (default: the account running
#                           the installer, i.e. \$SUDO_USER when invoked through sudo)
#   --prefix DIR            Directory for the binary (default: /usr/local/bin)
#   --no-systemd            Binary only, no service (always the case on macOS)
#   -y, --yes               Skip confirmation prompts
#   -h, --help              Print help
#
# Environment:
#   WMUX_RELEASE_URL        Base URL holding the archives and SHA256SUMS (default: GitHub releases)

set -euo pipefail

# -- Defaults ------------------------------------------------------------------

REPO="waterlens/wmux"
INSTALL_DIR="${WMUX_INSTALL_DIR:-/usr/local/bin}"
CONFIG_DIR="/etc/wmux"
ENV_FILE="${CONFIG_DIR}/wmux.env"
STATE_DIR="/var/lib/wmux"
DATA_DIR="${STATE_DIR}/data"
SERVICE_NAME="wmux"
SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"
SERVICE_USER=""            # resolved by resolve_service_user (default: the installing account)
SERVICE_USER_EXPLICIT=false

ACTION="install"
VERSION=""
LOCAL_FILE=""
YES=false
NO_SYSTEMD=false
PLATFORM=""
ARCH=""

# -- Color helpers -------------------------------------------------------------

if [[ -t 1 ]]; then
    RED='\033[0;31m'
    GREEN='\033[0;32m'
    YELLOW='\033[0;33m'
    CYAN='\033[0;36m'
    NC='\033[0m'
else
    RED='' GREEN='' YELLOW='' CYAN='' NC=''
fi

info()  { echo -e "${GREEN}[INFO]${NC} $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC} $*"; }
error() { echo -e "${RED}[ERROR]${NC} $*" >&2; }
die()   { error "$@"; exit 1; }

# -- Argument parsing ----------------------------------------------------------

parse_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            install|upgrade|uninstall|check|show-unit|help)
                ACTION="$1"; shift ;;
            -v|--version)
                [[ $# -ge 2 ]] || die "$1 needs a value"
                VERSION="$2"; shift 2 ;;
            -f|--file)
                [[ $# -ge 2 ]] || die "$1 needs a value"
                LOCAL_FILE="$2"; shift 2 ;;
            -r|--repo)
                [[ $# -ge 2 ]] || die "$1 needs a value"
                REPO="$2"; shift 2 ;;
            -u|--user)
                [[ $# -ge 2 ]] || die "$1 needs a value"
                SERVICE_USER="$2"; SERVICE_USER_EXPLICIT=true; shift 2 ;;
            --prefix)
                [[ $# -ge 2 ]] || die "$1 needs a value"
                INSTALL_DIR="$2"; shift 2 ;;
            -y|--yes)
                YES=true; shift ;;
            --no-systemd)
                NO_SYSTEMD=true; shift ;;
            -h|--help)
                ACTION="help"; shift ;;
            *)
                die "Unknown argument: $1. Run with 'help' for usage." ;;
        esac
    done
    BINARY="${INSTALL_DIR}/wmux"
}

# -- Help ----------------------------------------------------------------------

show_help() {
    cat <<'EOF'
wmux installer

Usage:
  install.sh [action] [options]

Actions:
  install    (default) Install or reinstall wmux; on Linux also installs a systemd service
  upgrade    Upgrade to the latest (or specified) version and restart the service
  uninstall  Remove wmux, its service and optionally its data
  check      Show installed vs latest version
  show-unit  Print the systemd unit and environment file this script installs
  help       Print this help message

Options:
  -v, --version VERSION   Install a specific version (e.g. v0.2.0)
  -f, --file PATH         Install from a local release archive (.tar.gz) or binary
  -r, --repo OWNER/REPO   GitHub repository (default: waterlens/wmux)
  -u, --user NAME         Existing account the service runs as (default: the account
                          running the installer, i.e. $SUDO_USER when invoked via sudo)
  --prefix DIR            Directory for the binary (default: /usr/local/bin)
  --no-systemd            Binary only, no service (always the case on macOS)
  -y, --yes               Skip confirmation prompts
  -h, --help              Print help

Installed layout (Linux with systemd):
  Binary:   /usr/local/bin/wmux
  Config:   /etc/wmux/wmux.env
  Data:     /var/lib/wmux/data      (SQLite, master key, terminal history)
  Service:  /etc/systemd/system/wmux.service

The web terminal opens shells as the service user, which is the account that ran
the installer (the account before sudo). The browser therefore gets your own
shell, dotfiles, tmux sessions and SSH agent. Pass --user to run wmux as another
existing account instead; no account is ever created.

Examples:
  # Install the latest release with a systemd service
  curl -fsSL https://raw.githubusercontent.com/waterlens/wmux/main/scripts/install.sh | sudo bash

  # Run the service as another existing account
  sudo bash install.sh install --user alice

  # Install a specific version, or from a downloaded archive
  sudo bash install.sh install -v v0.2.0
  sudo bash install.sh install -f ./wmux_v0.2.0_linux_amd64.tar.gz

  # Binary only, into ~/.local/bin, no root needed
  bash install.sh install --no-systemd --prefix ~/.local/bin

  # Upgrade, check, uninstall
  sudo bash install.sh upgrade
  bash install.sh check
  sudo bash install.sh uninstall
EOF
}

# -- Platform detection --------------------------------------------------------

detect_platform() {
    local os arch
    os="$(uname -s)"
    case "$os" in
        Linux)  os="linux" ;;
        Darwin) os="darwin" ;;
        *)      die "Unsupported OS: $os. Releases cover Linux and macOS." ;;
    esac

    arch="$(uname -m)"
    case "$arch" in
        x86_64|amd64)  arch="amd64" ;;
        aarch64|arm64) arch="arm64" ;;
        *)             die "Unsupported architecture: $arch. Releases cover amd64 and arm64." ;;
    esac

    PLATFORM="$os"
    ARCH="$arch"
    if [[ "$PLATFORM" != "linux" && "$NO_SYSTEMD" == false ]]; then
        NO_SYSTEMD=true
    fi
}

artifact_name() {
    echo "wmux_${1}_${PLATFORM}_${ARCH}.tar.gz"
}

# -- Version helpers -----------------------------------------------------------

installed_version() {
    if [[ -x "$BINARY" ]]; then
        "$BINARY" --version 2>/dev/null | awk '{print $2}' || true
    fi
}

latest_version() {
    local url="https://api.github.com/repos/${REPO}/releases/latest"
    fetch_stdout "$url" 2>/dev/null \
        | grep '"tag_name"' | head -1 \
        | sed 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/' || true
}

# Strip a leading 'v' for comparisons.
normalize_version() {
    echo "${1#v}"
}

resolve_version() {
    if [[ -z "$VERSION" ]]; then
        VERSION="$(latest_version)"
        [[ -n "$VERSION" ]] || die "Could not determine the latest version of ${REPO}. Specify one with -v."
    fi
    [[ "$VERSION" == v* ]] || VERSION="v${VERSION}"
}

# -- Download ------------------------------------------------------------------

fetch() {
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL --retry 3 -o "$2" "$1"
    elif command -v wget >/dev/null 2>&1; then
        wget -qO "$2" "$1"
    else
        die "curl or wget is required."
    fi
}

fetch_stdout() {
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$1"
    elif command -v wget >/dev/null 2>&1; then
        wget -qO- "$1"
    else
        die "curl or wget is required."
    fi
}

sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | cut -d ' ' -f 1
    else
        shasum -a 256 "$1" | cut -d ' ' -f 1
    fi
}

download_artifact() {
    local version="$1" name base tmpdir expected actual
    name="$(artifact_name "$version")"
    base="${WMUX_RELEASE_URL:-https://github.com/${REPO}/releases/download/${version}}"
    tmpdir="$(mktemp -d)"

    info "Downloading ${name} (${version})..."
    if ! fetch "${base}/${name}" "${tmpdir}/${name}"; then
        rm -rf "$tmpdir"
        die "Download failed. Check that version '${version}' exists and has artifact '${name}'."
    fi
    if ! fetch "${base}/SHA256SUMS" "${tmpdir}/SHA256SUMS"; then
        rm -rf "$tmpdir"
        die "Release ${version} has no SHA256SUMS; refusing to install an unverified archive."
    fi
    expected="$(grep " ${name}\$" "${tmpdir}/SHA256SUMS" | cut -d ' ' -f 1)"
    actual="$(sha256_of "${tmpdir}/${name}")"
    if [[ -z "$expected" || "$expected" != "$actual" ]]; then
        rm -rf "$tmpdir"
        die "Checksum mismatch for ${name}."
    fi
    info "Checksum verified."

    DOWNLOAD_DIR="$tmpdir"
    DOWNLOADED_ARCHIVE="${tmpdir}/${name}"
}

# -- Extract / prepare binary --------------------------------------------------

prepare_binary() {
    local source="$1" tmpdir found
    tmpdir="$(mktemp -d)"

    if [[ "$source" == *.tar.gz || "$source" == *.tgz ]]; then
        [[ -f "$source" ]] || die "File not found: $source"
        tar xzf "$source" -C "$tmpdir"
        # Release archives hold wmux_<version>_<os>_<arch>/wmux; accept a bare binary too.
        found="$(find "$tmpdir" -maxdepth 2 -type f -name wmux | head -1)"
        if [[ -z "$found" ]]; then
            rm -rf "$tmpdir"
            die "Archive does not contain a 'wmux' binary."
        fi
        PREPARED_BINARY="$found"
    elif [[ -f "$source" ]]; then
        cp "$source" "${tmpdir}/wmux"
        PREPARED_BINARY="${tmpdir}/wmux"
    else
        die "File not found: $source"
    fi
    PREPARED_DIR="$tmpdir"
    chmod +x "$PREPARED_BINARY"
}

cleanup_temp() {
    [[ -n "${DOWNLOAD_DIR:-}" ]] && rm -rf "$DOWNLOAD_DIR"
    [[ -n "${PREPARED_DIR:-}" ]] && rm -rf "$PREPARED_DIR"
    DOWNLOAD_DIR="" PREPARED_DIR=""
}

# -- Install binary ------------------------------------------------------------

install_binary() {
    local src="$1"
    info "Installing binary to ${BINARY}..."
    mkdir -p "$INSTALL_DIR"
    install -m 0755 "$src" "$BINARY"
}

# -- systemd setup -------------------------------------------------------------

service_group() {
    id -gn "$SERVICE_USER" 2>/dev/null || echo "$SERVICE_USER"
}

service_home() {
    local home
    home="$(getent passwd "$SERVICE_USER" 2>/dev/null | cut -d: -f6)"
    echo "${home:-/home/${SERVICE_USER}}"
}

# The service runs as the account that invoked the installer (the account before
# sudo) unless --user names another existing account. No account is created.
resolve_service_user() {
    if [[ -z "$SERVICE_USER" ]]; then
        SERVICE_USER="${SUDO_USER:-$(id -un)}"
    fi
}

require_service_user() {
    resolve_service_user
    id "$SERVICE_USER" >/dev/null 2>&1 \
        || die "User '${SERVICE_USER}' does not exist. Pass --user with an existing account."
    if [[ "$SERVICE_USER" == "root" && "$SERVICE_USER_EXPLICIT" == false ]]; then
        die "Refusing to run the web terminal as root by default. Run the installer through sudo from your own account, or pass --user root explicitly."
    fi
}

render_env() {
    cat <<ENVEOF
# wmux service configuration. Edit this file, then run: systemctl restart wmux
# Every variable is documented in the README's 配置 table. systemd does not
# expand ~ or \$HOME here, so paths must be absolute.

WMUX_HOST=127.0.0.1
WMUX_PORT=8787
WMUX_DATA_DIR=${DATA_DIR}

# Set the origin browsers use when wmux sits behind a domain or reverse proxy;
# without it, writes are only accepted through a literal IP or localhost.
# WMUX_PUBLIC_URL=https://terminal.example.com
# WMUX_TRUST_PROXY=true
# WMUX_COOKIE_SECURE=true

# WMUX_SESSION_TTL=168h
# WMUX_LOG_LEVEL=info
# OpenSSH config used for read-only host discovery (default: ~/.ssh/config of the service user).
# WMUX_SSH_CONFIG=$(service_home)/.ssh/config
ENVEOF
}

render_unit() {
    cat <<UNITEOF
[Unit]
Description=wmux personal web terminal
Documentation=https://github.com/${REPO}
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${SERVICE_USER}
Group=$(service_group)
EnvironmentFile=${ENV_FILE}
ExecStart=${BINARY}
WorkingDirectory=~
Restart=on-failure
RestartSec=3
TimeoutStopSec=15s
# Keep detached tmux/screen servers alive across a wmux service restart.
KillMode=process
LimitNOFILE=65536
# wmux spawns interactive shells as its user, so the usual unit sandboxing
# (ProtectSystem, ProtectHome, NoNewPrivileges) would cripple those shells.

[Install]
WantedBy=multi-user.target
UNITEOF
}

install_systemd() {
    if [[ "$NO_SYSTEMD" == true ]]; then
        return
    fi
    if ! command -v systemctl >/dev/null 2>&1; then
        warn "systemctl not found -- skipping systemd setup."
        return
    fi

    require_service_user

    mkdir -p "$CONFIG_DIR" "$STATE_DIR" "$DATA_DIR"
    chown "${SERVICE_USER}:$(service_group)" "$STATE_DIR"
    # Existing data may belong to the account a previous install ran as (for
    # example the former wmux system user); hand it to the current service user
    # so it can open its database, master key and recordings.
    if [[ -n "$(find "$DATA_DIR" ! -user "$SERVICE_USER" -print -quit 2>/dev/null)" ]]; then
        info "Transferring ownership of ${DATA_DIR} to ${SERVICE_USER}..."
        chown -R "${SERVICE_USER}:$(service_group)" "$DATA_DIR"
    else
        chown "${SERVICE_USER}:$(service_group)" "$DATA_DIR"
    fi
    chmod 0750 "$STATE_DIR"
    chmod 0700 "$DATA_DIR"

    # Environment file -- only create if absent (preserve user edits).
    if [[ ! -f "$ENV_FILE" ]]; then
        info "Installing default config to ${ENV_FILE}..."
        render_env > "$ENV_FILE"
        chown root:root "$ENV_FILE"
        chmod 0644 "$ENV_FILE"
    else
        info "Config ${ENV_FILE} already exists -- not overwriting."
    fi

    # Service unit -- always rewritten so the user, prefix and unit fixes ship with the binary.
    info "Installing systemd service (${SERVICE_FILE}, User=${SERVICE_USER})..."
    render_unit > "$SERVICE_FILE"
    systemctl daemon-reload
    systemctl enable "$SERVICE_NAME" >/dev/null 2>&1 || true
}

start_service() {
    if [[ "$NO_SYSTEMD" == true ]] || ! command -v systemctl >/dev/null 2>&1; then
        return
    fi
    if systemctl is-active --quiet "$SERVICE_NAME" 2>/dev/null; then
        info "Restarting service..."
        systemctl restart "$SERVICE_NAME"
    else
        info "Starting service..."
        systemctl start "$SERVICE_NAME"
    fi
}

check_multiplexer() {
    if ! command -v tmux >/dev/null 2>&1 && ! command -v screen >/dev/null 2>&1; then
        warn "Neither tmux nor screen is installed; local sessions will not survive a wmux restart."
        warn "Install one, e.g.: apt install tmux  |  dnf install tmux  |  brew install tmux"
    fi
}

# -- Confirm prompt ------------------------------------------------------------

confirm() {
    if [[ "$YES" == true ]]; then
        return 0
    fi
    if [[ ! -t 0 ]]; then
        warn "No terminal to ask '${1:-Continue?}'; pass -y to assume yes."
        return 1
    fi
    local msg="${1:-Continue?}"
    read -rp "$(echo -e "${CYAN}${msg} [y/N]${NC} ")" answer
    [[ "$answer" =~ ^[Yy]$ ]]
}

require_root() {
    if [[ "$(id -u)" -ne 0 ]]; then
        die "This action must run as root (or via sudo)."
    fi
}

# Root is needed for systemd and usually for the prefix; a writable prefix
# without systemd works as a plain user.
require_privileges() {
    if [[ "$NO_SYSTEMD" == true ]]; then
        local probe="$INSTALL_DIR"
        while [[ ! -d "$probe" && "$probe" != "/" ]]; do probe="$(dirname "$probe")"; done
        [[ -w "$probe" ]] && return 0
        die "Cannot write to ${INSTALL_DIR}. Run as root or choose --prefix."
    fi
    require_root
}

print_summary() {
    local ver
    ver="$(installed_version)"
    echo ""
    info "wmux ${ver:-?} installed to ${BINARY}"
    echo ""
    if [[ "$NO_SYSTEMD" == false ]] && command -v systemctl >/dev/null 2>&1; then
        echo "  Open:       http://127.0.0.1:8787  (the first visit creates the admin account)"
        echo "  Configure:  ${ENV_FILE}  (WMUX_PUBLIC_URL when behind a domain or proxy)"
        echo "  Status:     systemctl status ${SERVICE_NAME}"
        echo "  Logs:       journalctl -u ${SERVICE_NAME} -f"
        echo "  Runs as:    ${SERVICE_USER}  (another existing account: install.sh install --user NAME)"
    else
        echo "  Run:        WMUX_DATA_DIR=\$HOME/.local/share/wmux ${BINARY}"
        echo "  Open:       http://127.0.0.1:8787  (the first visit creates the admin account)"
        if [[ "$PLATFORM" == "linux" ]]; then
            echo "  Service:    rerun without --no-systemd, or see deploy/wmux.service.example for a user unit"
        fi
    fi
    case ":$PATH:" in
        *":${INSTALL_DIR}:"*) ;;
        *) echo "  Note:       ${INSTALL_DIR} is not on your PATH" ;;
    esac
    echo ""
}

# -- Actions -------------------------------------------------------------------

acquire_binary() {
    if [[ -n "$LOCAL_FILE" ]]; then
        info "Using local file: ${LOCAL_FILE}"
        prepare_binary "$LOCAL_FILE"
    else
        resolve_version
        download_artifact "$VERSION"
        prepare_binary "$DOWNLOADED_ARCHIVE"
    fi
}

do_install() {
    detect_platform
    require_privileges
    info "Platform: ${PLATFORM}/${ARCH}"

    local current
    current="$(installed_version)"
    [[ -n "$current" ]] && warn "wmux ${current} is already installed at ${BINARY}; reinstalling."

    acquire_binary
    install_binary "$PREPARED_BINARY"
    cleanup_temp

    install_systemd
    start_service
    check_multiplexer
    print_summary
}

do_upgrade() {
    detect_platform
    require_privileges

    local current
    current="$(installed_version)"
    [[ -n "$current" ]] || die "wmux is not installed at ${BINARY}. Run 'install' first."

    if [[ -z "$LOCAL_FILE" ]]; then
        resolve_version
        if [[ "$(normalize_version "$VERSION")" == "$(normalize_version "$current")" ]]; then
            info "Already at version ${current}. Nothing to do."
            return
        fi
        info "Upgrading: ${current} -> ${VERSION}"
    fi

    acquire_binary
    install_binary "$PREPARED_BINARY"
    cleanup_temp

    # The unit may have changed between versions; the env file is preserved.
    install_systemd
    if [[ "$NO_SYSTEMD" == false ]] && command -v systemctl >/dev/null 2>&1 \
        && systemctl is-active --quiet "$SERVICE_NAME" 2>/dev/null; then
        info "Restarting service..."
        systemctl restart "$SERVICE_NAME"
    fi
    info "Upgrade complete! Version: $(installed_version)"
}

do_uninstall() {
    detect_platform
    require_privileges

    info "This removes the wmux binary and service. Data is asked about separately."
    if ! confirm "Uninstall wmux?"; then
        die "Aborted."
    fi

    if command -v systemctl >/dev/null 2>&1; then
        if systemctl is-active --quiet "$SERVICE_NAME" 2>/dev/null; then
            info "Stopping service..."
            systemctl stop "$SERVICE_NAME"
        fi
        systemctl disable "$SERVICE_NAME" >/dev/null 2>&1 || true
        if [[ -f "$SERVICE_FILE" ]]; then
            info "Removing service file..."
            rm -f "$SERVICE_FILE"
            systemctl daemon-reload
        fi
    fi

    if [[ -f "$BINARY" ]]; then
        info "Removing binary ${BINARY}..."
        rm -f "$BINARY"
    fi

    if [[ -d "$CONFIG_DIR" || -d "$STATE_DIR" ]]; then
        warn "${DATA_DIR} holds the SQLite database, the master key that encrypts SSH credentials, and all terminal history."
        if confirm "Remove config (${CONFIG_DIR}) and data (${STATE_DIR})?"; then
            rm -rf "$CONFIG_DIR" "$STATE_DIR"
            info "Config and data removed."
        else
            info "Config and data preserved."
        fi
    fi

    info "wmux uninstalled."
}

do_check() {
    local current latest
    current="$(installed_version)"
    latest="$(latest_version)"

    echo "Installed: ${current:-(not installed at ${BINARY})}"
    echo "Latest:    ${latest:-(could not determine)}"

    if [[ -n "$current" && -n "$latest" ]]; then
        if [[ "$(normalize_version "$current")" == "$(normalize_version "$latest")" ]]; then
            info "Up to date."
        else
            warn "Update available. Run: install.sh upgrade"
        fi
    fi
}

do_show_unit() {
    resolve_service_user
    echo "# ${ENV_FILE}"
    render_env
    echo ""
    echo "# ${SERVICE_FILE}"
    render_unit
}

# -- Main ----------------------------------------------------------------------

main() {
    parse_args "$@"

    case "$ACTION" in
        help)      show_help ;;
        check)     do_check ;;
        show-unit) do_show_unit ;;
        install)   do_install ;;
        upgrade)   do_upgrade ;;
        uninstall) do_uninstall ;;
        *)         die "Unknown action: ${ACTION}" ;;
    esac
}

main "$@"
