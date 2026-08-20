# netcup_ipv6_proxy_deploy

这是一个面向 Linux/systemd 服务器的部署包。外部客户端连接 sing-box 的 HTTP/SOCKS 混合代理；每个 IPv6 目标连接由本地 SOCKS5 拨号器从指定 `/64` 前缀中随机选择一个源地址。IPv4 目标仍使用服务器的普通 IPv4 出口。

代理账号密码在目标机首次安装时随机生成，并以 `0600` 权限保存在 `/etc/ipv6-proxy/credentials`。

## 前提

- Debian/Ubuntu 类 Linux，使用 systemd；
- 运营商分配并正确路由到本机的公网 IPv6 `/64`；
- 已安装并存在 systemd 服务的 `sing-box` 和 `ndppd`；
- `Go 1.22+`、`Python 3`、`iproute2`；
- root 权限。

安装脚本会接管 `/etc/ndppd.conf` 和 `/etc/sing-box/config.json`。若原文件存在，会各自保留一份后缀为 `.ipv6-proxy.backup` 的首次备份。

## 部署

先确认公网网卡名：

```bash
ip -br link
```

然后在项目目录运行安装。下面的 `2001:db8:1234:5678::/64` 是文档专用示例，必须替换为目标服务器实际获配的 `/64`：

```bash
sudo bash install.sh \
  --prefix 2001:db8:1234:5678::/64 \
  --interface eth0
```

常用可选参数：

```text
--listen 0.0.0.0       代理监听地址
--port 27323           对外代理端口
--username ipv6proxy   代理用户名
--dialer-port 27324    仅回环监听的内部 SOCKS5 端口
--service-user sing-box
--service-group sing-box
```

如发行版的 sing-box 使用其他系统账号，请通过最后两个参数指定。脚本会先检查参数和依赖、测试并编译 Go 程序，再写入系统配置。

## 安装后

查看目标机生成的账号密码：

```bash
sudo cat /etc/ipv6-proxy/credentials
```

检查服务：

```bash
systemctl --no-pager --full status \
  ndppd ipv6-proxy-network ipv6-random-dialer sing-box
```

查看日志：

```bash
journalctl -u ipv6-random-dialer -u sing-box -n 100 --no-pager
```

请在云防火墙和本机防火墙中仅向可信客户端放行所选 TCP 代理端口。不要把代理端口无条件暴露给全网。

测试时可把服务器地址、用户名和密码填入支持 HTTP 或 SOCKS5 的客户端。连续访问 IPv6 回显服务时，IPv6 目标应看到同一 `/64` 下变化的源地址。

## 重新配置

修改前缀、网卡、端口或用户名时，使用新参数重新运行 `install.sh`。已有密码会保留。若要轮换密码：

```bash
sudo rm /etc/ipv6-proxy/credentials
sudo /usr/local/libexec/generate-ipv6-proxy-config
sudo systemctl restart sing-box
```

## 文件布局

```text
config/                       可部署的 sysctl、ndppd、systemd 模板
scripts/                      sing-box 配置及目标机凭据生成器
src/ipv6-random-dialer/       无第三方依赖的 Go SOCKS5 拨号器源码
install.sh                    参数校验、构建、安装及启动脚本
```

内部拨号器只监听 `127.0.0.1`，并拒绝环回、私网、链路本地、非全局单播地址以及代理自身的 IPv6 前缀，以降低 SSRF 风险。
