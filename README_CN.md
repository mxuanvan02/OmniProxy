<p align="center">
  <a href="https://github.com/mxuanvan02/OmniProxy">
    <picture>
      <img src="web/icon.svg" alt="OmniProxy" style="width: 25%;">
    </picture>
  </a>
</p>

# OmniProxy
<div align="center">
  <a href="https://go.dev/">
    <img src="https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat&logo=go" alt="Go Version">
  </a>
  <a href="https://www.docker.com/">
    <img src="https://img.shields.io/badge/Docker-Ready-2496ED?style=flat&logo=docker" alt="Docker">
  </a>
  <a href="LICENSE">
    <img src="https://img.shields.io/badge/License-MIT-green.svg" alt="License">
  </a>
  <a href="https://github.com/mxuanvan02/OmniProxy/releases">
    <img src="https://img.shields.io/github/v/release/mxuanvan02/OmniProxy?display_name=tag&sort=semver" alt="Release">
  </a>
  <a href="https://github.com/mxuanvan02/OmniProxy/stargazers">
    <img src="https://img.shields.io/github/stars/mxuanvan02/OmniProxy" alt="Stars">
  </a>
</div>
<div align="center">
  <p>将 Kiro / Codex 账号转换为 OpenAI &amp; Anthropic 兼容的 API 服务。</p>
</div>
<div align="center">
  <a href="README.md">English</a> | 中文 | <a href="README_VN.md">Tiếng Việt</a>
</div>
<div align="center">
  <p>如果这个项目帮到了你，欢迎点个 Star 支持一下。</p>
</div>


<p align="center">
  <a href="https://github.com/mxuanvan02/OmniProxy">
    <picture>
      <img src="resources/webui.jpg" alt="OmniProxy" style="width: 75%;">
    </picture>
  </a>
</p>

## 功能特性

### 核心 API

- **API 兼容** — Anthropic `/v1/messages`、OpenAI `/v1/chat/completions` 与 `/v1/responses`，支持 SSE 流式输出
- **12 种认证方式** — AWS Builder ID、IAM Identity Center（企业 SSO）、SSO Token、社交登录（Google/GitHub）、Kiro CLI 导入、Kiro SSO 三步浏览器登录、AWS SSO 缓存、Kiro 本地缓存、凭证 JSON、Kiro Web Cookie、API Key (ksk_)、Refresh Token
- **自动刷新令牌** — 凭证持续有效，无需人工干预
- **提示词过滤** — 将 Claude Code CLI 系统提示替换为精简后端提示、去除环境噪音、边界标记；自定义正则规则（管理面板）
- **端点配置** — 自动选择、Kiro、CodeWhisperer、Amazon-Q 端点，支持关闭故障转移
- **每账号出站代理** — 全局或账号级 SOCKS5 / HTTP 代理
- **用量追踪** — 每账号额度、令牌、请求次数、超额告警
- **思考模式** — 可配置触发后缀和输出格式（reasoning_content / thinking / think）
- **Web 管理面板** — 管理账号与设置，三语界面（EN / CN / VN）

### 多账号池

- **轮询负载均衡** — 加权分配
- **端点故障转移** — 自动切换，组合降级链
- **Provider 感知路由** — Claude 模型路由到外部 OpenAI 兼容账号，GPT 模型路由到 Codex 账号
- **按模型冷却** — 配额/认证失败只锁定对应模型，不影响账号其他模型

### Prompt 缓存优化

- **基于 instructions 的缓存键** — 共享同一系统提示的所有对话共用一个缓存条目，跨对话/Agent 共享
- **跨对话缓存共享** — 10 个 Agent 使用同一系统提示 → 1 个缓存条目（而非 10 个）
- **异步缓存预热** — 新轮换账号在后台发送预热请求，首个真实请求即命中缓存
- **预热去重** — 并发请求同一账号+缓存键不会触发重复预热
- **Token 阈值** — 短提示（< 1024 tokens）跳过预热，避免浪费配额
- **缓存粘性路由** — 同一对话的连续请求固定到同一账号，保持缓存热度

### 池路由策略（20+ 账号）

对于配额/重置窗口不均匀的大型池，两种可选策略优于轮询：

- **`cost-optimized`** — 优先选择剩余配额最多的账号（最低 `CodexPrimaryUsedPercent` / 最高 `ExtCreditsRemaining`）。减少中途 429。
- **`reset-aware`** — 避开配额窗口在 30 分钟内重置的账号。在安全账号中回退到 cost-optimized 排序。
- **`round-robin`**（默认） — 零开销，适合配额均匀的小/中型池。

策略仅在池中 ≥ 20 个唯一账号时激活。缓存粘性路由始终优先于策略 — 缓存命中比任何策略选择都节省更多配额。通过管理面板 → Usage → Pool 标签配置，或 `PATCH /admin/api/pool/strategy`。

## 注意

