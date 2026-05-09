#!/usr/bin/env bash

set -euo pipefail

GITHUB_REPO="${GITHUB_REPO:-}"
INSTALL_VERSION="${INSTALL_VERSION:-latest}"
ASSUME_YES="false"
SKIP_START="false"

XUI_MAIN_FOLDER="${XUI_MAIN_FOLDER:-/usr/local/x-ui}"
XUI_SERVICE="${XUI_SERVICE:-/etc/systemd/system}"
XUI_DB_FOLDER="${XUI_DB_FOLDER:-/etc/x-ui}"
XUI_LOG_FOLDER="${XUI_LOG_FOLDER:-/var/log/x-ui}"
XUI_ENV_FILE="${XUI_ENV_FILE:-/etc/default/x-ui}"
XUI_CLI_BIN="${XUI_CLI_BIN:-/usr/bin/x-ui}"

usage() {
  cat <<'EOF'
Usage:
  ./scripts/install-selfhosted.sh --repo owner/repo [--version v1.2.3] [--yes]

Required:
  --repo            GitHub repository, for example: yourname/3x-ui

Optional:
  --version         Release tag to install. Default: latest
  --yes             Skip confirmation prompt
  --skip-start      Install files but do not start x-ui
  --help            Show this help text

Environment variables:
  GITHUB_REPO       Same as --repo
  INSTALL_VERSION   Same as --version
EOF
}

log() {
  printf '[%s] %s\n' "$(date '+%F %T')" "$*"
}

die() {
  printf 'Error: %s\n' "$*" >&2
  exit 1
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "Missing required command: $1"
}

arch() {
  case "$(uname -m)" in
    x86_64|x64|amd64) echo 'amd64' ;;
    i*86|x86) echo '386' ;;
    armv8*|armv8|arm64|aarch64) echo 'arm64' ;;
    armv7*|armv7|arm) echo 'armv7' ;;
    armv6*|armv6) echo 'armv6' ;;
    armv5*|armv5) echo 'armv5' ;;
    s390x) echo 's390x' ;;
    *) die "Unsupported CPU architecture: $(uname -m)" ;;
  esac
}

detect_release() {
  if [[ -f /etc/os-release ]]; then
    . /etc/os-release
    echo "${ID:-unknown}"
    return 0
  fi
  if [[ -f /usr/lib/os-release ]]; then
    . /usr/lib/os-release
    echo "${ID:-unknown}"
    return 0
  fi
  echo "unknown"
}

install_base() {
  local release="$1"
  case "$release" in
    ubuntu|debian|armbian)
      apt-get update
      apt-get install -y -q curl tar ca-certificates openssl
      ;;
    fedora|amzn|virtuozzo|rhel|almalinux|rocky|ol)
      dnf -y update
      dnf install -y -q curl tar ca-certificates openssl
      ;;
    centos)
      if [[ "${VERSION_ID:-}" =~ ^7 ]]; then
        yum -y update
        yum install -y curl tar ca-certificates openssl
      else
        dnf -y update
        dnf install -y -q curl tar ca-certificates openssl
      fi
      ;;
    arch|manjaro|parch)
      pacman -Syu --noconfirm curl tar ca-certificates openssl
      ;;
    opensuse-tumbleweed|opensuse-leap)
      zypper refresh
      zypper -q install -y curl tar ca-certificates openssl
      ;;
    alpine)
      apk update
      apk add curl tar ca-certificates openssl
      ;;
    *)
      apt-get update
      apt-get install -y -q curl tar ca-certificates openssl
      ;;
  esac
}

resolve_tag() {
  local repo="$1"
  local requested="$2"
  local tag

  if [[ "$requested" != "latest" ]]; then
    printf '%s\n' "$requested"
    return 0
  fi

  tag="$(
    curl -fsSL "https://api.github.com/repos/$repo/releases?per_page=20" \
      | grep -m1 '"tag_name":' \
      | sed -E 's/.*"([^"]+)".*/\1/'
  )"

  [[ -n "$tag" ]] || die "Failed to resolve latest release tag from GitHub for $repo"
  printf '%s\n' "$tag"
}

