# ABOUTME: Tests for nexus-ui/api/index.py — the Nexus management portal Lambda
# ABOUTME: Covers routing, GET/PUT /api/orgs/{org}/codex-config, auth guards, and DynamoDB interactions

"""
Tests for nexus-ui/api/index.py
================================

Coverage targets
----------------
* Router dispatch (method + path matching, OPTIONS pre-flight, 404 fallback).
* JWT identity helpers (_get_email, _get_groups, _is_super_admin, _is_org_admin, _is_org_member).
* GET  /api/orgs/{org}/codex-config  — happy path, absent org item, missing fields, auth failure.
* PUT  /api/orgs/{org}/codex-config  — happy path (all fields), partial update, validation errors,
  unknown fields, empty body, auth failure.
* handle_list_orgs  — super-admin full scan, per-user scoped fetch.
* handle_provision_org — success, duplicate, validation errors, non-admin rejection.
"""

from __future__ import annotations

import importlib.util
import json
import os
import sys
from pathlib import Path
from typing import Any
from unittest.mock import MagicMock, patch

import pytest

# ---------------------------------------------------------------------------
# Module loading
# ---------------------------------------------------------------------------

LAMBDA_PATH = Path(__file__).resolve().parents[2] / "nexus-ui" / "api" / "index.py"


def _load_index(env: dict | None = None) -> Any:
    """Load nexus-ui/api/index.py as a fresh module with optional env overrides."""
    os.environ.setdefault("AWS_DEFAULT_REGION", "us-east-1")
    for k, v in (env or {}).items():
        os.environ[k] = v

    module_name = f"nexus_ui_api_index_{id(env)}"
    spec = importlib.util.spec_from_file_location(module_name, LAMBDA_PATH)
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    sys.modules[module_name] = module
    spec.loader.exec_module(module)
    return module


# Load the module once for the majority of tests (they share the same env).
# Tests that need a fresh env can call _load_index() themselves.
index = _load_index({"ORGS_TABLE": "TestNexusOrganizations"})


# ---------------------------------------------------------------------------
# Event-building helpers
# ---------------------------------------------------------------------------

def _event(
    method: str,
    path: str,
    body: dict | None = None,
    groups: list[str] | None = None,
    email: str = "user@example.com",
) -> dict:
    """Build a minimal API Gateway HTTP API event."""
    claims: dict = {"email": email}
    if groups is not None:
        claims["cognito:groups"] = groups
    return {
        "rawPath": path,
        "requestContext": {
            "http": {"method": method.upper()},
            "authorizer": {"jwt": {"claims": claims}},
        },
        "body": json.dumps(body) if body is not None else None,
    }


def _parse(response: dict) -> Any:
    """Decode the JSON body of a Lambda response dict."""
    return json.loads(response["body"])


# ---------------------------------------------------------------------------
# Shared mock helpers
# ---------------------------------------------------------------------------

def _mock_table() -> MagicMock:
    """Return a fresh MagicMock that can stand in for a DynamoDB Table resource."""
    return MagicMock()


# ---------------------------------------------------------------------------
# Identity helpers
# ---------------------------------------------------------------------------

class TestGetGroups:
    def test_cognito_groups_list(self):
        ev = _event("GET", "/", groups=["org-acme", "org-acme-admins"])
        assert set(index._get_groups(ev)) == {"org-acme", "org-acme-admins"}

    def test_cognito_groups_space_separated_string(self):
        """API Gateway sometimes stringifies the list with spaces."""
        ev = _event("GET", "/")
        ev["requestContext"]["authorizer"]["jwt"]["claims"]["cognito:groups"] = (
            "org-acme org-acme-admins"
        )
        groups = index._get_groups(ev)
        assert "org-acme" in groups
        assert "org-acme-admins" in groups

    def test_no_groups_returns_empty_list(self):
        ev = _event("GET", "/")
        assert index._get_groups(ev) == []

    def test_standard_groups_claim_used_as_fallback(self):
        ev = _event("GET", "/")
        ev["requestContext"]["authorizer"]["jwt"]["claims"]["groups"] = ["g1", "g2"]
        assert set(index._get_groups(ev)) == {"g1", "g2"}


