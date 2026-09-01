# OmniProxy

Proxy và bộ định tuyến API cho mô hình ngôn ngữ lớn, viết bằng Go. OmniProxy cung cấp các endpoint tương thích OpenAI và Anthropic cho các công cụ CLI cục bộ, rồi định tuyến yêu cầu tới nhiều nhà cung cấp phía trên (OpenAI Codex, Google Antigravity, AWS IAM SSO / Builder ID, các cổng tương thích OpenAI, AgentRouter, dịch vụ tìm kiếm web và API media Gommo AutoAI).

OmniProxy được phát triển từ dự án **SuperKiro**, bổ sung danh mục mô hình phân nhóm theo họ, chuyển đổi giao thức AgentRouter hai chiều, xoay vòng nhóm tài khoản với cách ly lỗi theo từng mô hình, và giao diện quản trị cục bộ.

---

[English](README.md) | [Tiếng Việt](README_VN.md) | [中文](README_CN.md)

---

## 1. Tính năng

* **Chuyển đổi giao thức:** Phục vụ `/v1/chat/completions`, `/v1/messages` (Anthropic), `/v1/responses` và `/v1/models`.
* **Các phương thức xác thực:**
  * **OpenAI Codex OAuth** — luồng PKCE qua trình duyệt, tự động làm mới token, theo dõi chu kỳ hạn mức.
  * **Google Antigravity OAuth** — luồng PKCE qua trình duyệt tới Cloud Code Assist, tự khám phá project theo từng tài khoản, và nhập credential mà Antigravity / Gemini CLI đã cài sẵn ghi ra máy. Đọc lưu ý ở [§7](#7-điều-khoản-dịch-vụ-của-google-antigravity) trước khi dùng.
  * **AgentRouter** — chuyển đổi định dạng payload tác vụ, ánh xạ sự kiện stream `agent_thought` sang `reasoning_content`, duy trì `X-Agent-Session-ID` giữa các lượt.
  * **Cổng tương thích OpenAI** — bất kỳ endpoint bên ngoài, tự khám phá danh mục mô hình từ `/v1/models`.
  * **AWS IAM SSO / Builder ID** — đăng nhập và làm mới token nền cho CodeWhisperer/Kiro.
  * **Service API key** — tìm kiếm web qua Firecrawl, Tavily, Exa, Jina Reader.
  * **Gommo AutoAI** — token dài hạn cho API media sau `api.gommo.net` (cũng là backend của front end 79AI).
* **Sinh media:** Ảnh (`/v1/images/generations`), giọng nói (`/v1/audio/speech`) và video (`/v1/videos/generations`, kèm `/v1/videos/{id}` cho bản render vượt quá thời gian chờ của request) được phục vụ từ những nhà cung cấp có khả năng đó. Các job bất đồng bộ phía trên được poll nội bộ, nên client nhận kết quả hoàn chỉnh thay vì một job id.
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
│        ┌──────────────┬───────────┼───────────┬──────────────┐         │
│        ▼              ▼           ▼           ▼              ▼         │
│  ┌───────────┐ ┌────────────┐ ┌───────────┐ ┌──────────┐ ┌──────────┐  │
│  │   Codex   │ │Antigravity │ │AgentRouter│ │ External │ │  Gommo   │  │
│  │   OAuth   │ │ (dạng      │ │ (Chuyển   │ │ Adapters │ │ (media   │  │
│  │  (PKCE)   │ │  Gemini)   │ │ đổi g.thức)│ │ (OpenAI) │ │ bất đ.bộ)│  │
│  └─────┬─────┘ └─────┬──────┘ └─────┬─────┘ └────┬─────┘ └────┬─────┘  │
└────────┼─────────────┼──────────────┼────────────┼────────────┼────────┘
         │             │              │            │            │
         ▼             ▼              ▼            ▼            ▼
  [OpenAI Codex] [Cloud Code    [AgentRouter]  [Nhà c.cấp  [api.gommo.net]
                   Assist]                       ngoài]
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
    },
    {
      "id": "00000000-0000-0000-0000-000000000003",
      "nickname": "Gommo Media",
      "authMethod": "gommo",
      "provider": "Gommo AutoAI",
      "accessToken": "<gommo-access-token>",
      "gommoDomain": "79ai.net",
      "gommoProjectId": "default",
      "imageModel": "<image-model-id>",
      "gommoTtsModel": "eleven_flash_v2_5",
      "gommoVoiceId": "<voice-id>",
      "capabilities": ["image", "video", "audio-tts"],
      "enabled": true,
      "weight": 1,
      "region": "external"
    }
  ]
}
```

`gommoDomain` là một phần của credential Gommo, không phải tùy chọn: phía trên nhận nó như một field trong body của mọi request và từ chối request thiếu nó. Tài khoản Gommo không được mang capability `chat` — nó sinh media và không trả lời được completion, nên nhóm chat sẽ định tuyến một request mà nó không bao giờ phục vụ được.

Tài khoản Antigravity do luồng đăng nhập/import ghi ra chứ không nên viết tay; `googleProjectId` được khám phá riêng cho từng tài khoản và không được sao chép giữa các tài khoản.

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

### Tạo media

Các route này cần một tài khoản có khai báo capability tương ứng (xem mục Gommo
trong [§4](#4-cấu-hình-dataconfigjson)).

```bash
# Ảnh — trả về một URL cho mỗi ảnh
curl http://127.0.0.1:8080/v1/images/generations \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $OMNIPROXY_API_KEY" \
  -d '{"prompt":"chiếc thuyền giấy trên mặt nước tĩnh","size":"1024x1024","n":1}'

