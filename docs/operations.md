# 高校基础设施管理员操作手册

本手册的域名、IP 和网段均为文档示例。部署时使用本单位审批后的入口、校园公网出口、IPv6 前缀和 VPN 地址池。

## 1. 确定网络边界

网关的任务是选择浏览器可访问的入口。部署前记录以下信息：

| 项目 | 确认方式 |
|---|---|
| 校内 / 校外目标 | 使用对应网络的浏览器验证 HTTPS、入口目录与登录流程 |
| 校内来源 | 记录网关实际看到的校园出口 IPv4、内部子网和 IPv6 前缀 |
| VPN / 访客来源 | 确认这些地址是否应强制走校外入口，将相应池加入排除文件 |
| 代理链 | 列出 Nginx、负载均衡器、CDN 等实际出口的地址；明确谁覆盖或追加 XFF |
| 监听范围 | 同机反向代理使用 loopback；分机代理使用管理网络地址及防火墙限制 |
| 认证 | 网关不处理登录态；业务系统与 WebVPN 必须保留自己的认证机制 |

NAT 后只能根据网关实际观察到的地址分流。如果某一 NAT 地址同时承载多种用户群体，仅靠 IP 无法区分它们。不要用未经验证的 `10.0.0.0/8` 代表全部校内用户；根据实际网络替换示例。

## 2. 准备配置

复制 `config.example.json`，填写两个目标和可信代理。以下例子展示同机 Nginx、IPv6，以及 VPN 排除池：

```json
{
  "listen_addr": "127.0.0.1:8080",
  "iprange_file": "campus.txt",
  "exclude_iprange_file": "excluded.txt",
  "internal_url": "https://internal.example.edu/jsxsd/",
  "external_url": "https://vpn.example.edu/jsxsd/",
  "trusted_proxies": "127.0.0.1/32,::1/128",
  "health_path": "/health",
  "ready_path": "/ready",
  "reload_interval": "1m",
  "request_timeout": "5s",
  "shutdown_timeout": "10s",
  "redirect_code": 302,
  "check_upstream": false,
  "ready_check_upstream": false,
  "log_json": true,
  "log_query": false
}
```

相对网段路径基于这个 JSON 文件所在目录，因此目录可整体迁移。环境变量和 CLI 的路径则基于进程工作目录，优先级高于 JSON。配置文件不支持注释，也不会自动替换 `${ENV_VAR}`。

`campus.txt` 示例：

```text
# 校内 IPv4（实际部署请替换）
10.0.0.0/8
# 文档 IPv6 示例
2001:db8::/32
```

`excluded.txt` 示例：

```text
# 示例 VPN / 访客池，匹配后强制使用校外入口
10.20.0.0/16
2001:db8:ffff::/48
```

同一个 IP 同时匹配两份规则时，排除规则优先。`10.1.2.3` 进入校内目标，`10.20.1.2` 进入校外目标。IPv4-mapped IPv6 地址也参与 IPv4 网段匹配。

## 3. Linux / systemd 部署

构建：

```bash
go build -trimpath -o jwc-302 .
# 其他操作系统交叉编译到 Linux
GOOS=linux GOARCH=amd64 go build -trimpath -o jwc-302 .
```

PowerShell 交叉编译时，设置 `$env:GOOS='linux'` 与 `$env:GOARCH='amd64'`；构建后用 `Remove-Item Env:GOOS, Env:GOARCH` 清除，避免影响后续本机测试。

下面假设系统已有 `www-data` 用户和组；其他发行版请创建专用账户并同步修改服务文件：

```bash
sudo install -d -m 755 /opt/jwc-302
sudo install -d -m 750 -o root -g www-data /etc/jwc-302
sudo install -m 755 jwc-302 /opt/jwc-302/jwc-302
sudo install -m 640 -o root -g www-data config.example.json /etc/jwc-302/config.json
sudo install -m 640 -o root -g www-data iprange.txt /etc/jwc-302/iprange.txt
# 编辑 config.json，替换示例目标和网段。
# 若启用排除文件，将它安装到 JSON 指定的路径，保持 www-data 可读。
sudo -u www-data /opt/jwc-302/jwc-302 -config=/etc/jwc-302/config.json -check-config
sudo install -m 644 jwc-302.service /etc/systemd/system/jwc-302.service
sudo systemctl daemon-reload
sudo systemctl enable --now jwc-302
sudo systemctl status jwc-302
sudo journalctl -u jwc-302 -f
```

