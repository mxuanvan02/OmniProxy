#!/usr/bin/env python3
"""Bulk re-auth Codex accounts via OpenAI device code flow.

Usage:
  python3 codex-bulk-reauth.py --count 5 --output tokens.json
  python3 codex-bulk-reauth.py --count 1  # single account

Each account: request device code → open browser → poll → save tokens.
You sign in with a different ChatGPT account each iteration.
"""
import argparse, json, time, sys, webbrowser, urllib.request, urllib.parse, os
from datetime import datetime, timezone

ISSUER = "https://auth.openai.com"
CLIENT_ID = "app_EMoamEEZ73f0CkXaXp7hrann"
TOKEN_URL = "https://auth.openai.com/oauth/token"
DEVICE_URL = f"{ISSUER}/codex/device"
UA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

def request_device_code():
    """Step 1: Request device code from OpenAI."""
    req = urllib.request.Request(
        f"{ISSUER}/api/accounts/deviceauth/usercode",
        data=json.dumps({"client_id": CLIENT_ID}).encode(),
        headers={"Content-Type": "application/json", "User-Agent": UA, "Accept": "application/json"},
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=15) as resp:
        return json.loads(resp.read())

def poll_for_auth(device_auth_id, user_code, interval=5, max_wait=900):
    """Step 3: Poll for authorization. Returns (authorization_code, code_verifier) or None."""
    start = time.monotonic()
    data = json.dumps({"device_auth_id": device_auth_id, "user_code": user_code}).encode()
    while time.monotonic() - start < max_wait:
        time.sleep(interval)
        req = urllib.request.Request(
            f"{ISSUER}/api/accounts/deviceauth/token",
            data=data,
            headers={"Content-Type": "application/json", "User-Agent": UA, "Accept": "application/json"},
            method="POST",
        )
        try:
            with urllib.request.urlopen(req, timeout=15) as resp:
                return json.loads(resp.read())
        except urllib.error.HTTPError as e:
            if e.code in (403, 404):
                continue  # Not authorized yet
            raise
    return None

def exchange_tokens(authorization_code, code_verifier):
    """Step 4: Exchange authorization code for access/refresh tokens."""
    redirect_uri = f"{ISSUER}/deviceauth/callback"
    data = urllib.parse.urlencode({
        "grant_type": "authorization_code",
        "code": authorization_code,
        "redirect_uri": redirect_uri,
        "client_id": CLIENT_ID,
        "code_verifier": code_verifier,
    }).encode()
    req = urllib.request.Request(
        TOKEN_URL, data=data,
        headers={"Content-Type": "application/x-www-form-urlencoded", "User-Agent": UA, "Accept": "application/json"},
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=15) as resp:
        return json.loads(resp.read())

def extract_account_info(access_token):
    """Extract chatgpt_account_id + plan from JWT."""
    import base64
    try:
        payload = access_token.split('.')[1]
        payload += '=' * (4 - len(payload) % 4)
        data = json.loads(base64.urlsafe_b64decode(payload))
        return {
            "chatgptAccountId": data.get("https://api.openai.com/auth", {}).get("chatgpt_account_id", ""),
            "chatgptPlanType": data.get("https://api.openai.com/auth", {}).get("chatgpt_plan_type", ""),
            "email": data.get("https://api.openai.com/profile", {}).get("email", ""),
        }
    except Exception:
        return {}

def login_one_account(idx, total):
    """Run device code flow for one account. Returns token dict or None."""
    print(f"\n{'='*60}")
    print(f"  Account {idx}/{total}")
    print(f"{'='*60}")

    # Step 1: Request device code
    try:
        dd = request_device_code()
    except Exception as e:
        print(f"  ERROR: Failed to request device code: {e}")
        return None

    user_code = dd.get("user_code", "")
    device_auth_id = dd.get("device_auth_id", "")
    interval = max(3, int(dd.get("interval", "5")))

    if not user_code or not device_auth_id:
        print(f"  ERROR: Incomplete device code response: {dd}")
        return None

    # Step 2: Open browser + show code
    print(f"\n  Open this URL and sign in with a DIFFERENT ChatGPT account:")
    print(f"  \033[94m{DEVICE_URL}\033[0m")
    print(f"\n  Enter code: \033[93m{user_code}\033[0m")
    print(f"\n  Opening browser automatically...")
    try:
        webbrowser.open(DEVICE_URL)
    except Exception:
        pass

    # Auto-copy code to clipboard (macOS)
    try:
        import subprocess
        subprocess.run(["pbcopy"], input=user_code.encode(), check=False, timeout=2)
        print(f"  (Code copied to clipboard)")
    except Exception:
        pass

    print(f"\n  Waiting for sign-in... (timeout 15min, Ctrl+C to skip)")

    # Step 3: Poll
    try:
        code_resp = poll_for_auth(device_auth_id, user_code, interval)
    except KeyboardInterrupt:
        print(f"\n  Skipped.")
        return None

    if not code_resp:
        print(f"  ERROR: Login timed out.")
        return None

    auth_code = code_resp.get("authorization_code", "")
    code_verifier = code_resp.get("code_verifier", "")
    if not auth_code or not code_verifier:
        print(f"  ERROR: Missing authorization_code/code_verifier")
        return None

    # Step 4: Exchange tokens
    try:
        tokens = exchange_tokens(auth_code, code_verifier)
    except Exception as e:
        print(f"  ERROR: Token exchange failed: {e}")
        return None

    access_token = tokens.get("access_token", "")
    refresh_token = tokens.get("refresh_token", "")
    if not access_token:
        print(f"  ERROR: No access_token in response")
        return None

    # Extract account info
    info = extract_account_info(access_token)
    email = info.get("email", f"codex-{device_auth_id[:8]}")

    print(f"  \033[92m✓ Logged in: {email} (plan: {info.get('chatgptPlanType','?')})\033[0m")

    return {
        "email": email,
        "accessToken": access_token,
        "refreshToken": refresh_token,
        "expiresAt": datetime.now(timezone.utc).isoformat(),
        "chatgptAccountId": info.get("chatgptAccountId", ""),
        "chatgptPlanType": info.get("chatgptPlanType", ""),
        "authMethod": "codex",
        "provider": "codex",
        "loginAt": datetime.now(timezone.utc).isoformat(),
    }

