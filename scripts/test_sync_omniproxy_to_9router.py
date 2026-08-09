#!/usr/bin/env python3
"""Focused tests for the OmniProxy -> 9router Codex sync script."""

from __future__ import annotations

import importlib.util
import sys
import unittest
from pathlib import Path
from unittest.mock import patch


SCRIPT_PATH = Path(__file__).with_name("sync-omniproxy-to-9router.py")
SPEC = importlib.util.spec_from_file_location("sync_omniproxy_to_9router", SCRIPT_PATH)
assert SPEC and SPEC.loader
SYNC = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = SYNC
SPEC.loader.exec_module(SYNC)


def source_account(account_id: str, email: str, enabled: bool) -> dict[str, object]:
    return {
        "authMethod": "codex",
        "accessToken": f"access-{account_id}",
        "refreshToken": f"refresh-{account_id}",
        "chatgptAccountId": account_id,
        "email": email,
        "nickname": email,
        "enabled": enabled,
        "expiresAt": 2_000_000_000,
        "codexPlanType": "plus",
    }


def connection(connection_id: str, account_id: str, email: str, active: bool) -> dict[str, object]:
    return {
        "id": connection_id,
        "provider": "codex",
        "email": email,
        "isActive": active,
        "providerSpecificData": {"chatgptAccountId": account_id},
    }


class SyncTests(unittest.TestCase):
    def test_plan_disables_all_existing_and_activates_enabled_source_accounts(self) -> None:
        source = [
            source_account("source-a", "a@example.test", True),
            source_account("source-b", "b@example.test", False),
        ]
        existing = [
            connection("old-a", "source-a", "a@example.test", False),
            connection("old-orphan", "orphan", "orphan@example.test", True),
        ]

        plan = SYNC.plan_sync(source, existing)

        self.assertEqual([item["id"] for item in plan.existing_to_disable], ["old-orphan"])
        self.assertEqual([item["id"] for item in plan.already_disabled], ["old-a"])
        self.assertEqual([item["chatgptAccountId"] for item in plan.imported_to_activate], ["source-a"])
        self.assertEqual([item["chatgptAccountId"] for item in plan.imported_to_keep_disabled], ["source-b"])

    def test_apply_imports_then_disables_existing_then_enables_enabled_source_accounts(self) -> None:
        source = [
            source_account("source-a", "a@example.test", True),
            source_account("source-b", "b@example.test", False),
        ]
        existing = [
            connection("old-a", "source-a", "a@example.test", True),
            connection("old-b", "b@example.test", "b@example.test", False),
        ]
        calls: list[tuple[str, str, object]] = []

        def fake_request(base_url: str, path: str, method: str = "GET", payload: object = None) -> object:
            calls.append((method, path, payload))
            if path == "/api/oauth/codex/bulk-import":
                return {
                    "success": 2,
                    "failed": 0,
                    "results": [
                        {"index": 0, "ok": True, "id": "staged-a"},
                        {"index": 1, "ok": True, "id": "staged-b"},
                    ],
                }
            return {"connection": {"id": path.rsplit("/", 1)[-1]}}

        with patch.object(SYNC, "request_json", side_effect=fake_request):
            SYNC.apply_sync("http://9router.test", source, existing)

        self.assertEqual(
            [(method, path) for method, path, _ in calls],
            [
                ("POST", "/api/oauth/codex/bulk-import"),
                ("PUT", "/api/providers/old-a"),
                ("PUT", "/api/providers/staged-a"),
            ],
        )
        imported = calls[0][2]
        self.assertIsInstance(imported, dict)
        payloads = imported["accounts"]  # type: ignore[index]
        self.assertEqual([payload["isActive"] for payload in payloads], [False, False])
        self.assertEqual(calls[1][2], {"isActive": False})
        self.assertEqual(calls[-1][2], {"isActive": True})

    def test_apply_does_not_disable_when_import_is_incomplete(self) -> None:
        source = [source_account("source-a", "a@example.test", True)]
        existing = [connection("old-a", "source-a", "a@example.test", True)]
        calls: list[tuple[str, str]] = []

        def fake_request(base_url: str, path: str, method: str = "GET", payload: object = None) -> object:
            calls.append((method, path))
            return {"success": 0, "failed": 1, "results": [{"index": 0, "ok": False}]}

        with patch.object(SYNC, "request_json", side_effect=fake_request):
            with self.assertRaisesRegex(ValueError, "did not import every staged"):
                SYNC.apply_sync("http://9router.test", source, existing)

        self.assertEqual(calls, [("POST", "/api/oauth/codex/bulk-import")])


if __name__ == "__main__":
    unittest.main()