class TestIsSuperAdmin:
    def test_nexus_admins_group_grants_super_admin(self):
        ev = _event("GET", "/", groups=["nexus-admins"])
        assert index._is_super_admin(ev) is True

    def test_regular_user_is_not_super_admin(self):
        ev = _event("GET", "/", groups=["org-acme"])
        assert index._is_super_admin(ev) is False

    def test_no_groups_is_not_super_admin(self):
        ev = _event("GET", "/")
        assert index._is_super_admin(ev) is False


class TestIsOrgAdmin:
    def test_org_admin_group_grants_access(self):
        ev = _event("GET", "/", groups=["org-acme-admins"])
        assert index._is_org_admin(ev, "acme") is True

    def test_super_admin_is_org_admin_for_any_org(self):
        ev = _event("GET", "/", groups=["nexus-admins"])
        assert index._is_org_admin(ev, "acme") is True
        assert index._is_org_admin(ev, "other-org") is True

    def test_regular_member_is_not_org_admin(self):
        ev = _event("GET", "/", groups=["org-acme"])
        assert index._is_org_admin(ev, "acme") is False

    def test_admin_of_different_org_is_not_admin_here(self):
        ev = _event("GET", "/", groups=["org-other-admins"])
        assert index._is_org_admin(ev, "acme") is False


class TestIsOrgMember:
    def test_org_member_group(self):
        ev = _event("GET", "/", groups=["org-acme"])
        assert index._is_org_member(ev, "acme") is True

    def test_org_admin_group_also_member(self):
        ev = _event("GET", "/", groups=["org-acme-admins"])
        assert index._is_org_member(ev, "acme") is True

    def test_super_admin_is_member_of_any_org(self):
        ev = _event("GET", "/", groups=["nexus-admins"])
        assert index._is_org_member(ev, "acme") is True

    def test_unrelated_group_is_not_member(self):
        ev = _event("GET", "/", groups=["org-other"])
        assert index._is_org_member(ev, "acme") is False

    def test_no_groups_is_not_member(self):
        ev = _event("GET", "/")
        assert index._is_org_member(ev, "acme") is False


# ---------------------------------------------------------------------------
# Router / dispatch
# ---------------------------------------------------------------------------

class TestDispatch:
    def test_options_returns_204(self):
        ev = _event("OPTIONS", "/api/orgs/acme/codex-config")
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 204

    def test_unknown_route_returns_404(self):
        ev = _event("GET", "/api/unknown-endpoint")
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 404
        assert "No route matched" in _parse(resp)["error"]

    def test_wrong_method_returns_404(self):
        # PATCH is not registered for /api/orgs
        ev = _event("PATCH", "/api/orgs")
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 404

    def test_trailing_slash_stripped(self):
        """Trailing slashes are normalised before matching."""
        table = _mock_table()
        table.scan.return_value = {"Items": []}
        index._orgs_table = table

        ev = _event("GET", "/api/orgs/", groups=["nexus-admins"])
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 200

    def test_cors_headers_present_on_every_response(self):
        table = _mock_table()
        table.scan.return_value = {"Items": []}
        index._orgs_table = table

        ev = _event("GET", "/api/orgs", groups=["nexus-admins"])
        resp = index.lambda_handler(ev, None)
        assert resp["headers"]["Access-Control-Allow-Origin"] == "*"

    def test_raw_path_preferred_over_path(self):
        """rawPath takes priority; path is a fallback."""
        table = _mock_table()
        table.scan.return_value = {"Items": []}
        index._orgs_table = table

        ev = _event("GET", "/api/orgs", groups=["nexus-admins"])
        ev["path"] = "/wrong-path"          # rawPath should win
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 200


