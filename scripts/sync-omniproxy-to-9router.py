#!/usr/bin/env python3
"""Synchronize Codex accounts from OmniProxy into a running 9router.

OmniProxy is the source of truth for the credentials and enabled state of the
accounts imported by this script. 9router does not expose an API to update OAuth
credentials on an existing connection, so the sync imports fresh connections
through its encrypted bulk-import API. Existing 9router Codex connections are
kept for history but disabled; only the newly imported connections matching
OmniProxy's ``enabled`` flag are activated.

The script never edits 9router's SQLite database directly.
"""

from __future__ import annotations

import argparse
import json
import sys
import urllib.error
import urllib.request
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any


DEFAULT_SOURCE = Path("data/config.json")
DEFAULT_BASE_URL = "http://127.0.0.1:20128"


@dataclass(frozen=True)
class SyncPlan:
    imported: list[dict[str, Any]]
    existing_to_disable: list[dict[str, Any]]
    already_disabled: list[dict[str, Any]]
    imported_to_activate: list[dict[str, Any]]
    imported_to_keep_disabled: list[dict[str, Any]]


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Synchronize OmniProxy Codex accounts into 9router")
    parser.add_argument("--source", type=Path, default=DEFAULT_SOURCE, help=f"OmniProxy config.json (default: {DEFAULT_SOURCE})")
    parser.add_argument("--base-url", default=DEFAULT_BASE_URL, help=f"Running 9router base URL (default: {DEFAULT_BASE_URL})")
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--dry-run", action="store_true", help="Preview only (default)")
    mode.add_argument("--apply", action="store_true", help="Import OmniProxy accounts, disable existing 9router connections, and activate enabled imports")
    return parser.parse_args()


def read_json(path: Path) -> dict[str, Any]:
    try:
        with path.open("r", encoding="utf-8") as file:
            data = json.load(file)
    except FileNotFoundError:
        raise ValueError(f"OmniProxy config not found: {path}") from None
    except json.JSONDecodeError as error:
        raise ValueError(f"invalid JSON in OmniProxy config {path}: {error}") from error
    if not isinstance(data, dict):
        raise ValueError(f"OmniProxy config must contain a JSON object: {path}")
    return data


def is_nonempty_string(value: Any) -> bool:
    return isinstance(value, str) and bool(value.strip())


def account_label(account: dict[str, Any]) -> str:
    return str(account.get("email") or account.get("chatgptAccountId") or "<unnamed>")