is_port_in_use() {
  local port="$1"
  if command -v ss >/dev/null 2>&1; then
    ss -ltn 2>/dev/null | awk -v p=":${port}$" '$4 ~ p {exit 0} END {exit 1}'
    return
  fi
  if command -v netstat >/dev/null 2>&1; then
    netstat -lnt 2>/dev/null | awk -v p=":${port} " '$4 ~ p {exit 0} END {exit 1}'
    return
  fi
  if command -v lsof >/dev/null 2>&1; then
    lsof -nP -iTCP:"${port}" -sTCP:LISTEN >/dev/null 2>&1 && return 0
  fi
  return 1
}

random_string() {
  local length="$1"
  openssl rand -base64 $((length * 2)) | tr -dc 'a-zA-Z0-9' | head -c "$length"
}

pick_random_port() {
  local port
  while :; do
    port="$(shuf -i 10240-62000 -n 1)"
    if ! is_port_in_use "$port"; then
      printf '%s\n' "$port"
      return 0
    fi
  done
}

generate_service_file() {
  cat >"$1" <<EOF
[Unit]
Description=x-ui Service
After=network.target
Wants=network.target

[Service]
EnvironmentFile=-$XUI_ENV_FILE
Environment="XRAY_VMESS_AEAD_FORCED=false"
Type=simple
WorkingDirectory=$XUI_MAIN_FOLDER/
ExecStart=$XUI_MAIN_FOLDER/x-ui
ExecReload=kill -USR1 \$MAINPID
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target
EOF
}

generate_cli_script() {
  cat >"$1" <<EOF
#!/usr/bin/env bash
set -euo pipefail

cmd="\${1:-}"
case "\$cmd" in
  start)
    exec systemctl start x-ui
    ;;
  stop)
    exec systemctl stop x-ui
    ;;
  restart)
    exec systemctl restart x-ui
    ;;
  status)
    exec systemctl status x-ui --no-pager
    ;;
  enable)
    exec systemctl enable x-ui
    ;;
  disable)
    exec systemctl disable x-ui
    ;;
  log)
    exec journalctl -u x-ui -e --no-pager
    ;;
  setting|cert|migrate)
    shift
    exec "$XUI_MAIN_FOLDER/x-ui" "\$cmd" "\$@"
    ;;
  settings)
    shift
    exec "$XUI_MAIN_FOLDER/x-ui" setting "\$@"
    ;;
  "")
    cat <<'USAGE'
x-ui helper
Usage:
  x-ui start|stop|restart|status|enable|disable|log
  x-ui setting [flags]
  x-ui cert [flags]
  x-ui migrate
USAGE
    ;;
  *)
    exec "$XUI_MAIN_FOLDER/x-ui" "\$@"
    ;;
esac
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --repo)
      GITHUB_REPO="${2:-}"; shift 2 ;;
    --version)
      INSTALL_VERSION="${2:-}"; shift 2 ;;
    --yes)
      ASSUME_YES="true"; shift ;;
    --skip-start)
      SKIP_START="true"; shift ;;
    --help|-h)
      usage; exit 0 ;;
    *)
      die "Unknown argument: $1" ;;
  esac
done

[[ $EUID -eq 0 ]] || die "Please run this script as root"
[[ -n "$GITHUB_REPO" ]] || die "--repo or GITHUB_REPO is required"

require_cmd curl
require_cmd tar
require_cmd mktemp
require_cmd systemctl

RELEASE="$(detect_release)"
ARCH="$(arch)"
TAG="$(resolve_tag "$GITHUB_REPO" "$INSTALL_VERSION")"
DOWNLOAD_URL="https://github.com/$GITHUB_REPO/releases/download/$TAG/x-ui-linux-$ARCH.tar.gz"

log "Repository: $GITHUB_REPO"
log "Release tag: $TAG"
log "Architecture: $ARCH"
log "Download URL: $DOWNLOAD_URL"

if [[ "$ASSUME_YES" != "true" ]]; then
  printf 'Proceed with installation? [y/N]: '
  read -r answer
  [[ "$answer" =~ ^[Yy]$ ]] || die "Aborted by user"
fi

install_base "$RELEASE"

