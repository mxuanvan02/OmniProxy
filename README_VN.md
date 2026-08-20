# OmniProxy

Proxy trung gian và bộ định tuyến API mô hình ngôn ngữ lớn (LLM Gateway) viết bằng ngôn ngữ Go. Hệ thống đóng vai trò cầu nối giữa các công cụ phát triển phần mềm (Hermes Agent, OpenClaw, K-Dense BYOK, Claude CLI, Cursor, Continue) với các nhà cung cấp mô hình trí tuệ nhân tạo (OpenAI Codex, AWS IAM SSO/Builder ID, các cổng tương thích OpenAI, AgentRouter và các dịch vụ tìm kiếm web).

OmniProxy được kế thừa và mở rộng từ nền tảng mã nguồn mở **SuperKiro**, bổ sung khả năng chuyển đổi giao thức AgentRouter, quản lý danh mục theo họ mô hình, cơ chế xoay vòng tài khoản phân tầng và giao diện quản trị cục bộ.

---

[English](README.md) | [Tiếng Việt](README_VN.md) | [中文](README_CN.md)

---

## 1. Tính năng cốt lõi

* **Chuyển đổi đa giao thức:** Cung cấp các endpoint chuẩn `/v1/chat/completions`, `/v1/messages` (Anthropic), `/v1/responses` và `/v1/models`.
* **Hỗ trợ đa phương thức xác thực:**
  * **OpenAI Codex OAuth:** Quy trình đăng nhập PKCE qua trình duyệt, tự động làm mới token và theo dõi chu kỳ hạn mức.
  * **Chuyển đổi giao thức AgentRouter:** Xử lý cấu trúc payload tác vụ riêng biệt, trích xuất sự kiện `agent_thought` sang luồng streaming `reasoning_content` chuẩn OpenAI và duy trì phiên `X-Agent-Session-ID`.
  * **Cổng tương thích OpenAI bên ngoài:** Kết nối các cổng API bên ngoài với khả năng tự động khám phá danh mục mô hình.
  * **AWS IAM SSO & Builder ID:** Đăng nhập và tự động làm mới token ngầm cho CodeWhisperer/Kiro.
  * **Service API Keys:** Tích hợp tìm kiếm web qua Firecrawl, Tavily, Exa và Jina Reader.
* **Định tuyến theo họ mô hình & Danh mục hợp nhất:** Phân loại hơn 500 định danh mô hình thành 9 họ (`gpt`, `claude`, `qwen`, `deepseek`, `glm`, `grok`, `llama`, `kimi`, `minimax`) với thông số giới hạn context và output chính xác.
* **Quản lý nhóm tài khoản & Dự phòng lỗi:**
  * Thuật toán chọn tài khoản: Round-Robin có trọng số, Tối ưu chi phí (Cost-Optimized) và Nhận biết chu kỳ reset (Reset-Aware).
  * **Khoá lỗi theo từng mô hình (Per-model lock):** Cách ly lỗi ở cấp độ mô hình trên từng tài khoản, không loại bỏ toàn bộ tài khoản khi chỉ có một mô hình bị giới hạn tốc độ.
  * **Bộ nhớ đệm prompt theo phiên (Sticky Prompt Cache):** Duy trì các lượt hội thoại liên tiếp trên cùng một tài khoản để tận dụng prompt caching của nhà cung cấp.
* **Giao diện quản trị & Giám sát cục bộ:** Giao diện web hỗ trợ 3 ngôn ngữ (Tiếng Anh, Tiếng Việt, Tiếng Trung), báo cáo token/credit theo ngày, quản lý khóa API và cấu hình chuỗi dự phòng (combos).

---

## 2. Sơ đồ kiến trúc