# Giọng nói — trả về dữ liệu âm thanh, giống route của OpenAI
curl http://127.0.0.1:8080/v1/audio/speech \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $OMNIPROXY_API_KEY" \
  -d '{"input":"Xin chào","voice":"<voice-id>"}' \
  --output speech.mp3

# Video — proxy tự poll tiến trình render; `id` dùng để lấy lại bản render chậm
curl http://127.0.0.1:8080/v1/videos/generations \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $OMNIPROXY_API_KEY" \
  -d '{"prompt":"chiếc thuyền giấy trôi theo dòng","ratio":"16_9"}'

curl http://127.0.0.1:8080/v1/videos/<id> \
  -H "Authorization: Bearer $OMNIPROXY_API_KEY"
```

---

## 6. Kiểm thử

```bash
go test -count=1 ./...
```

---

## 7. Điều khoản dịch vụ của Google Antigravity

Điều khoản Antigravity của Google chỉ cho phép truy cập qua client của chính
Google. Một tài khoản dùng qua proxy này có thể bị Google vô hiệu hóa bất cứ lúc
nào, và điều đó đã xảy ra với nhiều người trong thực tế. Hãy coi đó là kết quả
dự kiến khi bật provider này, không phải trường hợp biên.

OAuth client không được đóng gói kèm ở đây. Client ID và secret của bản desktop
Antigravity thuộc về Google, nên luồng đăng nhập đọc chúng từ
`ANTIGRAVITY_CLIENT_ID` và `ANTIGRAVITY_CLIENT_SECRET` (hoặc setting
`antigravityClientId` / `antigravityClientSecret`) và báo lỗi rõ ràng khi không
có. Hãy lấy giá trị từ chính IDE bạn đã cài. Việc nhập credential mà Antigravity
hoặc Gemini CLI đã ghi sẵn ra máy thì không cần cả hai.

OmniProxy xử lý điều đó như sau:

* Gửi đúng những field giao thức mà Cloud Code Assist API yêu cầu — OAuth token,
  descriptor `Client-Metadata`, và project của chính tài khoản đó — không gì thêm.
* Không luân phiên fingerprint client, không tạo giá trị platform hay api-client
  giả, không gửi project id hardcode thuộc về người khác. Vài client bên thứ ba
  làm điều cuối; khi đó request bị tính vào một project mà người gọi không có
  quyền, và đó chính là dấu hiệu mà cơ chế thực thi của Google tìm kiếm.
* Khi Google thực sự vô hiệu hóa một tài khoản, `classifyAntigravityFailure`
  nhận ra phản hồi đó và đánh dấu tài khoản là `BANNED`, để pool ngừng chọn nó
  thay vì thử lại một credential đã chết ở mỗi request.

Không có cấu hình nào làm điều này tuân thủ điều khoản đó. Hãy cân nhắc khi quyết
định dùng; các provider khác trong dự án không bị ảnh hưởng theo cách nào.

---

## 8. Giấy phép

Phát hành theo giấy phép [MIT](LICENSE). Phát triển từ dự án SuperKiro.