WORK_DIR="$(mktemp -d)"
ARCHIVE_PATH="$WORK_DIR/x-ui-linux-$ARCH.tar.gz"
SERVICE_FILE="$WORK_DIR/x-ui.service"
CLI_FILE="$WORK_DIR/x-ui.sh"
BACKUP_DIR="/root/xui-install-backup-$(date +%F-%H%M%S)"
DB_EXISTED="false"

cleanup() {
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT

if [[ -f "$XUI_DB_FOLDER/x-ui.db" ]]; then
  DB_EXISTED="true"
fi

log "Downloading release archive"
curl -fL "$DOWNLOAD_URL" -o "$ARCHIVE_PATH"

log "Extracting archive"
mkdir -p "$WORK_DIR/extract"
tar -xzf "$ARCHIVE_PATH" -C "$WORK_DIR/extract"
[[ -d "$WORK_DIR/extract/x-ui" ]] || die "Invalid release archive: x-ui directory missing"
[[ -f "$WORK_DIR/extract/x-ui/x-ui" ]] || die "Invalid release archive: x-ui binary missing"

log "Backing up current installation to $BACKUP_DIR"
mkdir -p "$BACKUP_DIR"
if [[ -d "$XUI_MAIN_FOLDER" ]]; then
  cp -a "$XUI_MAIN_FOLDER" "$BACKUP_DIR/" >/dev/null 2>&1 || true
fi
if [[ -f "$XUI_DB_FOLDER/x-ui.db" ]]; then
  cp -a "$XUI_DB_FOLDER/x-ui.db" "$BACKUP_DIR/" >/dev/null 2>&1 || true
fi
if [[ -f "$XUI_SERVICE/x-ui.service" ]]; then
  cp -a "$XUI_SERVICE/x-ui.service" "$BACKUP_DIR/" >/dev/null 2>&1 || true
fi
if [[ -f "$XUI_ENV_FILE" ]]; then
  cp -a "$XUI_ENV_FILE" "$BACKUP_DIR/" >/dev/null 2>&1 || true
fi

log "Stopping old x-ui service"
systemctl stop x-ui >/dev/null 2>&1 || true

log "Installing files"
rm -rf "$XUI_MAIN_FOLDER"
mkdir -p "$(dirname "$XUI_MAIN_FOLDER")"
cp -a "$WORK_DIR/extract/x-ui" "$XUI_MAIN_FOLDER"
chmod +x "$XUI_MAIN_FOLDER/x-ui"
mkdir -p "$XUI_DB_FOLDER" "$XUI_LOG_FOLDER"

generate_service_file "$SERVICE_FILE"
generate_cli_script "$CLI_FILE"
install -m 0644 "$SERVICE_FILE" "$XUI_SERVICE/x-ui.service"
install -m 0755 "$CLI_FILE" "$XUI_CLI_BIN"
systemctl daemon-reload
systemctl enable x-ui >/dev/null 2>&1 || true

if [[ "$SKIP_START" == "true" ]]; then
  log "Installation finished. x-ui service was not started because --skip-start was used."
  exit 0
fi

log "Starting x-ui"
systemctl start x-ui
sleep 3
systemctl is-active x-ui >/dev/null

if [[ "$DB_EXISTED" == "false" ]]; then
  log "Fresh installation detected, generating temporary secure panel settings"
  RANDOM_PORT="$(pick_random_port)"
  RANDOM_USER="$(random_string 10)"
  RANDOM_PASS="$(random_string 12)"
  RANDOM_PATH="$(random_string 18)"
  "$XUI_MAIN_FOLDER/x-ui" setting \
    -username "$RANDOM_USER" \
    -password "$RANDOM_PASS" \
    -port "$RANDOM_PORT" \
    -webBasePath "$RANDOM_PATH" >/dev/null
  systemctl restart x-ui
  sleep 2
  log "Temporary panel settings:"
  log "  username: $RANDOM_USER"
  log "  password: $RANDOM_PASS"
  log "  port: $RANDOM_PORT"
  log "  webBasePath: $RANDOM_PATH"
  log "These settings will be replaced if you later upload your own x-ui.db."
else
  log "Existing x-ui.db detected, panel settings were preserved."
fi

log "Installation completed successfully"
log "Backup directory: $BACKUP_DIR"
