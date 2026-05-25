"""Unit tests for the CPA-backed account pool.

We monkeypatch the two HTTP methods (_fetch_cpa_file_list and
_download_token_from_cpa) on a fresh AccountService instance so the tests
exercise the diff / cache / lifecycle logic without standing up a real
HTTP server. Live end-to-end against the real CPA is covered by the
container integration check in the project's docker-compose flow.
"""
from __future__ import annotations

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from services.account_service import AccountService, _CPAFile  # noqa: E402


def _mkfile(name: str, modtime: float = 1.0, disabled: bool = False, unavailable: bool = False) -> _CPAFile:
    raw = {
        "name": name,
        "email": name.split("-", 1)[1].rsplit("-", 1)[0] if "-" in name else name,
        "modtime": "",
        "disabled": disabled,
        "unavailable": unavailable,
    }
    f = _CPAFile(raw)
    f.modtime = modtime  # bypass the iso parser for test simplicity
    return f


def _patched_service(files_seq, token_factory):
    """Build an AccountService whose CPA HTTP calls are mocked. `files_seq` is
    an iterable of file lists returned by successive _refresh_file_list_now
    calls (last value sticks). `token_factory` is a callable name → token."""
    svc = AccountService()
    files_iter = iter(files_seq)
    current: list[list] = [[]]  # populated on first fetch

    def fake_fetch():
        try:
            current[0] = next(files_iter)
        except StopIteration:
            pass  # exhausted → sticky last value
        return list(current[0])

    def fake_download(name):
        return token_factory(name)

    svc._fetch_cpa_file_list = fake_fetch  # type: ignore[attr-defined]
    svc._download_token_from_cpa = fake_download  # type: ignore[attr-defined]
    return svc


# ---------------------------------------------------------------------------


def test_initial_refresh_adds_all_accounts():
    files = [_mkfile("codex-a@x-free.json", 1.0), _mkfile("codex-b@y-free.json", 1.0)]
    svc = _patched_service([files], token_factory=lambda n: f"token-{n}")
    svc._refresh_file_list_now()
    assert svc.account_count() == 2


def test_refresh_with_unchanged_modtime_is_noop():
    files = [_mkfile("codex-a-free.json", 1.0)]
    svc = _patched_service([files, files], token_factory=lambda n: "tok")
    svc._refresh_file_list_now()
    svc._refresh_file_list_now()
    assert svc.account_count() == 1


def test_modtime_bump_invalidates_cached_token():
    """When CPA's modtime advances, the cached access_token must be cleared
    so the next pick re-downloads from CPA (covers OAuth refresh path)."""
    initial = [_mkfile("codex-a-free.json", 1.0)]
    bumped = [_mkfile("codex-a-free.json", 2.0)]

    tokens_handed_out = []

    def factory(name):
        tok = f"tok-{len(tokens_handed_out)}"
        tokens_handed_out.append(tok)
        return tok

    svc = _patched_service([initial, bumped], token_factory=factory)
    svc._refresh_file_list_now()
    first = svc.get_available_access_token()
    svc.release_image_slot(first)

    svc._refresh_file_list_now()  # CPA reports new modtime
    second = svc.get_available_access_token()
    svc.release_image_slot(second)

    assert first != second, "token must rotate when modtime bumps"
    assert tokens_handed_out == [first, second]


def test_disabled_account_is_skipped_by_picker():
    files = [
        _mkfile("codex-good-free.json", 1.0),
        _mkfile("codex-dead-free.json", 1.0, disabled=True),
    ]
    svc = _patched_service([files], token_factory=lambda n: f"tok-{n}")
    svc._refresh_file_list_now()
    picked = svc.get_available_access_token()
    assert "good" in picked  # only the non-disabled account is eligible
    svc.release_image_slot(picked)


def test_unavailable_account_is_skipped_too():
    files = [
        _mkfile("codex-good-free.json", 1.0),
        _mkfile("codex-ratelimited-free.json", 1.0, unavailable=True),
    ]
    svc = _patched_service([files], token_factory=lambda n: f"tok-{n}")
    svc._refresh_file_list_now()
    picked = svc.get_available_access_token()
    assert "good" in picked
    svc.release_image_slot(picked)


def test_removed_file_drops_account():
    initial = [_mkfile("codex-a-free.json", 1.0), _mkfile("codex-b-free.json", 1.0)]
    shrunk = [_mkfile("codex-a-free.json", 1.0)]
    svc = _patched_service([initial, shrunk], token_factory=lambda n: "tok")
    svc._refresh_file_list_now()
    assert svc.account_count() == 2
    svc._refresh_file_list_now()
    assert svc.account_count() == 1


def test_token_lazily_downloaded_on_first_pick():
    files = [_mkfile("codex-a-free.json", 1.0)]
    download_calls = []

    def factory(name):
        download_calls.append(name)
        return f"downloaded-{name}"

    svc = _patched_service([files], token_factory=factory)
    svc._refresh_file_list_now()
    assert download_calls == []  # refresh alone must not download tokens

    tok = svc.get_available_access_token()
    assert download_calls == ["codex-a-free.json"]
    assert tok == "downloaded-codex-a-free.json"


def test_remove_invalid_token_clears_cache():
    """remove_invalid_token (called on 401 from ChatGPT) must clear the
    cached token so the NEXT pick re-downloads from CPA."""
    files = [_mkfile("codex-a-free.json", 1.0)]
    factory_counter = {"i": 0}

    def factory(name):
        factory_counter["i"] += 1
        return f"v{factory_counter['i']}"

    svc = _patched_service([files], token_factory=factory)
    svc._refresh_file_list_now()
    tok = svc.get_available_access_token()
    assert tok == "v1"
    svc.release_image_slot(tok)

    assert svc.remove_invalid_token(tok, event="401") is True
    # now pick again — fresh download should happen
    tok2 = svc.get_available_access_token()
    assert tok2 == "v2", "after invalidation, picker must re-download from CPA"
    svc.release_image_slot(tok2)


def test_mark_image_result_decrements_quota():
    files = [_mkfile("codex-a-free.json", 1.0)]
    svc = _patched_service([files], token_factory=lambda n: "tok")
    svc._refresh_file_list_now()
    # Manually seed a quota (real code learns this via ChatGPT refresh)
    acct = list(svc._accounts.values())[0]
    acct.quota = 3
    acct.quota_unknown = False
    acct.access_token = "tok"

    info = svc.mark_image_result("tok", success=True)
    assert info is not None
    assert info["quota"] == 2

    info = svc.mark_image_result("tok", success=False)
    assert info["fail"] == 1
    assert info["quota"] == 2  # failures don't decrement quota
