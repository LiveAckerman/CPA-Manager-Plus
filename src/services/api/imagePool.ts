/**
 * Image-service account pool diagnostic API.
 *
 * Talks to the in-container image-service through the cpa-manager reverse
 * proxy at /v0/image/* (which rewrites to /api/* on the backend). Read-only
 * + a single force-refresh action; nothing here issues tokens or rotates
 * credentials.
 */

import { apiClient } from './client';

export type ImagePoolAccountStatus = 'fresh' | 'active' | 'invalid';

export interface ImagePoolAccount {
  /** CPA-side file name (e.g. "codex-x@y.com-free.json") — stable identifier. */
  file_name: string;
  email: string;
  status: ImagePoolAccountStatus;
  /** image_gen.remaining as reported by ChatGPT's /backend-api/me. */
  quota: number;
  /** True until we've successfully called ChatGPT once for this account. */
  quota_unknown: boolean;
  /** UNIX seconds (float). 0 means never used. */
  last_used_at: number;
  /** Successful image-gen calls since service start. */
  success: number;
  /** Failed image-gen calls since service start. */
  fail: number;
  /** Concurrent image-gen calls currently in flight against this account. */
  inflight: number;
  /** Whether the access_token has been downloaded from CPA into memory yet. */
  has_access_token: boolean;
}

export interface ImagePoolListResponse {
  items: ImagePoolAccount[];
}

export interface ImagePoolRefreshResult {
  total_accounts: number;
  refreshed: number;
  invalidated: number;
  errors: number;
  downloaded_from_cpa: number;
  download_failed: number;
  skipped: number;
}

export const imagePoolApi = {
  list(): Promise<ImagePoolListResponse> {
    return apiClient.panelGet<ImagePoolListResponse>('/v0/image/accounts');
  },

  /**
   * Force-refresh image_gen quota for the pool.
   *
   * include_uncached=true (default) also downloads tokens from CPA for
   * accounts that haven't been used yet — what an operator clicking the
   * panel's refresh button actually wants. Pass false for a faster pass
   * that only re-checks accounts whose token is already in memory.
   *
   * Safe vs refresh_token: this never calls any OAuth grant endpoint and
   * never asks CPA to rotate credentials. It's read-only against ChatGPT
   * (`/backend-api/me`) and read-only against CPA
   * (`/v0/management/auth-files/download`).
   */
  refresh(includeUncached = true): Promise<ImagePoolRefreshResult> {
    return apiClient.panelPost<ImagePoolRefreshResult>(
      `/v0/image/accounts/refresh?include_uncached=${includeUncached ? 'true' : 'false'}`,
      undefined
    );
  },
};
