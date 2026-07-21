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
  <p>Chuyển đổi tài khoản Kiro / Codex thành dịch vụ API tương thích OpenAI &amp; Anthropic.</p>
</div>
<div align="center">
  <a href="README.md">English</a> | <a href="README_CN.md">中文</a> | Tiếng Việt
</div>
<div align="center">
  <p>Nếu dự án này hữu ích với bạn, hãy cho một Star nhé.</p>
</div>


<p align="center">
  <a href="https://github.com/mxuanvan02/OmniProxy">
    <picture>
      <img src="resources/webui.jpg" alt="OmniProxy" style="width: 75%;">
    </picture>
  </a>
</p>

## Tính năng

### API cốt lõi

- **Tương thích API** — Anthropic `/v1/messages`, OpenAI `/v1/chat/completions` & `/v1/responses`, SSE streaming
- **12 phương thức xác thực** — AWS Builder ID, IAM Identity Center (Enterprise SSO), SSO Token, Social Login (Google/GitHub), Kiro CLI import, Kiro SSO 3 bước đăng nhập qua trình duyệt, AWS SSO Cache, Kiro Local Cache, Credentials JSON, Kiro Web Cookie, API Key (ksk_), Refresh Token
- **Tự động làm mới Token** — thông tin xác thực luôn hợp lệ
- **Lọc prompt** — thay thế system prompt Claude Code CLI bằng phiên bản backend gọn nhẹ, loại bỏ nhiễu môi trường, dấu phân cách; quy tắc regex tùy chỉnh (admin panel)
- **Cấu hình endpoint** — tự động chọn, Kiro, CodeWhisperer hoặc Amazon-Q, tùy chọn tắt fallback
- **Proxy riêng cho từng tài khoản** — SOCKS5 / HTTP toàn cục hoặc cấp tài khoản
- **Theo dõi sử dụng** — tín dụng, token, số lượng request, cảnh báo vượt hạn mức
- **Chế độ Thinking** — cấu hình hậu tố kích hoạt, định dạng đầu ra (reasoning_content / thinking / think)
- **Web admin panel** — quản lý tài khoản, cài đặt, i18n (EN / CN / VN)

### Nhóm đa tài khoản

- **Cân bằng tải round-robin** — phân phối có trọng số
- **Chuyển đổi dự phòng endpoint** — tự động khi lỗi, chuỗi combo fallback
- **Routing theo provider** — model Claude route đến tài khoản external OpenAI-compatible, model GPT route đến tài khoản Codex
- **Cooldown theo model** — lỗi quota/auth chỉ khóa model đó, không ảnh hưởng model khác trên cùng tài khoản

### Tối ưu Prompt Cache

- **Cache key dựa trên instructions** — tất cả conversation dùng chung system prompt sẻ chia 1 cache entry, kể cả khác conversation/agent
- **Cross-conversation sharing** — 10 agent cùng system prompt → 1 cache entry (thay vì 10)
- **Cache warming async** — tài khoản mới được xoay sẽ nhận warmup request nền, request đầu tiên đã hit cache
- **Warming dedup** — concurrent request cùng account+cacheKey không trigger warmup trùng
- **Token threshold** — prompt ngắn (< 1024 tokens) bỏ qua warming để tiết kiệm quota
- **Cache-sticky pinning** — request liên tiếp cùng conversation pin vào cùng account, giữ cache nóng

### Chiến lược routing (20+ tài khoản)

Cho pool lớn với quota/reset window không đều, 2 chiến lược tùy chọn tốt hơn round-robin:

- **`cost-optimized`** — ưu tiên tài khoản còn nhiều quota nhất (CodexPrimaryUsedPercent thấp nhất / ExtCreditsRemaining cao nhất). Giảm 429 mid-stream.
- **`reset-aware`** — tránh tài khoản có quota window reset trong 30 phút. Fallback cost-optimized ranking trong số tài khoản an toàn.
- **`round-robin`** (mặc định) — zero overhead, phù hợp pool nhỏ/vừa quota đều.

Chiến lược chỉ kích hoạt khi pool ≥ 20 tài khoản. Cache-sticky pinning luôn ưu tiên hơn chiến lược — cache hit tiết kiệm quota hơn bất kỳ choice nào. Cấu hình qua admin panel → Usage → tab Pool, hoặc `PATCH /admin/api/pool/strategy`.

## Lưu ý

Không phải tất cả IDE, công cụ CLI và Agent đều được kiểm tra đầy đủ. Chỉ có Claude Code, OpenCode và Codex được kiểm tra.

## Bắt đầu nhanh

### Docker Compose (Khuyến nghị)

```bash
git clone https://github.com/mxuanvan02/OmniProxy.git
cd OmniProxy
mkdir -p data
docker-compose up -d
```

### Docker Run

```bash
docker run -d \
  --name omniproxy \
  -p 8080:8080 \
  -e ADMIN_PASSWORD=your_secure_password \
  -v /path/to/data:/app/data \
  --restart unless-stopped \
  ghcr.io/mxuanvan02/omniproxy:latest
```

### Build từ mã nguồn

```bash
git clone https://github.com/mxuanvan02/OmniProxy.git
cd OmniProxy
go build -o omniproxy .
./omniproxy
```

