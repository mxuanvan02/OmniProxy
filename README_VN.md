<p align="center">
  <a href="https://github.com/lenhanpham/SuperKiro">
    <picture>
      <img src="web/icon.svg" alt="SuperKiro" style="width: 25%;">
    </picture>
  </a>
</p>

# SuperKiro
<div align="center">
  <a href="https://go.dev/">
    <img src="https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go" alt="Go Version">
  </a>
  <a href="https://www.docker.com/">
    <img src="https://img.shields.io/badge/Docker-Ready-2496ED?style=flat&logo=docker" alt="Docker">
  </a>
  <a href="LICENSE">
    <img src="https://img.shields.io/badge/License-MIT-green.svg" alt="License">
  </a>
</div>
<div align="center">
  <p>Chuyển đổi tài khoản Kiro thành dịch vụ API tương thích OpenAI / Anthropic.</p>
</div>
<div align="center">
  <a href="README.md">English</a> | <a href="README_CN.md">中文</a> | Tiếng Việt
</div>
<div align="center">
  <p>Nếu dự án này hữu ích với bạn, hãy cho một Star nhé.</p>
</div>


<p align="center">
  <a href="https://github.com/lenhanpham/SuperKiro">
    <picture>
      <img src="resources/webui.jpg" alt="SuperKiro" style="width: 75%;">
    </picture>
  </a>
</p>

## Tính năng

- **Tương thích API** — Anthropic `/v1/messages`, OpenAI `/v1/chat/completions` & `/v1/responses`, SSE streaming
- **Nhóm đa tài khoản** — cân bằng tải round-robin, chuyển đổi dự phòng endpoint, chuỗi combo fallback
- **12 phương thức xác thực** — AWS Builder ID, IAM Identity Center (Enterprise SSO), SSO Token, Social Login (Google/GitHub), Kiro CLI import, Kiro SSO 3 bước đăng nhập qua trình duyệt, AWS SSO Cache, Kiro Local Cache, Credentials JSON, Kiro Web Cookie, API Key (ksk_), Refresh Token
- **Tự động làm mới Token** — thông tin xác thực luôn hợp lệ
- **Lọc prompt** — thay thế system prompt Claude Code CLI bằng phiên bản backend gọn nhẹ, loại bỏ nhiễu môi trường, dấu phân cách; quy tắc regex tùy chỉnh (admin panel)
- **Cấu hình endpoint** — tự động chọn, Kiro, CodeWhisperer hoặc Amazon-Q, tùy chọn tắt fallback
- **Proxy riêng cho từng tài khoản** — SOCKS5 / HTTP toàn cục hoặc cấp tài khoản
- **Theo dõi sử dụng** — tín dụng, token, số lượng request, cảnh báo vượt hạn mức
- **Chế độ Thinking** — cấu hình hậu tố kích hoạt, định dạng đầu ra (reasoning_content / thinking / think)
- **Web admin panel** — quản lý tài khoản, cài đặt, i18n (EN / CN / VN)

## Lưu ý

Không phải tất cả IDE, công cụ CLI và Agent đều được kiểm tra đầy đủ. Chỉ có Claude Code, OpenCode và Codex được kiểm tra.

## Bắt đầu nhanh

### Docker Compose (Khuyến nghị)

```bash
git clone https://github.com/lenhanpham/SuperKiro.git
cd SuperKiro
mkdir -p data
docker-compose up -d
```

### Docker Run

```bash
docker run -d \
  --name superkiro \
  -p 8080:8080 \
  -e ADMIN_PASSWORD=your_secure_password \
  -v /path/to/data:/app/data \
  --restart unless-stopped \
  ghcr.io/lenhanpham/superkiro:latest
```

### Build từ mã nguồn

```bash
git clone https://github.com/lenhanpham/SuperKiro.git
cd SuperKiro
go build -o superkiro .
./superkiro
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

## Ghi nhận

- SuperKiro là dự án fork từ Kiro-Go và được phát triển dựa trên [Kiro-Go](https://github.com/Quorinex/Kiro-Go) 

## Tuyên bố miễn trừ

Chỉ dành cho mục đích giáo dục và nghiên cứu. Không liên kết với Amazon, AWS hay Kiro. Người dùng tự chịu trách nhiệm tuân thủ các điều khoản dịch vụ và pháp luật hiện hành. Sử dụng với rủi ro của riêng bạn.

## Giấy phép

[MIT](LICENSE)
