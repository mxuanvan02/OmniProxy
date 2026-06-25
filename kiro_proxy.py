#!/usr/bin/env python3
"""Minimal proxy: OpenAI-compatible API -> kiro-cli with KIRO_API_KEY."""
import json, subprocess, sys, os
from http.server import HTTPServer, BaseHTTPRequestHandler

KIRO_API_KEY = os.environ.get("KIRO_API_KEY", "REDACTED_KIRO_API_KEY")
PORT = int(os.environ.get("PROXY_PORT", "8081"))

class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        if "/chat/completions" in self.path:
            self._handle_chat()
        elif "/messages" in self.path:
            self._handle_claude()
        else:
            self.send_error(404)

    def do_GET(self):
        if "/models" in self.path:
            self._json_response({"data": [{"id": "claude-sonnet-4-20250514"}, {"id": "claude-opus-4-20250514"}]})
        elif "/health" in self.path or self.path == "/":
            self._json_response({"status": "ok"})
        else:
            self.send_error(404)

    def _handle_chat(self):
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))))
        messages = body.get("messages", [])
        # Build prompt from last user message
        prompt = ""
        for m in messages:
            if m["role"] == "user":
                prompt = m["content"] if isinstance(m["content"], str) else str(m["content"])
        if not prompt:
            self._json_response({"error": "no user message"}, 400)
            return
        # Call kiro-cli
        result = self._call_kiro(prompt)
        if result is None:
            self._json_response({"error": {"message": "kiro-cli failed"}}, 500)
            return
        self._json_response({
            "id": "chatcmpl-kiro",
            "object": "chat.completion",
            "model": body.get("model", "claude-sonnet-4-20250514"),
            "choices": [{"index": 0, "message": {"role": "assistant", "content": result}, "finish_reason": "stop"}],
            "usage": {"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
        })

    def _handle_claude(self):
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))))
        messages = body.get("messages", [])
        prompt = ""
        for m in messages:
            if m["role"] == "user":
                prompt = m["content"] if isinstance(m["content"], str) else str(m["content"])
        if not prompt:
            self._json_response({"error": "no user message"}, 400)
            return
        result = self._call_kiro(prompt)
        if result is None:
            self._json_response({"type": "error", "error": {"type": "api_error", "message": "kiro-cli failed"}}, 500)
            return
        self._json_response({
            "id": "msg-kiro",
            "type": "message",
            "role": "assistant",
            "model": body.get("model", "claude-sonnet-4-20250514"),
            "content": [{"type": "text", "text": result}],
            "stop_reason": "end_turn",
            "usage": {"input_tokens": 0, "output_tokens": 0}
        })

    def _call_kiro(self, prompt):
        try:
            env = os.environ.copy()
            env["KIRO_API_KEY"] = KIRO_API_KEY
            env["HOME"] = "/tmp"
            result = subprocess.run(
                ["kiro-cli", "chat", "--no-interactive", prompt],
                capture_output=True, text=True, timeout=120, env=env
            )
            # Strip ANSI codes
            import re
            clean = re.sub(r'\x1b\[[0-9;]*m|\x1b\[\?[0-9]*[hl]|\x1b\[[0-9]*G', '', result.stdout)
            # Extract content after "> "
            lines = [l for l in clean.split('\n') if l.strip() and not l.strip().startswith('▸')]
            text = '\n'.join(lines).strip()
            if text.startswith('> '):
                text = text[2:]
            return text if text else None
        except Exception as e:
            print(f"Error: {e}", file=sys.stderr)
            return None

    def _json_response(self, data, code=200):
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps(data).encode())

    def log_message(self, format, *args):
        print(f"[kiro-proxy] {args[0]}", file=sys.stderr)

if __name__ == "__main__":
    print(f"Kiro API Proxy starting on port {PORT}...")
    HTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
