#!/usr/bin/env bash
set -Eeuo pipefail

usage() {
    cat <<'EOF'
Usage:
  sudo bash install.sh --prefix IPV6_CIDR --interface NAME [options]

Required:
  --prefix CIDR          Routed IPv6 /64, for example 2001:db8:1234:5678::/64
  --interface NAME       Public network interface, for example eth0 or ens3

Options:
  --listen ADDRESS       Public proxy listen address (default: 0.0.0.0)
  --port PORT            Public mixed-proxy port (default: 27323)
  --username USERNAME    Proxy authentication username (default: ipv6proxy)
  --dialer-port PORT     Loopback SOCKS5 port (default: 27324)
  --service-user USER    User running sing-box and the dialer (default: sing-box)
  --service-group GROUP  Group running sing-box and the dialer (default: sing-box)
  -h, --help             Show this help
EOF
}

IPV6_PREFIX=""
NETWORK_INTERFACE=""
PROXY_LISTEN="0.0.0.0"
PROXY_PORT="27323"
PROXY_USER="ipv6proxy"
DIALER_PORT="27324"
SERVICE_USER="sing-box"
SERVICE_GROUP="sing-box"

while (($#)); do
    case "$1" in
        --prefix)
            [[ $# -ge 2 ]] || { echo "missing value for --prefix" >&2; exit 2; }
            IPV6_PREFIX="$2"
            shift 2
            ;;
        --interface)
            [[ $# -ge 2 ]] || { echo "missing value for --interface" >&2; exit 2; }
            NETWORK_INTERFACE="$2"
            shift 2
            ;;
        --listen)
            [[ $# -ge 2 ]] || { echo "missing value for --listen" >&2; exit 2; }
            PROXY_LISTEN="$2"
            shift 2
            ;;
        --port)
            [[ $# -ge 2 ]] || { echo "missing value for --port" >&2; exit 2; }
            PROXY_PORT="$2"
            shift 2
            ;;
        --username)
            [[ $# -ge 2 ]] || { echo "missing value for --username" >&2; exit 2; }
            PROXY_USER="$2"
            shift 2
            ;;
        --dialer-port)
            [[ $# -ge 2 ]] || { echo "missing value for --dialer-port" >&2; exit 2; }
            DIALER_PORT="$2"
            shift 2
            ;;
        --service-user)
            [[ $# -ge 2 ]] || { echo "missing value for --service-user" >&2; exit 2; }
            SERVICE_USER="$2"
            shift 2
            ;;
        --service-group)
            [[ $# -ge 2 ]] || { echo "missing value for --service-group" >&2; exit 2; }
            SERVICE_GROUP="$2"
            shift 2
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            echo "unknown option: $1" >&2
            usage >&2
            exit 2
            ;;
    esac
done

if [[ $EUID -ne 0 ]]; then
    echo "run this installer as root (use sudo)" >&2
    exit 1
fi

if [[ -z $IPV6_PREFIX || -z $NETWORK_INTERFACE ]]; then
    echo "--prefix and --interface are required" >&2
    usage >&2
    exit 2
fi

required_commands=(getent go install ip ndppd python3 sing-box sysctl systemctl)
missing_commands=()
for command_name in "${required_commands[@]}"; do
    if ! command -v "$command_name" >/dev/null 2>&1; then
        missing_commands+=("$command_name")
    fi
done
if ((${#missing_commands[@]})); then
    echo "missing required commands: ${missing_commands[*]}" >&2
    echo "install the prerequisites listed in README.md, then retry" >&2
    exit 1
fi

if ! systemctl cat ndppd.service >/dev/null 2>&1; then
    echo "ndppd.service is not installed" >&2
    exit 1
fi
if ! systemctl cat sing-box.service >/dev/null 2>&1; then
    echo "sing-box.service is not installed" >&2
    exit 1
fi
if ! getent passwd "$SERVICE_USER" >/dev/null; then
    echo "service user does not exist: $SERVICE_USER" >&2
    exit 1
fi
if ! getent group "$SERVICE_GROUP" >/dev/null; then
    echo "service group does not exist: $SERVICE_GROUP" >&2
    exit 1
fi
if ! ip link show dev "$NETWORK_INTERFACE" >/dev/null 2>&1; then
    echo "network interface does not exist: $NETWORK_INTERFACE" >&2
    exit 1
fi
if [[ ! $PROXY_USER =~ ^[A-Za-z0-9._-]{1,64}$ ]]; then
    echo "proxy username may contain only letters, numbers, dot, underscore and hyphen" >&2
    exit 1
fi

python3 - "$IPV6_PREFIX" "$PROXY_LISTEN" "$PROXY_PORT" "$DIALER_PORT" <<'PY'
import ipaddress
import sys

try:
    prefix = ipaddress.ip_network(sys.argv[1], strict=True)
    if prefix.version != 6 or prefix.prefixlen != 64:
        raise ValueError("--prefix must be a canonical IPv6 /64 network")
    ipaddress.ip_address(sys.argv[2])
    ports = [int(sys.argv[3]), int(sys.argv[4])]
    if any(port < 1 or port > 65535 for port in ports):
        raise ValueError("ports must be in 1..65535")
    if ports[0] == ports[1]:
        raise ValueError("public and dialer ports must differ")
except ValueError as error:
    raise SystemExit(f"invalid configuration: {error}") from error
PY

PROJECT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
BUILD_DIR="$(mktemp -d)"
cleanup() {
    rm -rf -- "$BUILD_DIR"
}
trap cleanup EXIT

render_template() {
    local source_path="$1"
    local destination_path="$2"
    shift 2
    python3 - "$source_path" "$destination_path" "$@" <<'PY'
from pathlib import Path
import sys

source = Path(sys.argv[1])
destination = Path(sys.argv[2])
arguments = sys.argv[3:]
if len(arguments) % 2:
    raise SystemExit("template replacements must be key/value pairs")

content = source.read_text(encoding="utf-8")
for index in range(0, len(arguments), 2):
    content = content.replace(arguments[index], arguments[index + 1])
if "@" in content:
    unresolved = [word for word in content.split() if word.startswith("@")]
    if unresolved:
        raise SystemExit(f"unresolved template value(s): {unresolved}")
destination.write_text(content, encoding="utf-8", newline="\n")
PY
}

backup_once() {
    local target_path="$1"
    local backup_path="${target_path}.ipv6-proxy.backup"
    if [[ -e $target_path && ! -e $backup_path ]]; then
        cp -a -- "$target_path" "$backup_path"
        echo "backed up $target_path to $backup_path"
    fi
}

echo "building and testing ipv6-random-dialer"
(
    cd "$PROJECT_DIR/src/ipv6-random-dialer"
    go test ./...
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$BUILD_DIR/ipv6-random-dialer" .
)

render_template \
    "$PROJECT_DIR/config/ndppd/ndppd.conf.template" \
    "$BUILD_DIR/ndppd.conf" \
    "@NETWORK_INTERFACE@" "$NETWORK_INTERFACE" \
    "@IPV6_PREFIX@" "$IPV6_PREFIX"

render_template \
    "$PROJECT_DIR/config/systemd/ipv6-proxy-network.service.template" \
    "$BUILD_DIR/ipv6-proxy-network.service" \
    "@IPV6_PREFIX@" "$IPV6_PREFIX"

render_template \
    "$PROJECT_DIR/config/systemd/ipv6-random-dialer.service.template" \
    "$BUILD_DIR/ipv6-random-dialer.service" \
    "@SERVICE_USER@" "$SERVICE_USER" \
    "@SERVICE_GROUP@" "$SERVICE_GROUP" \
    "@DIALER_PORT@" "$DIALER_PORT" \
    "@IPV6_PREFIX@" "$IPV6_PREFIX"

systemctl stop sing-box.service ipv6-random-dialer.service ipv6-proxy-network.service \
    >/dev/null 2>&1 || true

backup_once /etc/ndppd.conf
backup_once /etc/sing-box/config.json

install -d -m 0700 /etc/ipv6-proxy
install -d -m 0755 /etc/sing-box /etc/systemd/system/sing-box.service.d /usr/local/libexec
install -m 0644 "$PROJECT_DIR/config/sysctl/90-ipv6-proxy.conf" /etc/sysctl.d/90-ipv6-proxy.conf
install -m 0644 "$BUILD_DIR/ndppd.conf" /etc/ndppd.conf
install -m 0644 "$BUILD_DIR/ipv6-proxy-network.service" /etc/systemd/system/ipv6-proxy-network.service
install -m 0644 "$BUILD_DIR/ipv6-random-dialer.service" /etc/systemd/system/ipv6-random-dialer.service
install -m 0644 "$PROJECT_DIR/config/systemd/sing-box-override.conf" \
    /etc/systemd/system/sing-box.service.d/ipv6-proxy.conf
install -m 0755 "$BUILD_DIR/ipv6-random-dialer" /usr/local/libexec/ipv6-random-dialer
install -m 0755 "$PROJECT_DIR/scripts/generate_sing_box_config.py" \
    /usr/local/libexec/generate-ipv6-proxy-config

umask 077
cat > /etc/ipv6-proxy/settings.env <<EOF
PROXY_LISTEN=$PROXY_LISTEN
PROXY_PORT=$PROXY_PORT
PROXY_USER=$PROXY_USER
DIALER_PORT=$DIALER_PORT
SERVICE_GROUP=$SERVICE_GROUP
EOF
chmod 0600 /etc/ipv6-proxy/settings.env

/usr/local/libexec/generate-ipv6-proxy-config
sing-box check -c /etc/sing-box/config.json
sysctl -p /etc/sysctl.d/90-ipv6-proxy.conf
systemctl daemon-reload
systemctl enable ndppd.service ipv6-proxy-network.service ipv6-random-dialer.service sing-box.service
systemctl restart ndppd.service
systemctl restart ipv6-proxy-network.service
systemctl restart ipv6-random-dialer.service
systemctl restart sing-box.service

echo
echo "installation complete"
echo "credentials: sudo cat /etc/ipv6-proxy/credentials"
echo "status:      systemctl --no-pager --full status ipv6-random-dialer sing-box"
echo "remember to restrict TCP port $PROXY_PORT in your firewall"
