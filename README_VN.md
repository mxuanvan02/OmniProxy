# OmniProxy

Proxy và bộ định tuyến API cho mô hình ngôn ngữ lớn, viết bằng Go. OmniProxy cung cấp các endpoint tương thích OpenAI và Anthropic cho các công cụ CLI cục bộ, rồi định tuyến yêu cầu tới nhiều nhà cung cấp phía trên (OpenAI Codex, AWS IAM SSO / Builder ID, các cổng tương thích OpenAI, AgentRouter và dịch vụ tìm kiếm web).

OmniProxy được phát triển từ dự án **SuperKiro**, bổ sung danh mục mô hình phân nhóm theo họ, chuyển đổi giao thức AgentRouter hai chiều, xoay vòng nhóm tài khoản với cách ly lỗi theo từng mô hình, và giao diện quản trị cục bộ.

---

[English](README.md) | [Tiếng Việt](README_VN.md) | [中文](README_CN.md)

---

## 1. Tính năng

* **Chuyển đổi giao thức:** Phục vụ `/v1/chat/completions`, `/v1/messages` (Anthropic), `/v1/responses` và `/v1/models`.
* **Các phương thức xác thực:**
  * **OpenAI Codex OAuth** — luồng PKCE qua trình duyệt, tự động làm mới token, theo dõi chu kỳ hạn mức.
  * **AgentRouter** — chuyển đổi định dạng payload tác vụ, ánh xạ sự kiện stream `agent_thought` sang `reasoning_content`, duy trì `X-Agent-Session-ID` giữa các lượt.
  * **Cổng tương thích OpenAI** — bất kỳ endpoint bên ngoài, tự khám phá danh mục mô hình từ `/v1/models`.
  * **AWS IAM SSO / Builder ID** — đăng nhập và làm mới token nền cho CodeWhisperer/Kiro.
  * **Service API key** — tìm kiếm web qua Firecrawl, Tavily, Exa, Jina Reader.
* **Danh mục theo họ mô hình:** Nhóm các mô hình đã khám phá thành các họ (`gpt`, `claude`, `qwen`, `deepseek`, `glm`, `grok`, `llama`, `kimi`, `minimax`) kèm giới hạn context và output token.
* **Nhóm tài khoản:**
  * Chiến lược chọn tài khoản: round-robin có trọng số, tối ưu chi phí, nhận biết chu kỳ reset.
  * Thời gian chờ theo từng mô hình, để một mô hình bị giới hạn tốc độ không loại bỏ toàn bộ tài khoản.
  * Định tuyến cố định theo hội thoại nhằm tái dùng prompt cache phía trên.
* **Giao diện quản trị cục bộ:** Web UI nhúng (EN / VI / ZH), thống kê token và credit theo ngày, quản lý API key, cấu hình chuỗi dự phòng.

---

## 2. Kiến trúc

```
[Client: Claude CLI, Codex CLI, mọi client tương thích OpenAI]
                                │
                                ▼  (/v1/chat/completions, /v1/messages, /v1/models)
┌────────────────────────────────────────────────────────────────────────┐
│                              OmniProxy                                 │
│                                                                        │
│  ┌──────────────────────┐  ┌─────────────────┐  ┌──────────────────┐  │
│  │   Inbound Handler    │  │ Danh mục mô hình│  │ Quản lý API key  │  │
│  │ (OpenAI / Anthropic) │  │  (Nhóm theo họ) │  │    và hạn mức    │  │
│  └──────────┬───────────┘  └────────┬────────┘  └─────────┬────────┘  │
│             │                       │                     │           │
│             ▼                       ▼                     ▼           │
│  ┌──────────────────────────────────────────────────────────────────┐  │
│  │                    Nhóm định tuyến tài khoản                     │  │
│  │  - Round-Robin / Cost-Optimized / Reset-Aware                    │  │
│  │  - Cách ly lỗi và thời gian chờ theo từng mô hình                │  │
│  │  - Định tuyến cố định tận dụng prompt cache                      │  │
│  └──────────────────────────────────┬───────────────────────────────┘  │
│                                     │                                  │
│             ┌───────────────────────┼───────────────────────┐          │
│             ▼                       ▼                       ▼          │
│  ┌─────────────────────┐ ┌─────────────────────┐ ┌──────────────────┐  │
│  │  Codex OAuth Client │ │ Adapter AgentRouter │ │ External Adapter │  │
│  │  (PKCE / SSE Stream)│ │ (Chuyển đổi giao   │ │ (OpenAI / Search)│  │
│  │                     │ │  thức hai chiều)    │ │                  │  │
│  └──────────┬──────────┘ └──────────┬──────────┘ └──────────┬───────┘  │
└─────────────┼───────────────────────┼───────────────────────┼──────────┘
              │                       │                       │
              ▼                       ▼                       ▼
      [OpenAI Codex]            [AgentRouter]       [Nhà cung cấp ngoài]
```