成功校验返回退出码 0，输出类似：

```json
{"status":"valid","ip_ranges":3,"excluded_ranges":0}
```

校验不会绑定监听端口或探测上游，不能代替实际请求验收。建议以服务账户执行，及早发现文件权限问题。

可选的 `/etc/jwc-302/environment` 是 systemd 环境文件，格式见 `.env.example`。其中的非空值会覆盖 JSON；只保留确实要覆盖的键，避免复制整份模板后意外覆盖 JSON 中的生产地址。

服务模板中的 `ExecStartPre` 会阻止错误配置启动。`ProtectSystem=strict` 限制服务写入系统目录；网段由外部运维过程维护。默认 `TimeoutStopSec=20s`，应大于 `shutdown_timeout` 并留有余量。

## 4. 配置反向代理

同机 Nginx 的 HTTPS 虚拟主机示例：

```nginx
server {
    listen 443 ssl;
    server_name gateway.example.edu;
    ssl_certificate /etc/nginx/tls/gateway.crt;
    ssl_certificate_key /etc/nginx/tls/gateway.key;

    # 可根据监控系统来源调整允许地址；保留与网关配置相同的路径。
    location = /health {
        allow 127.0.0.1;
        allow ::1;
        deny all;
        proxy_pass http://127.0.0.1:8080;
    }
    location = /ready {
        allow 127.0.0.1;
        allow ::1;
        deny all;
        proxy_pass http://127.0.0.1:8080;
    }
    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    }
}
```

网关配置 `trusted_proxies=127.0.0.1/32,::1/128`。若代理不在同机，监听管理网地址，仅信任实际代理 CIDR，并限制直接访问端口。

多层代理中，例如直连地址为本机 Nginx，XFF 是 `198.51.100.88, 192.0.2.10`，且 `192.0.2.10` 为已知负载均衡器：只有将它和本机 Nginx 都加入可信代理，网关才会继续向左识别 `198.51.100.88`。不要信任整个客户端网段。XFF 中的格式错误、透传而不追加的头，以及缺失的代理地址，都可能导致错误分流。

入口 TLS 只保护浏览器到入口的连接。跳转后的目标也必须使用 HTTPS 才能保护后续流量；网关不会自动升级目标协议。

## 5. 上线验收

在与服务相同的账户和工作目录下运行 `-check-config`。然后分别从校内、校外、IPv6 和 VPN / 访客网络验证 HTTP 状态和完整 Location，不仅检查页面能否打开。

在已信任本机代理的测试环境，可用以下命令模拟来源：

```bash
curl -I -H 'X-Forwarded-For: 10.1.2.3' 'http://127.0.0.1:8080/course?id=1&id=2'
curl -I -H 'X-Forwarded-For: 10.20.1.2' 'http://127.0.0.1:8080/course?id=1&id=2'
curl -I -H 'X-Forwarded-For: 2001:db8::123' 'http://127.0.0.1:8080/jsxsd/course?q=%2F'
curl -I -H 'X-Forwarded-For: 10.1.2.3,198.51.100.88' http://127.0.0.1:8080/course
curl -i http://127.0.0.1:8080/health
curl -i http://127.0.0.1:8080/ready
```

最后一条伪造链示例应使用校外入口：右侧不可信地址优先于左侧声明的校内 IP。关闭 trusted-proxies 后，所有模拟头都应失效。

需要保留 POST 方法时设置 `redirect_code=307`，再用本单位允许的测试表单验证目标确实收到原方法、请求体和查询参数。不要使用携带真实认证票据的请求进行公共日志排障。

## 6. 变更、热重载与回滚

