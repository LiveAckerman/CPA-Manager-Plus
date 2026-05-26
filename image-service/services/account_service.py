"""CPA-backed account pool.

This is the heart of why we forked: chatgpt2api originally maintained its own
accounts.json on disk, synced periodically from CPA. That meant two copies of
the same data, drifting between syncs, and silent staleness after CPA's
background OAuth refresh rotated an access_token.

This rewrite makes CPA the single source of truth:

  * The pool of *which* accounts exist comes from CPA's
    /v0/management/auth-files endpoint. We never persist this locally; we
    cache the list in memory and use CPA's per-file `modtime` to avoid
    re-fetching unchanged files.

  * For each account, we lazily download the current access_token from
    CPA's /v0/management/auth-files/download endpoint on first use, then
    cache that token + its ChatGPT-side runtime state (image_gen quota
    remaining, last-used timestamp, success/fail counters) in memory.

  * When ChatGPT returns 401 on a token (CPA's silent refresh wrote a new
    one), we mark the token stale and re-download from CPA on the next
    pick. No external sync job required — every pool change converges
    within one image-gen attempt.

  * Account state (quota / last_used / success / fail) is process-local and
    rebuilt from ChatGPT on startup; nothing is persisted. A restart costs
    one ChatGPT round-trip per active account to re-warm. That's
    acceptable for a daemon that restarts on the order of weeks.

What this module deliberately does NOT do:

  * No local accounts.json. CPA is canonical.
  * No CPA-sync background job. The CPA list is pulled on demand with a
    short TTL (config.cpa_list_cache_secs, default 15s) plus a periodic
    refresh of in-flight token state every config.refresh_account_interval_minute.
  * No multi-pool support. We use the single CPA configured via
    CPA_BASE_URL + CPA_MANAGEMENT_KEY env vars (the same ones the Go
    usage-service uses).
"""
from __future__ import annotations

import threading
import time
from dataclasses import dataclass, field
from typing import Any, Iterable

from curl_cffi import requests

from services.config import config
from utils.log import logger


# ---------------------------------------------------------------------------
# Per-account runtime state held in memory. Mirrors the fields the original
# chatgpt2api stored in accounts.json, minus the fields we don't care about
# (recent_requests, limits_progress fully — we keep just image_gen remaining).
# ---------------------------------------------------------------------------


@dataclass
class _AccountState:
    file_name: str                 # CPA file name (e.g. "codex-x@y.com-free.json")
    email: str                     # extracted from CPA's file metadata
    access_token: str = ""         # the JWT — lazily downloaded from CPA
    cpa_modtime: float = 0.0       # CPA's last-modified UNIX timestamp for this file
    quota: int = 0                 # image_gen remaining (refreshed via ChatGPT)
    quota_unknown: bool = True     # True until we've successfully called ChatGPT once
    last_used_at: float = 0.0      # for round-robin / least-recently-used picking
    success: int = 0
    fail: int = 0
    inflight: int = 0              # concurrent image-gen calls in progress
    in_use: bool = False           # short-term lock — only one picker at a time per account
    status: str = "fresh"          # fresh | active | invalid


class _CPAFile:
    """Light projection of one row out of /v0/management/auth-files."""

    __slots__ = ("name", "email", "modtime", "disabled", "unavailable")

    def __init__(self, raw: dict[str, Any]) -> None:
        self.name: str = str(raw.get("name") or raw.get("id") or "").strip()
        self.email: str = str(raw.get("email") or raw.get("account") or "").strip()
        modtime_str = str(raw.get("modtime") or "").strip()
        self.modtime: float = _parse_iso(modtime_str)
        self.disabled: bool = bool(raw.get("disabled"))
        self.unavailable: bool = bool(raw.get("unavailable"))


def _parse_iso(value: str) -> float:
    """CPA emits Go-formatted RFC3339 timestamps with sub-microsecond precision
    and offsets like +08:00. Python's datetime.fromisoformat handles both
    since 3.11."""
    if not value:
        return 0.0
    try:
        # strip nanosecond fraction if present (datetime supports microseconds only)
        if "." in value:
            head, _, tail = value.partition(".")
            # tail looks like "571509766+08:00"; keep first 6 digits of frac
            digits = ""
            i = 0
            while i < len(tail) and tail[i].isdigit():
                digits += tail[i]
                i += 1
            digits = (digits + "000000")[:6]
            value = f"{head}.{digits}{tail[i:]}"
        from datetime import datetime
        return datetime.fromisoformat(value).timestamp()
    except Exception:
        return 0.0