# ---------------------------------------------------------------------------
# GET /api/orgs/{org}/codex-config
# ---------------------------------------------------------------------------

class TestGetCodexConfig:
    """Tests for handle_get_codex_config."""

    def _make_event(self, org_id: str = "acme", groups: list[str] | None = None):
        return _event("GET", f"/api/orgs/{org_id}/codex-config", groups=groups)

    # ── happy path ──────────────────────────────────────────────────────────

    def test_returns_all_codex_fields_when_present(self):
        table = _mock_table()
        table.get_item.return_value = {
            "Item": {
                "codex_api_key": "sk-test-key",
                "codex_enabled": True,
                "codex_allowed_models": ["anthropic.claude-3-5-sonnet", "amazon.titan"],
            }
        }
        index._orgs_table = table

        ev = self._make_event(groups=["org-acme"])
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 200
        body = _parse(resp)
        assert body["codex_api_key"] == "sk-test-key"
        assert body["codex_enabled"] is True
        assert body["codex_allowed_models"] == [
            "anthropic.claude-3-5-sonnet",
            "amazon.titan",
        ]

    def test_super_admin_can_read_any_org(self):
        table = _mock_table()
        table.get_item.return_value = {
            "Item": {"codex_api_key": "key", "codex_enabled": False, "codex_allowed_models": []}
        }
        index._orgs_table = table

        ev = self._make_event(org_id="some-other-org", groups=["nexus-admins"])
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 200

    def test_org_admin_can_read_own_org(self):
        table = _mock_table()
        table.get_item.return_value = {"Item": {"codex_enabled": True}}
        index._orgs_table = table

        ev = self._make_event(groups=["org-acme-admins"])
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 200

    # ── graceful nulls / absent fields ──────────────────────────────────────

    def test_absent_org_item_returns_empty_payload(self):
        """When the org DETAILS row does not exist, return null/empty values gracefully."""
        table = _mock_table()
        table.get_item.return_value = {}   # no "Item" key
        index._orgs_table = table

        ev = self._make_event(groups=["org-acme"])
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 200
        body = _parse(resp)
        assert body["codex_api_key"] is None
        assert body["codex_enabled"] is None
        assert body["codex_allowed_models"] == []

    def test_item_with_no_codex_fields_returns_nulls(self):
        """Org item exists but has no codex fields — return null/empty gracefully."""
        table = _mock_table()
        table.get_item.return_value = {
            "Item": {"org_id": "acme", "name": "Acme Corp"}   # no codex fields
        }
        index._orgs_table = table

        ev = self._make_event(groups=["org-acme"])
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 200
        body = _parse(resp)
        assert body["codex_api_key"] is None
        assert body["codex_enabled"] is None
        assert body["codex_allowed_models"] == []

    def test_null_codex_allowed_models_becomes_empty_list(self):
        table = _mock_table()
        table.get_item.return_value = {
            "Item": {"codex_api_key": "k", "codex_enabled": True, "codex_allowed_models": None}
        }
        index._orgs_table = table

        ev = self._make_event(groups=["org-acme"])
        resp = index.lambda_handler(ev, None)
        assert _parse(resp)["codex_allowed_models"] == []

    # ── DynamoDB key used ────────────────────────────────────────────────────

    def test_correct_dynamodb_key_is_used(self):
        table = _mock_table()
        table.get_item.return_value = {"Item": {}}
        index._orgs_table = table

        ev = self._make_event(org_id="my-org", groups=["org-my-org"])
        index.lambda_handler(ev, None)

        call_kwargs = table.get_item.call_args[1]
        assert call_kwargs["Key"] == {"pk": "ORG#my-org", "sk": "DETAILS"}

    # ── auth guard ───────────────────────────────────────────────────────────

    def test_non_member_gets_403(self):
        ev = self._make_event(groups=["org-other-org"])
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 403

    def test_unauthenticated_no_groups_gets_403(self):
        ev = self._make_event(groups=[])
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 403

    # ── DynamoDB error ───────────────────────────────────────────────────────

    def test_dynamodb_error_returns_500(self):
        table = _mock_table()
        table.get_item.side_effect = RuntimeError("DynamoDB unavailable")
        index._orgs_table = table

        ev = self._make_event(groups=["org-acme"])
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 500