1. 备份当前二进制、JSON、校内规则和排除规则。
2. 在独立的暂存目录放置完整的新 JSON 和两份规则，运行指向该暂存配置的 `-check-config`。
3. 将正式网段文件通过同目录临时文件写完后再 `mv` 替换，保持权限和所有权。
4. 等待一个重载周期，检查 `/ready`、`last_reload` 和 `rules_stale`，再执行来源分流验收。
5. 若修改 JSON、环境变量、目标地址、可信代理或超时，重新校验并执行 `systemctl restart jwc-302`。

每次重载会先读取两份文件，再作为一个内存快照替换。两次文件读取本身不构成磁盘事务：如果两份规则必须同时变更且中间组合不可接受，应在维护窗口关闭定时重载（`reload_interval=0`），成组更新并校验后重启，或切换到包含完整新文件路径的 JSON 配置再重启。

运行中任一规则文件出错时：旧路由继续工作，`/health` 默认 200，`/ready` 返回 503；下一次读取成功后自动恢复。修复或回滚两份规则后观察新的 `last_reload`。不要仅通过重启掩盖规则错误：错误文件会使新进程启动失败。

回滚二进制时同步回滚配置格式。旧版本不认识新的 CLI / JSON 参数；保留与版本对应的配置和服务文件。

## 7. 监控与故障排查

通过 `metrics_path: "/metrics"` 或 `-metrics=/metrics` 启用 Prometheus 指标；默认关闭，启用后与业务共用监听地址。使用 `curl http://127.0.0.1:8080/metrics` 验证，并在 Prometheus 中配置抓取目标。通过绑定地址或反向代理访问控制限制指标访问。指标涵盖跳转、规则重载及状态、上游实际探测与耗时、构建信息；标签不包含客户端 IP 或 URL。

上游探测成功和失败都缓存 `upstream_cache_ttl`（默认 5 秒），并发请求共享一轮探测。设置为 `0` 关闭结果缓存。状态变化最多经过一个缓存周期才反映；健康 JSON 的 `upstream_checked_at` 标识本轮探测完成时间。规则重载失败不受缓存影响。

发布包附带 `SHA256SUMS`，部署前在下载目录运行 `sha256sum -c SHA256SUMS --ignore-missing`，并用 `jwc-302 -version` 核对版本和提交。保留旧安装包以便回滚。

推荐监控策略：

- **存活**：探测 `/health`。保持 `check_upstream=false`，避免上游故障触发网关重启风暴。
- **就绪**：负载均衡器探测 `/ready`。规则过期时摘流；恢复后自动加回。
- **上游**：需要时单独启用 `ready_check_upstream=true`。网关必须能同时访问两个目标；任一故障都会使该实例不就绪。单实例部署应评估摘流后的用户体验。
- **规则**：对 `rules_stale=true` 立即告警；`last_reload` 表示最近一次成功读取，并非文件最近修改时间。
- **日志**：采集 JSON 的 `real_ip`、`matched`、`excluded`、`status`，限制访问并设置保留周期；默认不采集查询参数。

| 现象 | 排查步骤 |
|---|---|
| 全部走校外 | 校内文件是否为空；代理是否可信；实际源地址是否为 NAT / IPv6；是否命中排除池 |
| 不应信任的请求走校内 | 检查过宽的可信代理列表，以及代理是否正确追加转发头 |
| 重载后不就绪 | 查看 journal 中的行号和文件错误；确认两份规则存在且服务账户可读 |
| 日志没有查询串 | 默认隐去；必要时受控启用 `log_query=true`，完成后关闭并重启 |
| 启动报未知字段 | 检查 JSON 拼写、重复键、版本对应关系；不支持 null 或注释 |
| 规则校验通过但无法监听 | `-check-config` 不绑定端口；检查地址、端口占用和防火墙 |
| 健康检查超时 | 检查是否开启上游探测、目标是否可达；减少检查频率并评估超时预算 |
| `/ready` 与业务冲突 | 更改 `ready_path` 或使用 `-ready=` 关闭，更新监控配置 |
| JSON 修改不生效 | JSON 不热加载；检查高优先级环境变量 / CLI 是否覆盖，校验后重启 |

上游探测接受 2xx/3xx、不会继续跟随跳转，因此无法确认登录业务正常。正式上线仍需从实际用户网络进行浏览器验收。