def main():
    ap = argparse.ArgumentParser(description="Bulk re-auth Codex accounts")
    ap.add_argument("--count", "-n", type=int, default=1, help="Number of accounts to login")
    ap.add_argument("--output", "-o", default=None, help="Output JSON file (default: ~/.omniroute/codex-tokens.json)")
    ap.add_argument("--import-omniroute", action="store_true", help="Import tokens directly to OmniRoute DB")
    args = ap.parse_args()

    output_file = args.output or os.path.expanduser("~/.omniroute/codex-tokens.json")

    # Load existing tokens
    existing = []
    if os.path.exists(output_file):
        try:
            existing = json.load(open(output_file))
        except Exception:
            existing = []

    print(f"Bulk Codex re-auth: {args.count} account(s)")
    print(f"Output: {output_file}")
    print(f"Existing tokens: {len(existing)}")
    print(f"\nFor each account, sign in with a DIFFERENT ChatGPT account in the browser.")

    new_tokens = []
    for i in range(1, args.count + 1):
        result = login_one_account(i, args.count)
        if result:
            new_tokens.append(result)
            # Save incrementally
            all_tokens = existing + new_tokens
            with open(output_file, "w") as f:
                json.dump(all_tokens, f, indent=2)
            print(f"  Saved ({len(new_tokens)} new, {len(all_tokens)} total)")

        if i < args.count:
            print(f"\n  Next account in 3 seconds... (Ctrl+C to stop)")
            try:
                time.sleep(3)
            except KeyboardInterrupt:
                print("\nStopped.")
                break

    print(f"\n{'='*60}")
    print(f"  Done: {len(new_tokens)} new account(s) logged in")
    print(f"  Total tokens: {len(existing) + len(new_tokens)}")
    print(f"  Saved to: {output_file}")
    print(f"{'='*60}")

    if args.import_omniroute and new_tokens:
        import_to_omniroute(new_tokens)

def import_to_omniroute(tokens):
    """Import tokens to OmniRoute DB."""
    import sqlite3
    db_path = os.path.expanduser("~/.omniroute/storage.sqlite")
    db = sqlite3.connect(db_path)
    now = datetime.now(timezone.utc).isoformat()
    imported = 0
    for t in tokens:
        # Check if exists (by email)
        existing = db.execute("SELECT id FROM provider_connections WHERE name=? AND provider='codex'", (t["email"],)).fetchone()
        if existing:
            # Update
            db.execute(
                "UPDATE provider_connections SET access_token=?, refresh_token=?, expires_at=?, test_status='active', last_error=NULL WHERE id=?",
                (t["accessToken"], t["refreshToken"], t["expiresAt"], existing[0])
            )
        else:
            # Insert
            import uuid
            conn_id = str(uuid.uuid4())
            psd = json.dumps({
                "chatgptAccountId": t.get("chatgptAccountId", ""),
                "chatgptPlanType": t.get("chatgptPlanType", ""),
            })
            db.execute(
                """INSERT INTO provider_connections
                (id, provider, auth_type, name, is_active, access_token, refresh_token, expires_at,
                 test_status, provider_specific_data, created_at, updated_at)
                VALUES (?, 'codex', 'oauth', ?, 1, ?, ?, ?, 'active', ?, ?, ?)""",
                (conn_id, t["email"], t["accessToken"], t["refreshToken"], t["expiresAt"],
                 psd, now, now)
            )
        imported += 1
    db.commit()
    db.close()
    print(f"\nImported {imported} tokens to OmniRoute DB")

if __name__ == "__main__":
    main()