# ---------------------------------------------------------------------------
# The service singleton. Keeps state under a single mutex; image gen calls
# acquire briefly to pick an account, then release while making the slow
# ChatGPT round-trip.
# ---------------------------------------------------------------------------


class AccountService:
    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._cond = threading.Condition(self._lock)
        # Keyed by CPA filename so we can update via the file list cleanly.
        self._accounts: dict[str, _AccountState] = {}
        # Modtime-cached file list from CPA. Refreshed when stale or on 401.
        self._cpa_files_ts: float = 0.0
        # http client; long-lived so it can keep CPA-side keepalive sockets.
        self._cpa_session = requests.Session()
        self._stop_event = threading.Event()
        self._refresh_thread: threading.Thread | None = None

    # ----- public API consumed by openai_backend_api + conversation -----

    def get_account(self, access_token: str) -> dict[str, Any] | None:
        """Used by openai_backend_api._build_fp() to look up an account's
        cached fingerprint. We don't persist fingerprints, so return a
        minimal dict that lets _build_fp generate fresh defaults."""
        with self._lock:
            for acct in self._accounts.values():
                if acct.access_token == access_token:
                    return {"access_token": access_token, "email": acct.email}
        return {"access_token": access_token} if access_token else None

    def get_available_access_token(self) -> str:
        """Pick the best access_token from the pool. Strategy:
          1. Refresh CPA file list if cache is stale.
          2. Among accounts that are non-disabled and have quota>0 (or unknown),
             pick the one with the smallest last_used_at + smallest inflight.
          3. Lazily download a token from CPA if we have a file slot but no
             cached token for it.
          4. If no account qualifies, raise — caller surfaces as 429.
        """
        self._refresh_file_list_if_stale()

        with self._cond:
            while True:
                candidate = self._pick_locked()
                if candidate is not None:
                    candidate.in_use = True
                    candidate.inflight += 1
                    candidate.last_used_at = time.time()
                    return self._ensure_token_locked(candidate)
                # No available account right now. Wait briefly for one to be
                # released (could be inflight slots clearing).
                if not self._cond.wait(timeout=5.0):
                    break

        raise RuntimeError("no available image quota")

    def release_image_slot(self, access_token: str) -> None:
        with self._cond:
            acct = self._find_by_token_locked(access_token)
            if acct is None:
                return
            acct.in_use = False
            if acct.inflight > 0:
                acct.inflight -= 1
            self._cond.notify_all()

    def mark_image_result(self, access_token: str, success: bool) -> dict[str, Any] | None:
        """Bookkeeping after one image-gen attempt. Returns the post-update
        per-account state dict, mostly for diagnostics."""
        with self._lock:
            acct = self._find_by_token_locked(access_token)
            if acct is None:
                return None
            if success:
                acct.success += 1
                if acct.quota > 0:
                    acct.quota -= 1
            else:
                acct.fail += 1
            return self._to_dict_locked(acct)

    def remove_invalid_token(self, access_token: str, event: str) -> bool:
        """Called when ChatGPT 401s. Most likely cause: CPA refreshed the
        OAuth flow and our cached access_token is stale. Drop the cached
        token (NOT the account itself); next pick will re-download from CPA."""
        with self._cond:
            acct = self._find_by_token_locked(access_token)
            if acct is None:
                return False
            logger.warning(
                "account %s invalidated (event=%s), will redownload from CPA",
                acct.email or acct.file_name, event,
            )
            acct.access_token = ""
            acct.status = "invalid"
            acct.in_use = False
            if acct.inflight > 0:
                acct.inflight -= 1
            self._cond.notify_all()
            # Force a fresh CPA file-list pull on next pick to pick up any rotation.
            self._cpa_files_ts = 0.0
            return True

    # ----- background refresh -----

    def start_background_refresh(self) -> None:
        """Spawned from FastAPI's lifespan. Periodically re-pulls the CPA
        file list so we discover new / disabled / refreshed accounts even
        without traffic."""
        if self._refresh_thread is not None:
            return
        self._stop_event.clear()
        self._refresh_thread = threading.Thread(
            target=self._background_loop, name="cpa-pool-refresh", daemon=True,
        )
        self._refresh_thread.start()

    def stop_background_refresh(self) -> None:
        self._stop_event.set()
        thread = self._refresh_thread
        self._refresh_thread = None
        if thread is not None:
            thread.join(timeout=2.0)

    def _background_loop(self) -> None:
        interval = max(30, config.refresh_account_interval_minute * 60)
        # Do an immediate first pull so /api/accounts shows non-empty
        # within a second of startup.
        try:
            self._refresh_file_list_now()
        except Exception as exc:
            logger.warning("initial CPA refresh failed: %s", exc)
        while not self._stop_event.wait(interval):
            try:
                self._refresh_file_list_now()
            except Exception as exc:
                logger.warning("periodic CPA refresh failed: %s", exc)

    # ----- diagnostics -----

    def account_count(self) -> int:
        with self._lock:
            return len(self._accounts)

    def list_accounts_redacted(self) -> list[dict[str, Any]]:
        with self._lock:
            return [self._to_dict_redacted_locked(a) for a in self._accounts.values()]

    def list_limited_tokens(self) -> list[str]:
        """Compat with vendor's start_limited_account_watcher. We no longer
        run that watcher (refresh is unified), but tests may call this."""
        with self._lock:
            return [a.access_token for a in self._accounts.values() if a.quota == 0 and a.access_token]

    def refresh_accounts(self, tokens: Iterable[str]) -> dict[str, Any]:
        """Compat shim for callers that supply an explicit token list."""
        return self.refresh_quotas(tokens=list(tokens), include_uncached=False)

    def refresh_quotas(
        self,
        tokens: list[str] | None = None,
        include_uncached: bool = True,
    ) -> dict[str, Any]:
        """Force a quota refresh against ChatGPT for each pooled account.

        Mirrors the vendor chatgpt2api refresh path (parallel /backend-api/me
        calls with bounded concurrency) so the diagnostic panel can show real
        `image_gen.remaining` numbers instead of perpetual "unknown".

        Safety: this method is read-only with respect to refresh_token. It
        calls ChatGPT's `/backend-api/me` (an info endpoint that issues no
        new tokens) and optionally CPA's `/v0/management/auth-files/download`
        (which returns the access_token CPA already holds — it does NOT ask
        CPA to rotate anything). The OAuth refresh_token grant endpoint
        (`auth.openai.com/oauth/token`) is never called from this path.

        Parameters
        ----------
        tokens : list[str] | None
            If provided, refresh only the accounts whose currently-cached
            access_token is in this list. If None, refresh every account.
        include_uncached : bool
            When True and `tokens` is None, "fresh" accounts (no cached
            access_token yet) are eagerly downloaded from CPA before being
            refreshed. Default True so a single button click populates the
            whole pool. Pass False for the cheap "refresh what we have"
            mode used by the legacy compat shim.

        Concurrency is capped at 10 workers — same ceiling as the vendor
        used. /backend-api/me calls take ~0.5-2s each; 130 accounts at 10
        concurrent finishes in roughly 10-30s.

        Returns a summary dict with success / invalid / error / skipped
        counts plus the total pool size, suitable for direct JSON return.
        """
        from concurrent.futures import ThreadPoolExecutor, as_completed
        from services.openai_backend_api import (
            InvalidAccessTokenError,
            OpenAIBackendAPI,
        )

        with self._lock:
            all_accts = list(self._accounts.values())

        if tokens is not None:
            wanted = {t for t in tokens if t}
            targets = [a for a in all_accts if a.access_token and a.access_token in wanted]
            uncached: list[_AccountState] = []
        else:
            targets = [a for a in all_accts if a.access_token]
            uncached = [a for a in all_accts if not a.access_token] if include_uncached else []

        # Phase 1: download access_tokens for "fresh" accounts so we have
        # something to call /me with. Bounded by the same worker pool — CPA
        # is local-network and quick, but no need to hammer it either.
        downloaded = 0
        download_failed = 0
        if uncached:
            def _ensure_token(acct: _AccountState) -> _AccountState | None:
                try:
                    token = self._download_token_from_cpa(acct.file_name)
                except Exception as exc:
                    logger.warning("refresh: CPA download failed for %s: %s", acct.email, exc)
                    return None
                with self._lock:
                    acct.access_token = token
                return acct

            with ThreadPoolExecutor(max_workers=min(10, len(uncached))) as ex:
                for fut in as_completed({ex.submit(_ensure_token, a): a for a in uncached}):
                    result = fut.result()
                    if result is not None:
                        downloaded += 1
                        targets.append(result)
                    else:
                        download_failed += 1

        # Phase 2: hit /backend-api/me per account, extract image_gen quota,
        # update in-memory state. Invalid tokens just get their access_token
        # cleared via the existing remove_invalid_token() helper — that does
        # NOT touch CPA or the refresh_token.
        success = 0
        invalidated = 0
        errors = 0

        def _refresh_one(acct: _AccountState) -> str:
            token = acct.access_token
            if not token:
                return "skipped"
            try:
                # IMPORTANT: image_gen.remaining lives in /backend-api/conversation/init,
                # NOT /backend-api/me. get_user_info() fans out to /me + /init +
                # /accounts/check in parallel and merges them; we only need the
                # quota fields it extracts. Matches the vendor refresh path.
                info = OpenAIBackendAPI(access_token=token).get_user_info()
            except InvalidAccessTokenError:
                self.remove_invalid_token(token, "refresh_quotas")
                return "invalid"
            except Exception as exc:
                logger.warning("refresh: get_user_info failed for %s: %s", acct.email, exc)
                return "error"
            remaining = int(info.get("quota") or 0)
            unknown = bool(info.get("image_quota_unknown", True))
            with self._lock:
                acct.quota = remaining
                acct.quota_unknown = unknown
                # Promote from "fresh" once we've successfully heard back.
                if acct.status == "fresh":
                    acct.status = "active"
            return "ok"

        if targets:
            with ThreadPoolExecutor(max_workers=min(10, len(targets))) as ex:
                for fut in as_completed({ex.submit(_refresh_one, a): a for a in targets}):
                    outcome = fut.result()
                    if outcome == "ok":
                        success += 1
                    elif outcome == "invalid":
                        invalidated += 1
                    elif outcome == "error":
                        errors += 1

        return {
            "total_accounts": len(all_accts),
            "refreshed": success,
            "invalidated": invalidated,
            "errors": errors,
            "downloaded_from_cpa": downloaded,
            "download_failed": download_failed,
            "skipped": len(all_accts) - success - invalidated - errors - download_failed,
        }

    # ----- internal helpers -----

    def _refresh_file_list_if_stale(self) -> None:
        with self._lock:
            stale = (time.time() - self._cpa_files_ts) > config.cpa_list_cache_secs
        if stale:
            try:
                self._refresh_file_list_now()
            except Exception as exc:
                logger.warning("CPA list refresh failed: %s", exc)

    def _refresh_file_list_now(self) -> None:
        files = self._fetch_cpa_file_list()
        with self._lock:
            seen = set()
            for f in files:
                if not f.name or not f.email:
                    continue
                seen.add(f.name)
                acct = self._accounts.get(f.name)
                if acct is None:
                    # Brand new account — no cached token, quota unknown.
                    self._accounts[f.name] = _AccountState(
                        file_name=f.name, email=f.email, cpa_modtime=f.modtime,
                    )
                    logger.info("added CPA account: %s", f.email)
                else:
                    # Update mutable per-file metadata. If modtime bumped,
                    # CPA likely rotated the token — invalidate our copy
                    # so the next pick re-downloads.
                    if f.modtime > acct.cpa_modtime > 0:
                        acct.access_token = ""
                        acct.status = "invalid"
                        logger.info("CPA modtime bumped for %s; will re-download token", f.email)
                    acct.cpa_modtime = f.modtime
                    acct.email = f.email
                    # Disabled / unavailable accounts get parked; pick skips them.
                    if f.disabled or f.unavailable:
                        acct.status = "disabled"
                    elif acct.status == "disabled":
                        acct.status = "fresh"
            # Drop accounts CPA no longer lists.
            for name in list(self._accounts.keys()):
                if name not in seen:
                    removed = self._accounts.pop(name)
                    logger.info("removed CPA account: %s", removed.email or name)
            self._cpa_files_ts = time.time()

    def _fetch_cpa_file_list(self) -> list[_CPAFile]:
        if not config.cpa_base_url or not config.cpa_management_key:
            raise RuntimeError("cpa not configured (CPA_BASE_URL / CPA_MANAGEMENT_KEY missing)")
        url = config.cpa_base_url.rstrip("/") + "/v0/management/auth-files"
        r = self._cpa_session.get(
            url,
            headers={"Authorization": f"Bearer {config.cpa_management_key}", "Accept": "application/json"},
            timeout=15,
        )
        if r.status_code // 100 != 2:
            raise RuntimeError(f"CPA list failed: HTTP {r.status_code}")
        payload = r.json() or {}
        raw_files = payload.get("files") or []
        return [_CPAFile(item) for item in raw_files if isinstance(item, dict)]

    def _download_token_from_cpa(self, file_name: str) -> str:
        # CPA's download endpoint takes the file name as `?name=` (NOT ?file=)
        # and responds with text/plain whose body is a JSON document like:
        #   {"access_token": "eyJ...", "refresh_token": "...", "id_token": "..."}
        # curl_cffi's .json() parses regardless of Content-Type.
        url = config.cpa_base_url.rstrip("/") + "/v0/management/auth-files/download"
        r = self._cpa_session.get(
            url,
            params={"name": file_name},
            headers={"Authorization": f"Bearer {config.cpa_management_key}"},
            timeout=15,
        )
        if r.status_code // 100 != 2:
            raise RuntimeError(f"CPA download {file_name} failed: HTTP {r.status_code}")
        try:
            body = r.json() or {}
        except Exception as exc:
            raise RuntimeError(f"CPA download {file_name}: non-JSON body ({exc})")
        token = (
            body.get("access_token")
            or body.get("token")
            or (body.get("tokens") or {}).get("access_token")
            or ""
        )
        token = (token or "").strip() if isinstance(token, str) else ""
        if not token:
            raise RuntimeError(f"CPA download {file_name}: no access_token in response")
        return token

    def _pick_locked(self) -> _AccountState | None:
        """Lock-held; picks the best account or returns None."""
        max_inflight = max(1, config.image_account_concurrency)
        eligible = [
            a for a in self._accounts.values()
            if a.status != "disabled"
            and not a.in_use
            and a.inflight < max_inflight
            and (a.quota_unknown or a.quota > 0)
        ]
        if not eligible:
            return None
        # Round-robin: least-recently-used wins.
        eligible.sort(key=lambda a: (a.inflight, a.last_used_at))
        return eligible[0]

    def _ensure_token_locked(self, acct: _AccountState) -> str:
        """Lock-held during call; releases briefly for the CPA download which
        is a blocking I/O."""
        if acct.access_token:
            return acct.access_token
        # We need to drop the lock while doing HTTP, then re-acquire.
        file_name = acct.file_name
        self._lock.release()
        try:
            token = self._download_token_from_cpa(file_name)
        finally:
            self._lock.acquire()
        acct = self._accounts.get(file_name)
        if acct is None:
            # File disappeared while we were downloading. Reraise as no-quota
            # so the picker tries another account.
            raise RuntimeError("no available image quota")
        acct.access_token = token
        acct.status = "active"
        return token

    def _find_by_token_locked(self, access_token: str) -> _AccountState | None:
        if not access_token:
            return None
        for acct in self._accounts.values():
            if acct.access_token == access_token:
                return acct
        return None

    def _to_dict_locked(self, acct: _AccountState) -> dict[str, Any]:
        return {
            "file_name": acct.file_name,
            "email": acct.email,
            "status": acct.status,
            "quota": acct.quota,
            "quota_unknown": acct.quota_unknown,
            "last_used_at": acct.last_used_at,
            "success": acct.success,
            "fail": acct.fail,
            "inflight": acct.inflight,
        }

    def _to_dict_redacted_locked(self, acct: _AccountState) -> dict[str, Any]:
        d = self._to_dict_locked(acct)
        d["has_access_token"] = bool(acct.access_token)
        return d


account_service = AccountService()