# ---------------------------------------------------------------------------
# PUT /api/orgs/{org}/codex-config
# ---------------------------------------------------------------------------

class TestPutCodexConfig:
    """Tests for handle_put_codex_config."""

    def _make_event(
        self,
        org_id: str = "acme",
        body: dict | None = None,
        groups: list[str] | None = None,
    ):
        return _event("PUT", f"/api/orgs/{org_id}/codex-config", body=body, groups=groups)

    def _stub_table(self, returned_attributes: dict | None = None) -> MagicMock:
        """Return a table mock whose update_item echoes the provided attributes."""
        table = _mock_table()
        table.update_item.return_value = {
            "Attributes": returned_attributes or {}
        }
        index._orgs_table = table
        return table

    # ── happy path — all fields ──────────────────────────────────────────────

    def test_all_fields_updated_and_returned(self):
        attrs = {
            "org_id": "acme",
            "codex_api_key": "new-api-key",
            "codex_enabled": True,
            "codex_allowed_models": ["model-a", "model-b"],
        }
        table = self._stub_table(attrs)

        ev = self._make_event(
            body={
                "codex_api_key": "new-api-key",
                "codex_enabled": True,
                "codex_allowed_models": ["model-a", "model-b"],
            },
            groups=["org-acme-admins"],
        )
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 200
        body = _parse(resp)
        assert body["org_id"] == "acme"
        assert body["codex_api_key"] == "new-api-key"
        assert body["codex_enabled"] is True
        assert body["codex_allowed_models"] == ["model-a", "model-b"]

    # ── happy path — partial update ──────────────────────────────────────────

    def test_partial_update_only_api_key(self):
        table = self._stub_table(
            {"org_id": "acme", "codex_api_key": "only-key", "codex_enabled": None, "codex_allowed_models": []}
        )
        ev = self._make_event(body={"codex_api_key": "only-key"}, groups=["org-acme-admins"])
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 200
        body = _parse(resp)
        assert body["codex_api_key"] == "only-key"

    def test_partial_update_only_enabled_flag(self):
        table = self._stub_table({"org_id": "acme", "codex_enabled": False})
        ev = self._make_event(body={"codex_enabled": False}, groups=["org-acme-admins"])
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 200

    def test_partial_update_only_allowed_models(self):
        table = self._stub_table(
            {"org_id": "acme", "codex_allowed_models": ["m1"]}
        )
        ev = self._make_event(
            body={"codex_allowed_models": ["m1"]}, groups=["org-acme-admins"]
        )
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 200

    # ── null/empty values ────────────────────────────────────────────────────

    def test_null_api_key_accepted(self):
        table = self._stub_table({"org_id": "acme", "codex_api_key": None})
        ev = self._make_event(body={"codex_api_key": None}, groups=["org-acme-admins"])
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 200

    def test_null_codex_enabled_accepted(self):
        table = self._stub_table({"org_id": "acme"})
        ev = self._make_event(body={"codex_enabled": None}, groups=["org-acme-admins"])
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 200

    def test_null_allowed_models_becomes_empty_list(self):
        table = self._stub_table({"org_id": "acme", "codex_allowed_models": None})
        ev = self._make_event(body={"codex_allowed_models": None}, groups=["org-acme-admins"])
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 200
        body = _parse(resp)
        assert body["codex_allowed_models"] == []

    # ── DynamoDB UpdateExpression structure ──────────────────────────────────

    def test_update_expression_contains_set_keyword(self):
        table = self._stub_table()
        ev = self._make_event(
            body={"codex_api_key": "k", "codex_enabled": True},
            groups=["org-acme-admins"],
        )
        index.lambda_handler(ev, None)
        call_kwargs = table.update_item.call_args[1]
        assert call_kwargs["UpdateExpression"].startswith("SET ")

    def test_update_expression_includes_all_supplied_fields(self):
        table = self._stub_table()
        ev = self._make_event(
            body={
                "codex_api_key": "k",
                "codex_enabled": False,
                "codex_allowed_models": ["x"],
            },
            groups=["org-acme-admins"],
        )
        index.lambda_handler(ev, None)
        call_kwargs = table.update_item.call_args[1]
        expr_names = call_kwargs["ExpressionAttributeNames"]
        # All three fields must appear in the alias map
        assert "codex_api_key" in expr_names.values()
        assert "codex_enabled" in expr_names.values()
        assert "codex_allowed_models" in expr_names.values()

    def test_update_always_stamps_org_id(self):
        """org_id is always written to make the item self-describing."""
        table = self._stub_table()
        ev = self._make_event(
            body={"codex_enabled": True}, groups=["org-acme-admins"]
        )
        index.lambda_handler(ev, None)
        call_kwargs = table.update_item.call_args[1]
        assert ":org_id" in call_kwargs["ExpressionAttributeValues"]
        assert call_kwargs["ExpressionAttributeValues"][":org_id"] == "acme"

    def test_correct_dynamodb_key_is_used(self):
        table = self._stub_table()
        ev = self._make_event(
            org_id="beta", body={"codex_api_key": "k"}, groups=["org-beta-admins"]
        )
        index.lambda_handler(ev, None)
        call_kwargs = table.update_item.call_args[1]
        assert call_kwargs["Key"] == {"pk": "ORG#beta", "sk": "DETAILS"}

    def test_return_values_all_new_is_requested(self):
        table = self._stub_table()
        ev = self._make_event(body={"codex_api_key": "k"}, groups=["org-acme-admins"])
        index.lambda_handler(ev, None)
        call_kwargs = table.update_item.call_args[1]
        assert call_kwargs["ReturnValues"] == "ALL_NEW"

    # ── auth guard ───────────────────────────────────────────────────────────

    def test_non_admin_member_gets_403(self):
        ev = self._make_event(body={"codex_enabled": True}, groups=["org-acme"])
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 403

    def test_non_member_gets_403(self):
        ev = self._make_event(body={"codex_enabled": True}, groups=["org-other-admins"])
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 403

    def test_super_admin_can_update_any_org(self):
        table = self._stub_table({"org_id": "acme", "codex_enabled": True})
        ev = self._make_event(body={"codex_enabled": True}, groups=["nexus-admins"])
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 200

    # ── input validation ─────────────────────────────────────────────────────

    def test_empty_body_returns_400(self):
        ev = self._make_event(body={}, groups=["org-acme-admins"])
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 400
        assert "at least one" in _parse(resp)["error"].lower()

    def test_no_body_returns_400(self):
        ev = self._make_event(groups=["org-acme-admins"])
        # body=None means event["body"] is None
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 400

    def test_invalid_json_body_returns_400(self):
        ev = self._make_event(groups=["org-acme-admins"])
        ev["body"] = "{not valid json"
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 400
        assert "Invalid JSON" in _parse(resp)["error"]

    def test_codex_api_key_must_be_string_or_null(self):
        ev = self._make_event(body={"codex_api_key": 12345}, groups=["org-acme-admins"])
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 400
        assert "codex_api_key" in _parse(resp)["error"]

    def test_codex_enabled_must_be_bool_or_null(self):
        ev = self._make_event(
            body={"codex_enabled": "yes"}, groups=["org-acme-admins"]
        )
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 400
        assert "codex_enabled" in _parse(resp)["error"]

    def test_codex_allowed_models_must_be_list(self):
        ev = self._make_event(
            body={"codex_allowed_models": "all"}, groups=["org-acme-admins"]
        )
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 400
        assert "codex_allowed_models" in _parse(resp)["error"]

    def test_codex_allowed_models_items_must_be_strings(self):
        ev = self._make_event(
            body={"codex_allowed_models": [1, 2, 3]}, groups=["org-acme-admins"]
        )
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 400
        assert "codex_allowed_models" in _parse(resp)["error"]

    def test_unknown_fields_return_400(self):
        ev = self._make_event(
            body={"codex_enabled": True, "surprise_field": "x"},
            groups=["org-acme-admins"],
        )
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 400
        assert "Unknown field" in _parse(resp)["error"]
        assert "surprise_field" in _parse(resp)["error"]

    # ── DynamoDB error ───────────────────────────────────────────────────────

    def test_dynamodb_error_returns_500(self):
        table = _mock_table()
        table.update_item.side_effect = RuntimeError("Throughput exceeded")
        index._orgs_table = table

        ev = self._make_event(body={"codex_enabled": False}, groups=["org-acme-admins"])
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 500


