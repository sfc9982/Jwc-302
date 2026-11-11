# Jwc-302

[![CI](https://github.com/sfc9982/Jwc-302/actions/workflows/ci.yml/badge.svg)](https://github.com/sfc9982/Jwc-302/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.21%2B-00ADD8?logo=go)](https://go.dev/)

基于 Go 的轻量教务系统入口分流网关。按客户端 IP 的 CIDR 归属，将浏览器 **302 重定向**到校内系统或校外 WebVPN 入口，并保留有效请求的路径与原始查询参数。

适用于需要为多个网络环境提供统一入口的部署。网关仅返回跳转地址，后续登录、页面和下载请求由浏览器直接发送到目标系统。

> 仓库内的域名和网段均为示例。上线前请替换成自己部署环境的地址及网段；本项目不提供教务系统或 VPN 服务，也不会代替目标系统的认证。

## 请求如何分流

```text
浏览器 → 网关
          ├─ /health → JSON 健康状态
          └─ 其他路径 → 获取客户端 IP → 匹配 CIDR
                                        ├─ 命中 → 校内入口
                                        └─ 未命中 → 校外入口
                                      HTTP 302 + Location
```

以默认入口目录 `/jsxsd/` 为例：

| 请求 | 网段匹配 | 响应 Location |
|---|---|---|
| `/` | 命中 | `https://internal.example.edu/jsxsd/` |
| `/course?id=1&id=2` | 命中 | `https://internal.example.edu/jsxsd/course?id=1&id=2` |
| `/jsxsd/course?q=%2F` | 未命中 | `https://vpn.example.edu/jsxsd/course?q=%2F` |
| `/%2e%2e/admin` | 任意 | HTTP 400，无 Location |

入口目录来自配置的基地址，可以替换成其他系统路径。所有普通请求方法目前统一返回 302；浏览器可能在跳转时改变 POST 方法，因此建议将网关用于 GET/HEAD 入口导航。

## 功能

- IPv4 / IPv6 CIDR 匹配；未命中时使用校外入口。
- 路径与原始查询串透传，已有入口目录时避免重复添加。
- 固定跳转目标的协议和主机，拒绝目录穿越、反斜杠及非 origin-form URI。
- 可信代理转发头解析，支持 `X-Forwarded-For` 与 `X-Real-IP`。
- 网段文件定时热重载，解析失败保留上一次有效规则。
- JSON / 文本请求日志、可选上游健康探测、请求读写超时。
- SIGINT / SIGTERM 优雅退出，最多等待 10 秒。
- 单个可执行文件，无数据库依赖；附带 systemd 与 Nginx 配置示例。

## 快速开始

需要 Go 1.21 或更高版本。

```bash
git clone https://github.com/sfc9982/Jwc-302.git
cd Jwc-302
go mod download
go build -o jwc-302 .
./jwc-302
```

默认仅监听 `127.0.0.1:8080`。PowerShell 下编译和运行使用：

```powershell
go build -o jwc-302.exe .
.\jwc-302.exe
```

在另一个终端检查服务；Windows PowerShell 请使用 `curl.exe`：

```bash
curl -i http://127.0.0.1:8080/health
curl -I 'http://127.0.0.1:8080/course?id=1'
```

健康端点默认返回 200；第二条请求从本机发起，按示例规则会返回校外入口的 302。不要使用 `curl -L` 验证示例目标，因为示例地址不代表可用的上游服务。

### 配置网段

`iprange.txt` 每行一个 CIDR，允许空行及以 `#` 开头的整行注释，不支持行尾注释：

```text
# 替换为实际的校内网段
10.0.0.0/8
# 仅供文档演示的网段
192.0.2.0/24
2001:db8::/32
```

IPv6 校内地址也必须配置对应规则。空文件是有效配置，会使全部请求进入校外入口。启动时文件不存在或格式错误，进程退出；运行中重载失败则保留旧规则并记录警告。默认每 5 分钟读取一次，可用 `-reload-interval=1m` 调整，`0` 关闭。

更新规则时，建议先写入同目录临时文件，再原子替换正式文件，避免读取到写入中的内容。

### 本地验证两种入口

启动服务时显式信任本机测试代理：

```bash
./jwc-302 -trusted-proxies=127.0.0.1/32
```

然后发送两种来源的请求：

```bash
# 示例校内来源 → internal.example.edu
curl -I -H 'X-Forwarded-For: 10.1.2.3' http://127.0.0.1:8080/course
# 示例校外来源 → vpn.example.edu
curl -I -H 'X-Forwarded-For: 198.51.100.2' http://127.0.0.1:8080/course
```

生产环境只信任实际代理的地址。直接暴露服务时，保持 `TRUSTED_PROXIES` 为空，客户端自行提供的转发头会被忽略。

## 配置参考

配置优先级为命令行参数 > 环境变量 > 默认值。当前时长和布尔环境变量会先做格式校验，即使传入命令行覆盖值，格式错误的对应环境变量仍会使进程退出。

程序**不会自动读取 `.env`**。手动启动请先导出环境变量；systemd 服务会读取可选的 `/opt/jwc-302/.env`，可从 `.env.example` 复制。

| 参数 | 环境变量 | 默认值 | 含义 |
|---|---|---|---|
| `-addr` | `LISTEN_ADDR` | `127.0.0.1:8080` | 监听地址；直接对外服务时显式设为 `:8080` |
| `-iprange` | `IPRANGE_FILE` | `iprange.txt` | CIDR 文件；相对路径基于工作目录 |
| `-internal` | `INTERNAL_URL` | `https://internal.example.edu/jsxsd/` | 校内入口基地址 |
| `-external` | `EXTERNAL_URL` | `https://vpn.example.edu/jsxsd/` | 校外入口基地址 |
| `-health` | `HEALTH_PATH` | `/health` | 健康端点；命令行传 `-health=` 可关闭 |
| `-trusted-proxies` | `TRUSTED_PROXIES` | 空 | 可信代理 CIDR，逗号分隔 |
| `-reload-interval` | `RELOAD_INTERVAL` | `5m` | 网段重载周期，非负；`0` 关闭 |
| `-request-timeout` | `REQUEST_TIMEOUT` | `5s` | 正数；请求读写超时，单个上游探测使用约一半预算 |
| `-check-upstream` | `CHECK_UPSTREAM` | `false` | 健康端点是否探测两个上游 |
| `-log-json` | `LOG_JSON` | `false` | 输出 JSON 日志 |

环境变量布尔值接受 `true/false`、`1/0`、`yes/no`、`on/off`，忽略大小写。空环境变量按未设置处理；CLI 布尔参数使用 `-log-json=true` / `-log-json=false`。

目标基地址必须是绝对 HTTP(S) URL，不能包含用户名、密码、查询串、片段或目录穿越路径；尾部斜杠会自动补齐。

### 可信代理规则

1. 检查直连 IP；不可信时忽略所有转发头。
2. 存在非空 `X-Forwarded-For` 时，从右向左跳过可信代理，使用首个不可信 IP。
3. 已检查的链条部分存在非法 IP 时，回退到直连 IP，不再使用 `X-Real-IP`。链条中更左侧、不参与判断的值不会继续解析。
4. 链条全部可信时使用最左侧 IP。
5. 没有非空 `X-Forwarded-For` 时，尝试 `X-Real-IP`，否则使用直连 IP。

代理必须覆盖或正确追加转发头。不要将整个校内网段或 `0.0.0.0/0` 配为可信代理。

## 健康检查

```bash
curl http://127.0.0.1:8080/health
```

默认只检查进程存活，不访问上游：

```json
{"status":"ok","time":"2026-11-11T12:00:00+08:00","ip_ranges":3}
```

启用 `CHECK_UPSTREAM=true` 后，两个基地址会并行执行 GET 探测：

```json
{
  "status": "degraded",
  "time": "2026-11-11T12:00:00+08:00",
  "ip_ranges": 3,
  "upstream": {"internal": "ok", "external": "status:503"}
}
```

- 两个上游均返回 2xx / 3xx：HTTP 200，`status=ok`。
- 任一失败或超时：HTTP 503，`status=degraded`，附带原因。
- 探测不跟随重定向，响应体限制为 1 MiB；3xx 不代表最终登录页面正常。
- 每次健康请求都会发起探测；该结果反映网关服务器的可达性，不代表所有用户网络的可达性。
- 使用示例目标时保持上游探测关闭；部署后再按监控需求开启。

## 部署

### Linux / systemd

交叉编译（Bash）：

```bash
GOOS=linux GOARCH=amd64 go build -trimpath -o jwc-302 .
```

PowerShell：

```powershell
$env:GOOS='linux'
$env:GOARCH='amd64'
go build -trimpath -o jwc-302 .
Remove-Item Env:GOOS, Env:GOARCH
```

将二进制、实际网段文件和配置文件部署到 `/opt/jwc-302/`，再安装仓库自带的服务文件：

```bash
sudo install -d -m 755 /opt/jwc-302
sudo install -m 755 jwc-302 /opt/jwc-302/jwc-302
sudo install -m 644 iprange.txt /opt/jwc-302/iprange.txt
sudo install -m 600 -o root -g root .env.example /opt/jwc-302/.env
# 编辑 /opt/jwc-302/.env 和 iprange.txt，替换示例目标与网段
sudo install -m 644 jwc-302.service /etc/systemd/system/jwc-302.service
sudo systemctl daemon-reload
sudo systemctl enable --now jwc-302
sudo journalctl -u jwc-302 -f
```

服务使用 `www-data` 账户（目标系统需存在此账户），并以 `/opt/jwc-302` 为工作目录。systemd 读取 `.env` 后将变量注入服务；配置文件无需允许服务账户直接读取。

### Nginx

在已有 HTTPS 虚拟主机内添加：

```nginx
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
}
```

对应设置 `TRUSTED_PROXIES=127.0.0.1/32`。TLS 证书由入口代理管理；302 后浏览器直接访问目标，上游也需要支持 HTTPS 才能保护后续连接。

## 日志与运行边界

启用 `LOG_JSON=true` 后，请求日志示例：

```json
{"time":"2026-11-11T12:00:00+08:00","level":"INFO","msg":"redirect","method":"GET","uri":"/course?id=1","direct_ip":"127.0.0.1","real_ip":"10.1.2.3","matched":true,"target":"https://internal.example.edu/jsxsd/course?id=1","status":302}
```

当前日志会记录完整 URI 和目标查询参数；如果业务链接含票据或敏感数据，请在日志采集侧脱敏并限制访问。IP 分流用于选择入口，目标系统仍需独立鉴权。

## 测试与 CI

测试使用临时文件、本机 TCP 服务和本地模拟上游，不依赖真实教务站点或 VPN。CI 不仅检查编译或进程能否启动，还断言实际 HTTP 状态、Location、查询参数、网段变更及错误恢复后的行为。

| 测试层 | 验证内容 |
|---|---|
| 函数测试 | IPv4/IPv6 网段边界、IPv4-mapped 地址、代理链信任、URL 编码及查询保真、配置默认值/环境变量/CLI 覆盖、非法配置退出、文件加载失败保留旧规则 |
| HTTP 功能测试 | 实际 TCP 请求的内外网分流、伪造转发头、重复入口目录、HEAD/POST、非法路径、Location 与禁止缓存响应头 |
| 健康测试 | 自定义/关闭健康端点、JSON 字段、2xx/3xx/4xx/5xx、单边故障、连接失败、超时、超大响应 |
| 真实进程测试 | 编译并启动可执行文件、启动失败、请求分流、有效热重载、无效文件保持旧规则、配置恢复、IPv6 来源、JSON 日志、Linux SIGTERM 优雅退出 |
| 并发测试 | 同时重载规则、处理跳转及健康请求，在 race detector 下检查共享状态 |
| 模糊测试 | 随机 URI 输入；成功输出必须保持目标协议/主机、入口路径边界和原始查询参数 |

本地执行：

```bash
# 函数和 HTTP 功能测试；随机顺序重复运行
go test -shuffle=on -count=3 -coverprofile=coverage.out ./...
go tool cover -func=coverage.out
# PowerShell 中请将整个参数加引号：'-coverprofile=coverage.out'

# 真实可执行程序功能测试
go test -tags=integration -run '^TestGatewayProcess$' -count=1 -timeout=180s -v ./...

# 含进程测试代码的静态检查
go vet -tags=integration ./...

# 并发检测（需要支持 race 的平台和 C 编译器）
go test -race -shuffle=on -count=2 ./...

# URL 模糊测试；CI 使用 20 秒的固定预算
go test -run='^$' -fuzz='^FuzzBuildTarget$' -fuzztime=20s -parallel=2 .
```

[GitHub Actions](.github/workflows/ci.yml) 在每次推送到 `master`、PR 和手动触发时运行：Linux Go 1.21、Linux 稳定版、Windows 稳定版的完整功能测试；另外运行 Linux race 和 fuzz 任务。覆盖率报告作为构件保存 14 天，模糊测试失败时上传复现输入。Windows 不执行 POSIX SIGTERM 断言，该项由 Linux 任务覆盖。

[Dependabot](.github/dependabot.yml) 每周检查 Go 模块和 Actions 更新。

## 项目结构

```text
main.go                      配置、分流、重载、健康检查和服务生命周期
main_test.go                 基础回归测试
behavior_test.go             功能边界、真实 HTTP、并发与 fuzz 测试
process_integration_test.go   真实可执行程序功能测试（integration 标签）
iprange.txt                  示例 CIDR 文件
.env.example                 环境变量模板
jwc-302.service              systemd 服务模板
.github/workflows/ci.yml     跨平台功能测试与 race/fuzz CI
```

提交改动前请执行 `gofmt -w *.go`，为行为变化补充测试，并运行功能测试与静态检查。避免将真实部署凭据、身份信息或环境专属配置提交到仓库。