---

## 3. Cài đặt

### Yêu cầu
* Go 1.25+ (hoặc Docker).

### Biên dịch từ mã nguồn
```bash
git clone https://github.com/mxuanvan02/OmniProxy.git
cd OmniProxy
go build -o omniproxy .
./omniproxy
```

Dịch vụ lắng nghe tại `http://127.0.0.1:8080` (hoặc cổng khai báo trong `data/config.json`). Giao diện quản trị ở `http://127.0.0.1:8080/admin`.

### Chạy bằng Docker
```bash
docker compose up -d
```

> Cấu hình mặc định không bắt buộc API key. Hãy đặt `requireApiKey: true` và đổi `password` trước khi mở cổng ra ngoài loopback.

---

## 4. Cấu hình (`data/config.json`)

```json
{
  "host": "127.0.0.1",
  "port": 8080,
  "password": "your-admin-password",
  "requireApiKey": false,
  "accounts": [
    {
      "id": "00000000-0000-0000-0000-000000000001",
      "nickname": "AgentRouter Primary",
      "authMethod": "agentrouter",
      "provider": "AgentRouter",
      "baseUrl": "https://agentrouter.org",
      "accessToken": "sk-ar-...",
      "enabled": true,
      "weight": 1,
      "region": "external"
    },
    {
      "id": "00000000-0000-0000-0000-000000000002",
      "nickname": "External Provider",
      "authMethod": "external_openai",
      "provider": "External OpenAI",
      "baseUrl": "https://api.example.com",
      "accessToken": "sk-...",
      "enabled": true,
      "weight": 1,
      "region": "external"
    }
  ]
}
```

Tài khoản cũng có thể thêm từ giao diện quản trị (`/admin` → Add Account), nơi xử lý sẵn các luồng OAuth và import.

---

## 5. Tích hợp client

### Claude CLI

Trỏ Anthropic base URL về OmniProxy:

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8080
export ANTHROPIC_API_KEY=<your-omniproxy-api-key>   # giá trị bất kỳ khi requireApiKey là false

claude --model claude-opus-5 -p "Hello"
```

Hoặc lưu cố định trong `~/.claude/settings.json`:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:8080"
  }
}
```

### Codex CLI

Khai báo OmniProxy như một provider tương thích OpenAI trong `~/.codex/config.toml`:

```toml
model = "gpt-5.6-sol"
model_provider = "omniproxy"

[model_providers.omniproxy]
name = "OmniProxy"
base_url = "http://127.0.0.1:8080/v1"
env_key = "OMNIPROXY_API_KEY"
```

```bash
export OMNIPROXY_API_KEY=<your-omniproxy-api-key>
codex exec "Hello"
```

### Gọi API trực tiếp

```bash
# Anthropic Messages
curl http://127.0.0.1:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -H "x-api-key: $OMNIPROXY_API_KEY" \
  -d '{"model":"claude-opus-5","max_tokens":128,"messages":[{"role":"user","content":"Hello"}]}'

# OpenAI Chat Completions
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $OMNIPROXY_API_KEY" \
  -d '{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"Hello"}]}'

# Danh sách mô hình khả dụng
curl http://127.0.0.1:8080/v1/models
```

---

## 6. Kiểm thử

```bash
go test -count=1 ./...
```

---

## 7. Giấy phép

Phát hành theo giấy phép [MIT](LICENSE). Phát triển từ dự án SuperKiro.