# ---------------------------------------------------------------------------
# GET /api/orgs
# ---------------------------------------------------------------------------

class TestListOrgs:
    def test_super_admin_gets_all_orgs_via_scan(self):
        table = _mock_table()
        table.scan.return_value = {
            "Items": [
                {"org_id": "alpha", "name": "Alpha"},
                {"org_id": "beta", "name": "Beta"},
            ]
        }
        index._orgs_table = table

        ev = _event("GET", "/api/orgs", groups=["nexus-admins"])
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 200
        body = _parse(resp)
        assert len(body["orgs"]) == 2

    def test_regular_member_sees_only_own_orgs(self):
        table = _mock_table()
        table.get_item.return_value = {"Item": {"org_id": "acme", "name": "Acme"}}
        index._orgs_table = table

        ev = _event("GET", "/api/orgs", groups=["org-acme"])
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 200
        body = _parse(resp)
        assert any(o["org_id"] == "acme" for o in body["orgs"])

    def test_empty_org_list_returns_empty_array(self):
        table = _mock_table()
        table.scan.return_value = {"Items": []}
        index._orgs_table = table

        ev = _event("GET", "/api/orgs", groups=["nexus-admins"])
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 200
        assert _parse(resp)["orgs"] == []


# ---------------------------------------------------------------------------
# POST /api/orgs/provision
# ---------------------------------------------------------------------------