并非所有 IDE、CLI 工具和 Agent 都经过完整测试。只有 Claude Code、OpenCode 和 Codex 经过测试。

## 快速开始

### Docker Compose（推荐）

```bash
git clone https://github.com/mxuanvan02/OmniProxy.git
cd OmniProxy
mkdir -p data
docker-compose up -d
```

### Docker 运行

```bash
docker run -d \
  --name omniproxy \
  -p 8080:8080 \
  -e ADMIN_PASSWORD=your_secure_password \
  -v /path/to/data:/app/data \
  --restart unless-stopped \
  ghcr.io/mxuanvan02/omniproxy:latest
```

### 源码编译

```bash
git clone https://github.com/mxuanvan02/OmniProxy.git
cd OmniProxy
go build -o omniproxy .
./omniproxy
```

### 部署到 Zeabur

仓库已包含 `Dockerfile`，可直接在 Zeabur 上构建运行。

**方式一：面板一键部署**

1. Fork 本仓库到你的 GitHub 账号。
2. 在 Zeabur 新建服务，选择 **Deploy from GitHub**，绑定刚才 fork 的仓库。
3. Zeabur 自动识别 `Dockerfile` 并完成构建。
4. 在 **Networking** 标签暴露端口 `8080` 并绑定域名。
5. 在 **Variables** 标签至少设置 `ADMIN_PASSWORD`（管理面板密码）。
6. 如需持久化账号 / 配置，挂载 Volume 到 `/app/data`。

**方式二：CLI 部署**

```bash
npm i -g zeabur
zeabur auth login
zeabur deploy
```

> 命令需在项目根目录执行。CLI 会生成 `.zeabur/context.json` 记录目标 project / service，包含个人 ID，请勿提交。

部署完成后访问 `https://<你的域名>/admin` 登录管理面板。

首次运行会在 `data/config.json` 自动生成配置，挂载 `/app/data` 以持久化。默认管理密码为 `changeme`，生产环境请务必通过 `ADMIN_PASSWORD` 环境变量或在管理面板中修改。

## 使用方法

访问 `http://localhost:8080/admin` 登录、添加账号，然后调用 API：

```bash
# Claude
curl http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -d '{"model":"claude-sonnet-4.5","max_tokens":1024,"messages":[{"role":"user","content":"你好！"}]}'

# OpenAI / Chat
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"你好！"}]}'

# OpenAI / Responses
curl http://localhost:8080/v1/responses \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any" \
  -d '{"model":"claude-sonnet-4.5","input":"你好！","max_output_tokens":1024}'
```

## 思考模式

在模型名后加后缀（默认 `-thinking`）即可启用，例如 `claude-sonnet-4.5-thinking`。Claude 兼容请求如果带有顶层 `thinking` 配置，例如 `{"type":"enabled","budget_tokens":2048}` 或 `{"type":"adaptive"}`，也会自动启用 thinking 模式。输出格式可在管理面板「设置 - Thinking 模式」中配置。

## 出站代理

可在管理面板「设置 - 出站代理设置」中配置代理。支持 SOCKS5 和 HTTP 代理。

设置保存后即时生效，无需重启服务。

## 环境变量

| 变量 | 说明 | 默认值 |
|-----|------|-------|
| `CONFIG_PATH` | 配置文件路径 | `data/config.json` |
| `ADMIN_PASSWORD` | 管理面板密码（覆盖配置文件） | - |

## 参与贡献

欢迎友好交流。遇到问题时，建议先让 Claude Code、Codex 等工具帮忙排查一下，大部分问题都能自己解决。如果能直接提个 PR 就更好了。

开发设置和指南请参阅 [CONTRIBUTING.md](CONTRIBUTING.md)。

## 更新日志

发布历史和重要变更请参阅 [CHANGELOG.md](CHANGELOG.md)。

## 致谢

OmniProxy 是从 [Kiro-Go](https://github.com/Quorinex/Kiro-Go) fork 出来的，并基于它开发。原项目提供了 Kiro 账号管理、令牌刷新和 OpenAI / Anthropic 兼容 API 层的基础。

OmniProxy 相比上游的主要新增：

- Codex（ChatGPT 订阅）账号支持与用量追踪
- 外部 OpenAI 兼容 Provider 支持
- 基于 instructions 的 Prompt 缓存键，跨对话共享
- 异步缓存预热（去重 + Token 阈值）
- 池路由策略（cost-optimized、reset-aware）适用于 20+ 账号池
- 组合降级链，每个组合可独立配置策略
- Web 管理面板，三语界面（EN / CN / VN）
- 每账号出站代理（SOCKS5 / HTTP）
- 自定义正则规则的提示词过滤系统

## 免责声明

本项目仅供学习和研究目的使用，与 Amazon、AWS 或 Kiro 没有任何关联。用户需自行确保使用行为符合所有适用的服务条款和法律法规，使用风险自负。

## 许可证

[MIT](LICENSE) — Copyright (c) 2026 mxuanvan02