```
[Công cụ: Hermes Agent, OpenClaw, K-Dense, Claude CLI, Cursor]
                                │
                                ▼  (/v1/chat/completions, /v1/messages, /v1/models)
┌────────────────────────────────────────────────────────────────────────┐
│                              OmniProxy                                 │
│                                                                        │
│  ┌──────────────────────┐  ┌─────────────────┐  ┌──────────────────┐  │
│  │     Inbound Handler  │  │ Danh mục mô hình│  │ Quản lý khóa API │  │
│  │ (OpenAI / Anthropic) │  │  (Nhóm theo họ) │  │    và hạn mức    │  │
│  └──────────┬───────────┘  └────────┬────────┘  └─────────┬────────┘  │
│             │                       │                     │           │
│             ▼                       ▼                     ▼           │
│  ┌──────────────────────────────────────────────────────────────────┐  │
│  │                      Nhóm định tuyến tài khoản                   │  │
│  │  - Round-Robin / Cost-Optimized / Reset-Aware                    │  │
│  │  - Cách ly lỗi và thời gian chờ theo từng mô hình                │  │
│  │  - Định tuyến phiên cố định tận dụng Prompt Cache                │  │
│  └──────────────────────────────────┬───────────────────────────────┘  │
│                                     │                                  │
│             ┌───────────────────────┼───────────────────────┐          │
│             ▼                       ▼                       ▼          │
│  ┌─────────────────────┐ ┌─────────────────────┐ ┌──────────────────┐  │
│  │  Codex OAuth Client │ │ Adapter AgentRouter │ │  External Adapter│  │
│  │  (PKCE / SSE Stream)│ │ (Chuyển đổi protocol│ │(OpenAI / Search) │  │
│  └──────────┬──────────┘ └──────────┬──────────┘ └──────────┬───────┘  │
└─────────────┼───────────────────────┼───────────────────────┼──────────┘
              │                       │                       │
              ▼                       ▼                       ▼
      [OpenAI Codex]            [AgentRouter]       [Nhà cung cấp ngoài]
```

---

## 3. Cài đặt và vận hành

### Yêu cầu môi trường
* Go phiên bản 1.25 trở lên (hoặc Docker).

### Biên dịch từ mã nguồn
```bash
git clone https://github.com/mxuanvan02/OmniProxy.git
cd OmniProxy
go build -o omniproxy .
./omniproxy
```

Dịch vụ mặc định lắng nghe tại `http://127.0.0.1:8080` (hoặc cổng được định nghĩa trong `data/config.json`). Giao diện quản trị truy cập tại `http://127.0.0.1:8080/web/`.

### Vận hành với Docker
```bash
docker compose up -d
```

### Vận hành trên macOS (launchd)
```bash
cp com.van.omniproxy.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/com.van.omniproxy.plist
```

---

## 4. Mẫu cấu hình (`data/config.json`)

```json
{
  "host": "0.0.0.0",
  "port": 8080,
  "password": "mat-khau-quan-tri",
  "requireApiKey": false,
  "accounts": [
    {
      "id": "6d45de3c-84bc-403d-840b-78deb9bc43b9",
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
      "id": "5f369898-a56b-4072-8cbf-c4166553044e",
      "nickname": "DeepSeek External",
      "authMethod": "external_openai",
      "provider": "External OpenAI",
      "baseUrl": "https://api.deepseek.com",
      "accessToken": "sk-...",
      "enabled": true,
      "weight": 1,
      "region": "external"
    }
  ]
}
```

---

## 5. Tích hợp công cụ

### Hermes Agent / OpenClaw
```yaml
providers:
  omniproxy:
    base_url: "http://127.0.0.1:8080/v1"
    api_key: "***"
    models:
      - gpt-5.6-sol
      - claude-opus-5
      - deepseek-v4-pro
      - qwen3.8-max
```

### K-Dense BYOK
Khai báo trong file `.env`:
```env
OMNIPROXY_BASE_URL=http://127.0.0.1:8080/v1
DEFAULT_MODEL_PROVIDER=omniproxy
```

### Claude CLI (`~/.claude/settings.json`)
```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:8080"
  }
}
```

---

## 6. Kiểm thử

Thực thi toàn bộ bộ kiểm thử tự động:
```bash
go test -count=1 ./...
```

---

## 7. Giấy phép

Phát hành dưới giấy phép mã nguồn mở [MIT](LICENSE). Xây dựng trên nền tảng của dự án SuperKiro.