### Triển khai trên Zeabur

Kho chứa đã bao gồm `Dockerfile`, có thể build và chạy trực tiếp trên Zeabur.

**Cách 1: Dashboard (một cú nhấp chuột)**

1. Fork kho này về tài khoản GitHub của bạn.
2. Trên Zeabur, tạo service mới, chọn **Deploy from GitHub** và chọn fork của bạn.
3. Zeabur tự động nhận diện `Dockerfile` và build image.
4. Trong tab **Networking**, expose port `8080` và gắn domain.
5. Trong tab **Variables**, đặt ít nhất `ADMIN_PASSWORD` (mật khẩu admin).
6. Gắn Volume tại `/app/data` nếu muốn dữ liệu tài khoản / cấu hình tồn tại qua các lần redeploy.

**Cách 2: CLI**

```bash
npm i -g zeabur
zeabur auth login
zeabur deploy
```

> Chạy lệnh từ thư mục gốc của dự án. CLI ghi `.zeabur/context.json` để ghi nhớ project/service mục tiêu — file chứa ID cá nhân, đừng commit.

Sau khi service hoạt động, mở `https://<domain-của-bạn>/admin` để đăng nhập.

Cấu hình được tự động tạo tại `data/config.json`. Gắn `/app/data` để dữ liệu bền vững. Mật khẩu admin mặc định là `changeme` — hãy thay đổi qua biến môi trường `ADMIN_PASSWORD` hoặc trong admin panel trước khi đưa lên production.

## Cách dùng

Mở `http://localhost:8080/admin`, đăng nhập, thêm tài khoản, sau đó gọi API:

```bash
# Claude
curl http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -d '{"model":"claude-sonnet-4.5","max_tokens":1024,"messages":[{"role":"user","content":"Xin chào!"}]}'

# OpenAI / Chat
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"Xin chào!"}]}'

# OpenAI / Responses
curl http://localhost:8080/v1/responses \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any" \
  -d '{"model":"claude-sonnet-4.5","input":"Xin chào!","max_output_tokens":1024}'
```

## Chế độ Thinking

Thêm hậu tố (mặc định `-thinking`) vào tên model, ví dụ `claude-sonnet-4.5-thinking`. Các request tương thích Claude có cấu hình `thinking` ở cấp cao nhất như `{"type":"enabled","budget_tokens":2048}` hoặc `{"type":"adaptive"}` cũng tự động bật chế độ thinking. Cấu hình định dạng đầu ra trong admin panel tại Cài đặt - Thinking Mode.

## Proxy ra ngoài

Với người dùng trong khu vực mạng bị hạn chế, cấu hình proxy ra ngoài trong admin panel tại **Cài đặt - Cài đặt Proxy ra ngoài**. Hỗ trợ SOCKS5 và HTTP proxy.

Cài đặt có hiệu lực ngay lập tức, không cần khởi động lại.

## Biến môi trường

| Biến | Mô tả | Mặc định |
|------|-------|---------|
| `CONFIG_PATH` | Đường dẫn file cấu hình | `data/config.json` |
| `ADMIN_PASSWORD` | Mật khẩu admin panel (ghi đè cấu hình) | - |

## Đóng góp

Chào đón thảo luận thân thiện. Nếu gặp vấn đề, hãy thử hỏi Claude Code, Codex hoặc các công cụ tương tự trước — hầu hết vấn đề đều tự giải quyết được. Pull Request còn tuyệt hơn.

Xem [CONTRIBUTING.md](CONTRIBUTING.md) cho hướng dẫn setup development.

## Changelog

Xem [CHANGELOG.md](CHANGELOG.md) cho lịch sử release và thay đổi đáng chú ý.

## Ghi nhận

OmniProxy là dự án fork từ [Kiro-Go](https://github.com/Quorinex/Kiro-Go) và được phát triển dựa trên nó. Dự án gốc cung cấp nền tảng quản lý tài khoản Kiro, làm mới token và lớp API tương thích OpenAI / Anthropic.

Những bổ sung chính trong OmniProxy so với upstream:

- Hỗ trợ tài khoản Codex (ChatGPT subscription) với theo dõi usage
- Hỗ trợ external OpenAI-compatible provider
- Cache key dựa trên instructions, sẻ chia cross-conversation
- Cache warming async (dedup + token threshold)
- Pool routing strategies (cost-optimized, reset-aware) cho pool 20+ tài khoản
- Combo fallback chains, mỗi combo cấu hình strategy riêng
- Web admin panel với i18n (EN / CN / VN)
- Proxy riêng cho từng tài khoản (SOCKS5 / HTTP)
- Hệ thống prompt filter với regex rules tùy chỉnh

## Tuyên bố miễn trừ

Chỉ dành cho mục đích giáo dục và nghiên cứu. Không liên kết với Amazon, AWS hay Kiro. Người dùng tự chịu trách nhiệm tuân thủ các điều khoản dịch vụ và pháp luật hiện hành. Sử dụng với rủi ro của riêng bạn.

## Giấy phép

[MIT](LICENSE) — Copyright (c) 2026 mxuanvan02