class TestProvisionOrg:
    def test_super_admin_can_provision_org(self):
        table = _mock_table()
        index._orgs_table = table

        ev = _event(
            "POST",
            "/api/orgs/provision",
            body={"org_id": "new-org", "name": "New Org"},
            groups=["nexus-admins"],
        )
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 201
        body = _parse(resp)
        assert body["org_id"] == "new-org"
        assert body["name"] == "New Org"

    def test_non_super_admin_gets_403(self):
        ev = _event(
            "POST",
            "/api/orgs/provision",
            body={"org_id": "new-org", "name": "New Org"},
            groups=["org-acme-admins"],
        )
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 403

    def test_missing_org_id_returns_400(self):
        ev = _event(
            "POST",
            "/api/orgs/provision",
            body={"name": "No ID"},
            groups=["nexus-admins"],
        )
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 400
        assert "org_id" in _parse(resp)["error"]

    def test_missing_name_returns_400(self):
        ev = _event(
            "POST",
            "/api/orgs/provision",
            body={"org_id": "valid-id"},
            groups=["nexus-admins"],
        )
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 400
        assert "name" in _parse(resp)["error"]

    def test_invalid_org_id_format_returns_400(self):
        for bad_id in ["-starts-with-dash", "HAS_UPPERCASE", "has spaces", "a" * 64]:
            ev = _event(
                "POST",
                "/api/orgs/provision",
                body={"org_id": bad_id, "name": "Test"},
                groups=["nexus-admins"],
            )
            resp = index.lambda_handler(ev, None)
            assert resp["statusCode"] == 400, f"Expected 400 for org_id={bad_id!r}"

    def test_valid_org_id_formats_accepted(self):
        for good_id in ["a", "my-org", "org123", "a" * 63]:
            table = _mock_table()
            index._orgs_table = table
            ev = _event(
                "POST",
                "/api/orgs/provision",
                body={"org_id": good_id, "name": "OK"},
                groups=["nexus-admins"],
            )
            resp = index.lambda_handler(ev, None)
            assert resp["statusCode"] == 201, f"Expected 201 for org_id={good_id!r}"

    def test_duplicate_org_returns_409(self):
        """ConditionalCheckFailedException (matched by class name) must yield 409."""
        # Create a mock exception whose class *name* matches botocore's exception,
        # mirroring what the Lambda catches by type(exc).__name__.
        ConditionalCheckFailed = type("ConditionalCheckFailedException", (Exception,), {})

        table = _mock_table()
        table.put_item.side_effect = ConditionalCheckFailed("The conditional request failed")
        index._orgs_table = table

        ev = _event(
            "POST",
            "/api/orgs/provision",
            body={"org_id": "existing-org", "name": "Dup"},
            groups=["nexus-admins"],
        )
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 409
        assert "existing-org" in _parse(resp)["error"]


