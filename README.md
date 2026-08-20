# Cloud ProxyPool

> 基于腾讯云函数 (SCF) 的分布式 IP 代理池  
> A distributed IP proxy pool powered by Tencent Cloud Functions

---

## ✨ 特性 (Features)

- 🌍 **多区域 IP 轮换** - 支持广州/上海/北京/成都等多个云函数节点
- 🔐 **HTTPS 透明代理** - 内置 MITM 中间人攻击支持，一键生成自签 CA 证书
- 🚀 **双协议支持** - HTTP 代理 (10800) + SOCKS5 代理 (10801)
- 🔒 **双重身份认证** - 本地代理支持 HTTP Basic Auth，云函数调用需携带鉴权 Token
- 🛡️ **SSRF 防护** - 云函数拒绝访问内网/保留地址与云元数据服务
- 💾 **流量录制** - 开启 `dump` 模式可将所有请求/响应写入日志
- ⚡ **智能熔断** - 自动检测并暂时屏蔽失败节点，提升稳定性
- 📊 **Web 监控面板** - 实时查看 QPS、成功率、节点健康状态 (http://127.0.0.1:8081)
- 🚢 **一键部署** - 自动化脚本批量部署云函数到多个区域，自带状态轮询与失败自愈
- 📸 **运行截图** - [查看项目运行预览](#-使用截图)

---

## 📸 使用截图

![CloudProxyPool Usage](https://i.imgur.com/Aulg001.png)

---

## 📦 项目结构

```
CloudProxyPool/
├── client/                  # 客户端 (Go)
│   ├── certs/              # 自动生成的 MITM CA 证书 (首次运行创建)
│   ├── config/             # 配置解析
│   ├── cloud/              # 云函数调用 + Token 鉴权 + 熔断逻辑
│   ├── proxy/              # HTTP/SOCKS5 代理核心
│   ├── dashboard/          # Web 监控面板
│   └── main.go             # 入口文件
├── server/                  # 云函数代码 (Python, 仅标准库)
│   └── index.py            # 云函数入口
├── deploy/                  # 自动化部署工具
│   ├── deploy.py           # 部署脚本
│   ├── deploy.toml.example # 部署配置模板 (复制为 deploy.toml 并填入密钥)
│   └── pyproject.toml      # Python 依赖 (uv)
└── README.md               # 本文档
```

> `deploy/deploy.toml` 与 `client/config.toml` 含密钥/Token，由部署脚本生成，已被 `.gitignore` 排除，**切勿提交到仓库**。

---

## 🚀 快速开始

### 1. 部署云函数

```bash
cd deploy
cp deploy.toml.example deploy.toml
# 编辑 deploy.toml，填入腾讯云 SecretId/SecretKey

uv run deploy.py
# 或: pip install -r requirements.txt && python deploy.py
```

部署脚本会自动完成：打包服务端代码 → 创建/更新各区域函数（轮询等待就绪，`CreateFailed` 僵尸函数自动删除重建）→ 下发鉴权 Token → 配置函数 URL → 健康检查 → 生成 `client/config.toml`。

### 2. 编译并启动客户端

```bash
cd ../client
go build -o cloud-proxy .
./cloud-proxy -C config.toml
```

首次启动会自动生成 CA 证书到 `certs/` 目录。

### 3. 配置代理

**HTTP 代理 (推荐):**

```bash
# Windows PowerShell
$env:http_proxy="http://127.0.0.1:10800"
$env:https_proxy="http://127.0.0.1:10800"

# Linux/Mac
export http_proxy=http://127.0.0.1:10800
export https_proxy=http://127.0.0.1:10800
```

**SOCKS5 代理:**

```bash
curl -x socks5://127.0.0.1:10801 http://myip.ipip.net
```

### 4. 安装 CA 证书 (HTTPS 必需)

**Windows:**
1. 双击 `certs/ca.crt`
2. 点击"安装证书"
3. 选择"受信任的根证书颁发机构"

**Linux/Mac:**
```bash
# Ubuntu/Debian
sudo cp certs/ca.crt /usr/local/share/ca-certificates/cloud-proxy-ca.crt
sudo update-ca-certificates

# Mac
sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain certs/ca.crt
```

> Firefox 使用独立的证书库，需在 设置 → 隐私与安全 → 证书 中单独导入。

---

## ⚙️ 配置文件

`client/config.toml` 示例（由 `deploy.py` 自动生成）：

```toml
[client]
listen_addr = "127.0.0.1:10800"  # HTTP 代理监听地址
socks_addr = ":10801"            # SOCKS5 监听地址 (可选)
dashboard_addr = ":8081"         # 监控面板地址 (可选)

# 认证 (可选)
# user = "admin"
# password = "supersecret"

# 流量录制 (可选)
# dump = true
# dump_file = "traffic.log"

debug = false

[cloud]
# 由 deploy.py 自动生成
function_urls = [
    "https://your-appid-xxxx.ap-shanghai.tencentscf.com",
    "https://your-appid-yyyy.ap-guangzhou.tencentscf.com"
]
region = "multi-region"
token = "部署时自动生成的鉴权 Token"   # 必须与 deploy.toml 中 auth_token 一致
```

---

## 🔐 安全机制

**云函数鉴权 (Token)**

函数 URL 是公网可达的。所有调用必须携带 `X-Auth-Token` 请求头且与函数环境变量 `AUTH_TOKEN` 匹配，否则返回 403。Token 在首次部署时自动生成，保存在三处并保持同步：

- `deploy/deploy.toml` → `[security] auth_token`
- `client/config.toml` → `[cloud] token`
- 云函数环境变量 `AUTH_TOKEN`

**轮换 Token**：修改 `deploy.toml` 中的 `auth_token` 后重跑 `uv run deploy.py`，客户端配置会同步更新。

**SSRF 防护**

云函数拒绝代理访问内网/保留地址（`127.0.0.0/8`、`169.254.0.0/16`、`10.0.0.0/8` 等）及云平台元数据服务，防止函数被当作内网探测跳板。

**本地文件卫生**

以下敏感文件已被 `.gitignore` 排除：`deploy/deploy.toml`（云 API 密钥）、`client/config.toml`（函数 URL + Token）、`client/certs/*.key`（MITM 私钥）。

---

## ⚠️ 已知限制

- **云函数出口仅 IPv4**：IPv6 目标无法代理。客户端会在 SOCKS 握手阶段直接拒绝 IPv6 目标，应用（如微信）一般会自动回退到 IPv4 服务器。
- **无状态请求转发**：每个请求对应一次独立的函数调用，不支持长连接/WebSocket。
- **超时与大小限制**：上游请求超时 10 秒，单次响应大小受 SCF 响应限制（约 6MB）。
- **账号依赖**：函数依赖腾讯云 CLS 日志服务（未开通会导致函数创建失败）。

---

## 📊 Web 监控面板

启动客户端后访问 `http://127.0.0.1:8081`：

- **实时统计**: 总请求数、成功数、失败数
- **节点状态**: 每个云函数 URL 的健康状态和失败计数
- **熔断监控**: 显示哪些节点正在冷却

---

## 🛡️ 智能熔断机制

当某个云函数节点连续失败 **5 次**时，会被自动标记为不健康并暂停使用 **2 分钟**。

冷却期结束后自动恢复，无需手动干预。

---

## 📝 流量录制

开启流量录制后，所有请求/响应详情会写入 `traffic.log`：

```toml
[client]
dump = true
dump_file = "traffic.log"
```

日志格式示例：

```
[2026-01-21 14:00:00] REQUEST: GET http://example.com/
> User-Agent: curl/7.68.0
> Host: example.com

--------------------------------------------------
[2026-01-21 14:00:01] RESPONSE: http://example.com/ -> 200 (Size: 1256 bytes)
==================================================
```

---

## 🔐 HTTP Basic Auth

编辑 `config.toml` 启用本地代理认证：

```toml
[client]
user = "admin"
password = "your_strong_password"
```

客户端使用：

```bash
curl -x http://admin:your_strong_password@127.0.0.1:10800 http://ipinfo.io
```

---

## 🌐 支持的场景

✅ **爬虫 IP 轮换**  
✅ **IP接口测试**  
✅ **绕过 IP 限制**  
✅ **HTTPS 流量抓包**  
✅ **Burp Suite / Proxifier 联动**  
✅ **端口扫描 (SOCKS5 模式)**

---

## 🔧 故障排查

### 1. HTTPS 提示证书错误

➡️ 确保已安装 `certs/ca.crt` 到系统受信任根证书

### 2. 云函数调用失败

➡️ 检查 `config.toml` 中的 `function_urls` 是否正确  
➡️ 运行 `deploy.py` 重新部署并更新配置

### 3. 返回 403 Unauthorized

➡️ `config.toml` 中的 `token` 与函数侧 Token 不一致，重跑 `deploy.py` 同步，或确认使用的是新版客户端（旧版不携带 Token）

### 4. 部署报 `CreateFailed` / `CLS service is unregistered`

➡️ 前腾讯云控制台开通日志服务 CLS，然后重跑 `deploy.py`（脚本会自动删除失败的僵尸函数并重建）

### 5. 报 `Network is unreachable` 且目标为 IPv6 地址

➡️ 云函数出口仅 IPv4，客户端已自动拒绝 IPv6 目标；若应用强制只走 IPv6，需在系统层面禁用 IPv6

### 6. SOCKS5 无法连接

➡️ 确认 `config.toml` 中已配置 `socks_addr = ":10801"`  
➡️ 重启客户端

### 7. Web 面板打不开

➡️ 确认 `dashboard_addr = ":8081"` 已配置  
➡️ 检查 8081 端口是否被占用

---

## 📄 许可证

MIT License

---

## 🙏 致谢

- [25smoking/CloudProxyPool](https://github.com/25smoking/CloudProxyPool) - 本项目基于其 fork 而来
- [goproxy](https://github.com/elazarl/goproxy) - HTTP 代理核心
- [go-socks5](https://github.com/armon/go-socks5) - SOCKS5 协议实现
- 腾讯云函数 (SCF) - 无服务器计算平台

---

**⚠️ 免责声明**: 本工具仅供学习和合法用途，请勿用于非法活动。使用者需遵守当地法律法规。
