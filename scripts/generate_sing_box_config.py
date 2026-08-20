#!/usr/bin/env python3
"""Generate the sing-box configuration and target-local proxy credentials."""

import grp
import ipaddress
import json
import os
from pathlib import Path
import secrets
import tempfile


BASE_DIR = Path("/etc/ipv6-proxy")
SETTINGS_FILE = BASE_DIR / "settings.env"
CREDENTIALS_FILE = BASE_DIR / "credentials"
SING_BOX_CONFIG = Path("/etc/sing-box/config.json")


def atomic_write(path: Path, content: str, mode: int, uid: int = 0, gid: int = 0) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile(
        mode="w",
        encoding="utf-8",
        newline="\n",
        dir=path.parent,
        prefix=f".{path.name}.",
        delete=False,
    ) as output:
        output.write(content)
        temporary_path = Path(output.name)
    os.chmod(temporary_path, mode)
    os.chown(temporary_path, uid, gid)
    os.replace(temporary_path, path)


def load_settings() -> dict[str, str]:
    if not SETTINGS_FILE.exists():
        raise SystemExit(f"missing settings file: {SETTINGS_FILE}")

    settings: dict[str, str] = {}
    for line in SETTINGS_FILE.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        key, separator, value = line.partition("=")
        if not separator:
            raise SystemExit(f"invalid settings line: {key}")
        settings[key.strip()] = value.strip()

    required = {
        "PROXY_LISTEN",
        "PROXY_PORT",
        "PROXY_USER",
        "DIALER_PORT",
        "SERVICE_GROUP",
    }
    missing = sorted(required.difference(settings))
    if missing:
        raise SystemExit(f"missing settings: {', '.join(missing)}")

    try:
        ipaddress.ip_address(settings["PROXY_LISTEN"])
        for key in ("PROXY_PORT", "DIALER_PORT"):
            port = int(settings[key])
            if not 1 <= port <= 65535:
                raise ValueError(f"{key} is outside 1..65535")
    except ValueError as error:
        raise SystemExit(f"invalid settings: {error}") from error

    return settings


def load_or_create_credentials(username: str) -> tuple[str, str]:
    password = ""
    if CREDENTIALS_FILE.exists():
        values: dict[str, str] = {}
        for line in CREDENTIALS_FILE.read_text(encoding="utf-8").splitlines():
            key, separator, value = line.partition("=")
            if separator:
                values[key] = value
        password = values.get("password", "")

    if not password:
        password = secrets.token_urlsafe(24)

    atomic_write(
        CREDENTIALS_FILE,
        f"username={username}\npassword={password}\n",
        0o600,
    )
    return username, password


def generate_sing_box_config(
    listen_address: str,
    listen_port: int,
    dialer_port: int,
    username: str,
    password: str,
) -> str:
    configuration = {
        "log": {
            "level": "info",
            "timestamp": True,
        },
        "inbounds": [
            {
                "type": "mixed",
                "tag": "mixed-in",
                "listen": listen_address,
                "listen_port": listen_port,
                "users": [
                    {
                        "username": username,
                        "password": password,
                    }
                ],
            }
        ],
        "outbounds": [
            {
                "type": "socks",
                "tag": "random-ipv6",
                "server": "127.0.0.1",
                "server_port": dialer_port,
                "version": "5",
            }
        ],
        "route": {
            "rules": [
                {
                    "ip_is_private": True,
                    "action": "reject",
                }
            ],
            "final": "random-ipv6",
        },
    }
    return json.dumps(configuration, indent=2, ensure_ascii=False) + "\n"


def main() -> None:
    if os.geteuid() != 0:
        raise SystemExit("this generator must run as root")

    settings = load_settings()
    BASE_DIR.mkdir(parents=True, exist_ok=True)
    os.chmod(BASE_DIR, 0o700)

    username, password = load_or_create_credentials(settings["PROXY_USER"])
    service_gid = grp.getgrnam(settings["SERVICE_GROUP"]).gr_gid

    configuration = generate_sing_box_config(
        settings["PROXY_LISTEN"],
        int(settings["PROXY_PORT"]),
        int(settings["DIALER_PORT"]),
        username,
        password,
    )
    atomic_write(SING_BOX_CONFIG, configuration, 0o640, uid=0, gid=service_gid)
    print(
        "generated sing-box config "
        f"listen={settings['PROXY_LISTEN']}:{settings['PROXY_PORT']}"
    )


if __name__ == "__main__":
    main()