# ---------------------------------------------------------------------------
# End-to-end: lambda_handler entry point
# ---------------------------------------------------------------------------

class TestLambdaHandler:
    """Smoke tests that go through the public lambda_handler entry point."""

    def test_get_codex_config_e2e(self):
        table = _mock_table()
        table.get_item.return_value = {
            "Item": {
                "codex_api_key": "e2e-key",
                "codex_enabled": True,
                "codex_allowed_models": ["x"],
            }
        }
        index._orgs_table = table

        ev = _event(
            "GET",
            "/api/orgs/e2e-org/codex-config",
            groups=["org-e2e-org"],
        )
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 200
        body = _parse(resp)
        assert body["codex_api_key"] == "e2e-key"

    def test_put_codex_config_e2e(self):
        table = _mock_table()
        table.update_item.return_value = {
            "Attributes": {
                "org_id": "e2e-org",
                "codex_api_key": "updated-key",
                "codex_enabled": False,
                "codex_allowed_models": [],
            }
        }
        index._orgs_table = table

        ev = _event(
            "PUT",
            "/api/orgs/e2e-org/codex-config",
            body={"codex_api_key": "updated-key", "codex_enabled": False},
            groups=["org-e2e-org-admins"],
        )
        resp = index.lambda_handler(ev, None)
        assert resp["statusCode"] == 200
        body = _parse(resp)
        assert body["codex_api_key"] == "updated-key"
        assert body["codex_enabled"] is False

    def test_get_and_put_use_same_org_id_from_path(self):
        """Verify the {org} path parameter is correctly extracted for both methods."""
        table = _mock_table()
        table.get_item.return_value = {"Item": {}}
        index._orgs_table = table

        for method in ("GET", "PUT"):
            table.reset_mock()
            if method == "GET":
                table.get_item.return_value = {"Item": {}}
                ev = _event(
                    "GET",
                    "/api/orgs/path-org/codex-config",
                    groups=["org-path-org"],
                )
            else:
                table.update_item.return_value = {"Attributes": {"org_id": "path-org"}}
                ev = _event(
                    "PUT",
                    "/api/orgs/path-org/codex-config",
                    body={"codex_enabled": True},
                    groups=["org-path-org-admins"],
                )

            index.lambda_handler(ev, None)

            if method == "GET":
                key_used = table.get_item.call_args[1]["Key"]
            else:
                key_used = table.update_item.call_args[1]["Key"]

            assert key_used == {"pk": "ORG#path-org", "sk": "DETAILS"}, (
                f"Wrong key for {method}"
            )
