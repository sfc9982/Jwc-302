# Jwc-302

[![CI](https://github.com/sfc9982/Jwc-302/actions/workflows/ci.yml/badge.svg)](https://github.com/sfc9982/Jwc-302/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go)](https://go.dev/)

面向高校基础设施管理员的教务入口分流网关：根据客户端 IPv4 / IPv6 的 CIDR 归属，将浏览器重定向到校内教务系统或校外 WebVPN，保留有效请求的路径与原始查询参数。

网关仅返回跳转地址，登录、页面和下载流量由浏览器直接连接目标系统。IP 分流用于选择入口；目标系统仍需独立鉴权。仓库内的域名、网段和配置均为通用示例，上线前必须替换。

## 运维功能

- **分层配置**：命令行 > 环境变量 > JSON 配置文件 > 默认值；错误的被覆盖环境变量不会阻止启动。
- **上线前校验**：`-check-config` 校验配置、校内与排除网段文件后退出，不监听端口、不访问上游。
- **网段排除**：VPN / 访客地址池即使处于校内大网段中，也可以强制进入校外入口。
- **一致热重载**：校内与排除文件均解析成功后一起替换；任一失败保留整组旧规则。
- **可选指标**：启用 Prometheus 抓取端点，观察跳转、重载、规则状态和实际上游探测。
- **版本发布**：版本标签通过测试后自动发布 Linux / Windows 安装包和 SHA-256 校验文件。
- **存活与就绪分离**：`/health` 用于进程存活；`/ready` 在网段重载失败时返回 503，可独立开启上游探测。
- **跳转策略**：默认 302 适合入口导航；可选 307 保留客户端请求方法与请求体。
- **默认日志脱敏**：URI 和目标地址的查询参数默认不进入请求日志；真实 Location 保持完整。
- **代理与退出**：可信代理链解析、IPv6、可配置请求与退出超时、停止服务时取消重载任务。

Go 最低版本为 **1.26.0**，推荐工具链固定为 **1.27.4**。依赖使用 `fasthttp v1.78.0`；CI 验证 Go 1.26 和当前稳定版。

## 快速开始

```bash
git clone https://github.com/sfc9982/Jwc-302.git
cd Jwc-302
go mod download
go build -trimpath -o jwc-302 .
# 示例配置中的网段文件路径相对于配置文件所在目录
./jwc-302 -config=config.example.json -check-config
./jwc-302 -config=config.example.json
```

PowerShell 下使用 `go build -o jwc-302.exe .`，并以 `.\jwc-302.exe` 启动。`go` 会按 `toolchain` 指令选择推荐工具链；离线环境请预装相应版本并缓存依赖。

```bash
curl -i http://127.0.0.1:8080/health
curl -i http://127.0.0.1:8080/ready
curl -I 'http://127.0.0.1:8080/course?id=1'
```

Windows PowerShell 使用 `curl.exe`。默认仅监听 `127.0.0.1:8080`；本机不在示例校内网段内，所以第三个请求返回校外入口。示例目标并非可用的业务上游，验证时不要使用 `curl -L` 跟随它。

生产部署、代理链、VPN 排除池、监控与回滚步骤见 **[管理员操作手册](docs/operations.md)**。

## 分流规则

```text
请求 → /health 或 /ready → 健康状态 JSON
     → 其他路径 → 提取可信客户端 IP
                  ├─ 命中排除网段 → 校外入口
                  ├─ 命中校内网段 → 校内入口
                  └─ 其他地址     → 校外入口
                 拼接有效路径与原始查询串 → 302 / 307
```

| 请求 | 来源 | 默认结果 |
|---|---|---|
| `/` | 校内 | `https://internal.example.edu/jsxsd/` |
| `/course?id=1&id=2` | 校内 | `https://internal.example.edu/jsxsd/course?id=1&id=2` |
| `/jsxsd/course?q=%2F` | 校外或排除网段 | `https://vpn.example.edu/jsxsd/course?q=%2F` |
| `/%2e%2e/admin` | 任意 | HTTP 400，无 Location |

已有入口目录时不会重复添加。入口目录由目标基地址决定，可配置为其他路径。目标基地址必须是绝对 HTTP(S) URL，不能包含用户凭据、查询串、片段或目录穿越。跳转响应设置 `Cache-Control: no-store`。

302 对 POST 的后续处理由客户端决定；307 可以保留方法和请求体，但目标系统仍需接受该方法，并满足自身认证、Cookie、CSRF 等要求。

## 配置参考

推荐从 [config.example.json](config.example.json) 复制 JSON 配置，或参考 [.env.example](.env.example)。程序不会自动读取 `.env`，需由 shell、systemd 或容器注入环境变量。

| 命令行 | 环境变量 / JSON 键 | 默认值 | 用途 |
|---|---|---|---|
| `-config` | `CONFIG_FILE` / 不适用 | 空 | 可选 JSON 文件；`-config=` 显式关闭环境变量指定的文件 |
| `-check-config` | 不适用 | false | 校验配置和网段后退出，成功输出 JSON，退出码 0 |
| `-addr` | `LISTEN_ADDR` / `listen_addr` | `127.0.0.1:8080` | 监听地址 |
| `-iprange` | `IPRANGE_FILE` / `iprange_file` | `iprange.txt` | 校内网段文件 |
| `-exclude-iprange` | `EXCLUDE_IPRANGE_FILE` / `exclude_iprange_file` | 空 | 排除文件；匹配后优先使用校外入口 |
| `-internal` | `INTERNAL_URL` / `internal_url` | `https://internal.example.edu/jsxsd/` | 校内基地址 |
| `-external` | `EXTERNAL_URL` / `external_url` | `https://vpn.example.edu/jsxsd/` | 校外基地址 |
| `-health` | `HEALTH_PATH` / `health_path` | `/health` | 存活端点，显式空字符串关闭 |
| `-ready` | `READY_PATH` / `ready_path` | `/ready` | 就绪端点，显式空字符串关闭 |
| `-metrics` | `METRICS_PATH` / `metrics_path` | 空 | Prometheus 指标端点，设置为 `/metrics` 启用 |
| `-upstream-cache-ttl` | `UPSTREAM_CACHE_TTL` / `upstream_cache_ttl` | `5s` | 上游探测结果缓存时间；0 关闭结果缓存 |
| `-trusted-proxies` | `TRUSTED_PROXIES` / `trusted_proxies` | 空 | 可信代理 CIDR，逗号分隔 |
| `-reload-interval` | `RELOAD_INTERVAL` / `reload_interval` | `5m` | 两个网段文件的读取周期；0 关闭 |
| `-request-timeout` | `REQUEST_TIMEOUT` / `request_timeout` | `5s` | 请求读写超时；上游探测使用约一半预算 |
| `-shutdown-timeout` | `SHUTDOWN_TIMEOUT` / `shutdown_timeout` | `10s` | 优雅退出最长等待时间 |
| `-redirect-code` | `REDIRECT_CODE` / `redirect_code` | `302` | 允许 302、307 |
| `-check-upstream` | `CHECK_UPSTREAM` / `check_upstream` | false | 兼容选项：两个健康端点均探测上游 |
| `-ready-check-upstream` | `READY_CHECK_UPSTREAM` / `ready_check_upstream` | false | 仅就绪端点探测上游，适合负载均衡器 |
| `-log-json` | `LOG_JSON` / `log_json` | false | JSON 日志 |
| `-log-query` | `LOG_QUERY` / `log_query` | false | 是否记录查询参数，可能包含认证票据 |

JSON 中时长必须是字符串（如 `"5s"`）；布尔值和状态码使用原生 JSON 类型。配置文件拒绝未知字段、重复键、null、尾随数据及超过 1 MiB 的内容。文件本身始终进行结构和类型校验；最终合并后的配置再做语义校验。

JSON **显式设置**的网段相对路径基于 JSON 文件目录；CLI、环境变量和默认路径基于进程工作目录。空环境变量按未设置处理；需要清空低优先级配置时，使用 `-exclude-iprange=`、`-ready=` 等显式 CLI 参数。环境变量布尔值支持 `true/false`、`1/0`、`yes/no`、`on/off`；CLI 布尔参数使用 `-log-json=false` 等形式。

配置文件只在启动时读取。定时热重载仅作用于校内与排除 CIDR 文件；目标地址、代理列表、日志策略等修改后需要重新校验并重启服务。

### CIDR 文件

每行一个 IPv4 / IPv6 CIDR，允许空行和以 `#` 开头的整行注释，不支持行尾注释。

- [iprange.txt](iprange.txt) 是示例校内规则，包含 `10.0.0.0/8` 和文档示例网络。
- [examples/excluded-iprange.txt](examples/excluded-iprange.txt) 演示从大网段中排除 `10.20.0.0/16` 等 VPN / 访客池。
- 空校内文件有效，所有请求使用校外入口；空排除文件有效，相当于不排除。
- 启动时任一启用文件缺失或错误，启动失败；运行中错误则保留整个旧快照，标记规则过期。

### 可信代理

直连 IP 不可信时，忽略转发头。可信时优先从 `X-Forwarded-For` 右向左跳过已知代理，使用首个不可信 IP；已检查部分出现非法 IP 则回退直连 IP。链条全部可信时使用最左侧 IP；只有没有非空 XFF 时才尝试 `X-Real-IP`。

可信代理必须覆盖或正确追加转发头。不要把整个校园网段或任意来源配置为可信代理。具体单层和多层代理例子见操作手册。

## 监控与日志

| 端点/选项 | 行为 |
|---|---|
| `/health` | 默认返回 200；规则重载失败仍表示进程存活，同时返回 `rules_stale=true` |
| `/ready` | 规则重载失败返回 503；下一次成功重载自动恢复 200 |
| `ready_check_upstream=true` | 仅 `/ready` 并行 GET 两个目标基地址；任一失败返回 503 |
| `check_upstream=true` | 保留原行为：`/health` 与 `/ready` 均受上游探测结果影响 |

```json
{
  "status": "ok",
  "time": "2026-11-11T12:00:00+08:00",
  "ip_ranges": 3,
  "excluded_ranges": 1,
  "last_reload": "2026-11-11T11:59:00+08:00",
  "rules_stale": false
}
```

探测接受 2xx/3xx，不跟随重定向，响应体限制 1 MiB。结果只代表网关服务器到上游的可达性，不验证最终登录页面。上游探测结果默认缓存 5 秒，并发请求共用同一轮探测；上游状态变化最多延迟一个缓存周期反映。缓存关闭时仍会合并并发探测；使用示例目标时保持探测关闭。

默认请求日志的 `uri`、`target` 不含查询串，`Location` 仍保留原查询参数。`log_query=true` 仅用于受控排障，结束后关闭。日志包含客户端 IP，请按本单位的访问与保留策略管理。

## 测试与 CI

测试使用临时文件、真实本机 TCP 服务、模拟上游以及编译后的网关程序，不依赖实际教务站点或 VPN。

```bash
# 随机顺序重复运行功能测试
go test -shuffle=on -count=3 -coverprofile=coverage.out ./...
go tool cover -func=coverage.out
# PowerShell 下将整个参数加引号：'-coverprofile=coverage.out'

# 编译并启动真实进程，验证 JSON/CLI、热重载及恢复
go test -tags=integration -run '^TestGatewayProcess$' -count=1 -timeout=180s -v ./...
go vet -tags=integration ./...

# 并发检测（需要支持 race 的平台和 C 编译器）
go test -race -shuffle=on -count=2 ./...

# URL 保真和目标边界模糊测试
go test -run='^$' -fuzz='^FuzzBuildTarget$' -fuzztime=20s -parallel=2 .
```

[CI](.github/workflows/ci.yml) 在推送到 `master`、PR、手动触发时运行 Linux Go 1.26、Linux 稳定版、Windows 稳定版功能测试，另有 Linux race 和 fuzz。设置 `GOTOOLCHAIN=local`，确保最低版本任务不会自动切换到推荐工具链。覆盖率报告保存 14 天；fuzz 失败会上传复现输入。

测试覆盖配置优先级和严格 JSON 校验、IP/代理信任边界、排除网段、原子规则更新、307 实际 POST 方法/请求体保留、日志查询脱敏、健康与就绪隔离、失败恢复和 Linux SIGTERM 退出。Windows 不执行 POSIX SIGTERM 断言，由 Linux CI 覆盖。

[Dependabot](.github/dependabot.yml) 每周检查 Go 模块和 Actions 更新。

### 发布与性能基线

`-version` 输出版本、提交、构建时间和 Go 版本，无需配置文件。推送 `vMAJOR.MINOR.PATCH` 或带预发布后缀的标签后，CI 通过会自动公开 Release，附带 Linux amd64 / arm64、Windows amd64 安装包及 `SHA256SUMS`。本地构建显示 `dev`。

性能基准及实测结果见 [性能报告](docs/performance.md)；手动运行 Performance baseline 工作流可保存新的测量结果。

## 升级说明

从早期版本升级时：

1. 将构建环境从 Go 1.21 升级至 Go 1.26+，推荐 1.27.4。
2. 默认请求日志不再含查询串；如采集规则依赖完整 URI，请调整。
3. 新增 `/ready` 保留路径。已有业务使用该路径，或已将 `/ready` 配为存活端点时，设置不同的 `ready_path` 或 `-ready=`。
4. 新 systemd 模板使用 `/etc/jwc-302/config.json` 和可选 `/etc/jwc-302/environment`；旧 `/opt/jwc-302/.env` 不再自动加载，需迁移。
5. 使用 `-check-config` 验证后再切换二进制。一般配置变更需要重启，只有 CIDR 文件定时热重载。

## 代码结构

```text
main.go                       服务生命周期、分流、代理 IP、URL 与健康检查
config.go                     CLI / 环境变量 / JSON 合并及校验
ranges.go                     CIDR 读取、快照替换、重载状态与任务取消
main_test.go / behavior_test.go 基础回归、HTTP、并发与 fuzz
operators_test.go             配置、排除规则、监控和日志等运维功能测试
process_integration_test.go    真实可执行程序集成测试
config.example.json           通用 JSON 配置模板
.env.example                  环境变量参考
jwc-302.service               systemd 模板
examples/                     示例排除网段
docs/operations.md            部署、验收、监控和回滚手册
```

提交前执行 `gofmt -w *.go`，为行为变化补充功能测试。请勿提交真实机构标识、环境专属网段、地址或凭据。