def iso8601_from_unix(value: Any) -> str:
    try:
        seconds = int(value)
    except (TypeError, ValueError):
        return ""
    if seconds <= 0:
        return ""
    return datetime.fromtimestamp(seconds, timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z")


def remaining_seconds(value: Any) -> int:
    try:
        return max(0, int(value) - int(datetime.now(timezone.utc).timestamp()))
    except (TypeError, ValueError):
        return 0


def source_codex_accounts(config: dict[str, Any]) -> tuple[list[dict[str, Any]], list[str]]:
    accounts = config.get("accounts", [])
    if not isinstance(accounts, list):
        raise ValueError("OmniProxy config field accounts must be an array")

    valid: list[dict[str, Any]] = []
    skipped: list[str] = []
    seen_ids: set[str] = set()
    for account in accounts:
        if not isinstance(account, dict) or account.get("authMethod") != "codex":
            continue
        email = str(account.get("email") or "<unnamed>")
        account_id = account.get("chatgptAccountId")
        if not is_nonempty_string(account.get("accessToken")):
            skipped.append(f"{email}: missing accessToken")
        elif not is_nonempty_string(account.get("refreshToken")):
            skipped.append(f"{email}: missing refreshToken")
        elif not is_nonempty_string(account_id):
            skipped.append(f"{email}: missing chatgptAccountId")
        elif account_id.strip() in seen_ids:
            skipped.append(f"{email}: duplicate chatgptAccountId {account_id.strip()}")
        else:
            seen_ids.add(account_id.strip())
            valid.append(account)
    return valid, skipped


def request_json(base_url: str, path: str, method: str = "GET", payload: Any = None) -> Any:
    body = None
    headers = {"Accept": "application/json"}
    if payload is not None:
        body = json.dumps(payload).encode("utf-8")
        headers["Content-Type"] = "application/json"
    request = urllib.request.Request(base_url.rstrip("/") + path, data=body, headers=headers, method=method)
    try:
        with urllib.request.urlopen(request, timeout=20) as response:
            return json.load(response)
    except urllib.error.HTTPError as error:
        detail = error.read().decode("utf-8", errors="replace").strip()
        raise ValueError(f"9router API {method} {path} failed with HTTP {error.code}: {detail}") from error
    except urllib.error.URLError as error:
        raise ValueError(f"cannot reach 9router at {base_url}: {error.reason}") from error
    except json.JSONDecodeError as error:
        raise ValueError(f"9router API {method} {path} returned invalid JSON: {error}") from error


def codex_connections(response: Any) -> list[dict[str, Any]]:
    if not isinstance(response, dict) or not isinstance(response.get("connections"), list):
        raise ValueError("9router /api/providers response does not contain a connections array")
    return [item for item in response["connections"] if isinstance(item, dict) and item.get("provider") == "codex"]


def connection_account_id(connection: dict[str, Any]) -> str:
    data = connection.get("providerSpecificData")
    return data["chatgptAccountId"].strip() if isinstance(data, dict) and is_nonempty_string(data.get("chatgptAccountId")) else ""


def plan_sync(source: list[dict[str, Any]], existing: list[dict[str, Any]]) -> SyncPlan:
    existing_to_disable = [item for item in existing if bool(item.get("isActive"))]
    already_disabled = [item for item in existing if not bool(item.get("isActive"))]
    return SyncPlan(
        imported=source,
        existing_to_disable=existing_to_disable,
        already_disabled=already_disabled,
        imported_to_activate=[account for account in source if bool(account.get("enabled"))],
        imported_to_keep_disabled=[account for account in source if not bool(account.get("enabled"))],
    )


def import_payload(account: dict[str, Any]) -> dict[str, Any]:
    email = str(account.get("email") or "").strip()
    return {
        "name": str(account.get("nickname") or email).strip() or email,
        "email": email,
        "isActive": False,
        "accessToken": account["accessToken"],
        "refreshToken": account["refreshToken"],
        "expiresAt": iso8601_from_unix(account.get("expiresAt")),
        "expiresIn": remaining_seconds(account.get("expiresAt")),
        "testStatus": "active",
        "providerSpecificData": {
            "chatgptAccountId": account["chatgptAccountId"].strip(),
            "chatgptPlanType": account.get("codexPlanType", ""),
        },
    }


def imported_connection_ids(result: Any, expected: int) -> list[str]:
    if not isinstance(result, dict) or result.get("success") != expected or result.get("failed") not in (0, None):
        raise ValueError(f"9router did not import every staged account: {result}")
    results = result.get("results")
    if not isinstance(results, list) or len(results) != expected:
        raise ValueError(f"9router bulk-import returned an incomplete result: {result}")
    ids: list[str] = []
    for index, item in enumerate(results):
        if not isinstance(item, dict) or item.get("index") != index or not item.get("ok") or not is_nonempty_string(item.get("id")):
            raise ValueError(f"9router bulk-import failed for staged account {index}: {item}")
        ids.append(item["id"])
    return ids


def print_group(label: str, accounts: list[dict[str, Any]], marker: str) -> None:
    print(f"{label}: {len(accounts)}")
    for account in accounts:
        print(f"  {marker} {account_label(account)}")


def print_plan(source: list[dict[str, Any]], existing: list[dict[str, Any]], plan: SyncPlan, skipped: list[str]) -> None:
    print(f"Source Codex accounts: {len(source)}")
    print(f"Existing Codex connections in 9router: {len(existing)}")
    print_group("Import / refresh", plan.imported, "+")
    print_group("Existing connections to disable", plan.existing_to_disable, "<")
    print_group("Existing connections already disabled", plan.already_disabled, "=")
    print_group("Imported connections to activate", plan.imported_to_activate, ">")
    print_group("Imported connections to keep disabled", plan.imported_to_keep_disabled, "-")
    for reason in skipped:
        print(f"  ! {reason}")


def apply_sync(base_url: str, source: list[dict[str, Any]], existing: list[dict[str, Any]]) -> None:
    print(f"Importing {len(source)} OmniProxy connection(s) as inactive...")
    result = request_json(base_url, "/api/oauth/codex/bulk-import", "POST", {"accounts": [import_payload(item) for item in source]})
    staged_ids = imported_connection_ids(result, len(source))
    existing_to_disable = [connection for connection in existing if bool(connection.get("isActive"))]
    print(f"Imported {len(staged_ids)} connection(s). Disabling {len(existing_to_disable)} active existing connection(s)...")
    for connection in existing_to_disable:
        connection_id = connection.get("id")
        if not is_nonempty_string(connection_id):
            raise ValueError(f"9router returned Codex connection without id: {account_label(connection)}")
        request_json(base_url, f"/api/providers/{connection_id}", "PUT", {"isActive": False})
    enabled = [(connection_id, account) for connection_id, account in zip(staged_ids, source) if bool(account.get("enabled"))]
    print(f"Activating {len(enabled)} imported connection(s) requested by OmniProxy...")
    for connection_id, _account in enabled:
        request_json(base_url, f"/api/providers/{connection_id}", "PUT", {"isActive": True})
    print("Sync applied successfully. Existing 9router connections were retained and disabled.")


def main() -> int:
    args = parse_args()
    try:
        source, skipped = source_codex_accounts(read_json(args.source.expanduser()))
        existing = codex_connections(request_json(args.base_url, "/api/providers"))
        print_plan(source, existing, plan_sync(source, existing), skipped)
        if not args.apply:
            print("Dry run complete. Re-run with --apply to import OmniProxy credentials and disable existing 9router connections.")
            return 0
        if skipped:
            raise ValueError("refusing --apply because OmniProxy contains invalid Codex accounts")
        if not source and existing:
            raise ValueError("refusing --apply with no valid source Codex accounts; resolve source data before disabling existing connections")
        if source:
            apply_sync(args.base_url, source, existing)
        else:
            print("Nothing to synchronize.")
        return 0
    except (OSError, ValueError) as error:
        print(f"ERROR: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
