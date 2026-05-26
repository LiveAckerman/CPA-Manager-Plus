/**
 * Image-pool monitoring panel.
 *
 * Aggregate diagnostic view over the in-container image-service's account
 * pool: how many accounts are in the pool, how much image_gen quota is
 * left across them, success/fail counters, and a paginated/sortable table
 * of every account in the pool.
 *
 * The refresh button forces a get_user_info() round-trip for every pool
 * account (downloading the access_token from CPA first if not cached).
 * That fills in the `quota` numbers, which otherwise stay at 0+unknown
 * until each account is first used for a real image gen. See
 * `imagePool.ts` for the safety argument (no refresh_token rotation).
 *
 * Colour conventions match the page's existing monitoring styles:
 *   - status badges: active=green / fresh=amber / invalid=red
 *   - quota cell: 0=red, 1-5=amber, >5=green, unknown=muted
 *   - success summary uses good tone; fail summary uses bad tone when >0.
 */
import {
  useCallback,
  useEffect,
  useMemo,
  useState,
  type CSSProperties,
} from 'react';
import { useTranslation } from 'react-i18next';
import { Card } from '@/components/ui/Card';
import { Select } from '@/components/ui/Select';
import { Button } from '@/components/ui/Button';
import {
  imagePoolApi,
  type ImagePoolAccount,
  type ImagePoolAccountStatus,
  type ImagePoolRefreshResult,
} from '@/services/api/imagePool';
import { MonitoringPanel } from './MonitoringPanel';
import styles from '@/pages/MonitoringCenterPage.module.scss';

type SortMode =
  | 'quota-desc'
  | 'success-desc'
  | 'fail-desc'
  | 'last-used-desc'
  | 'email-asc';

// Match how the rest of the monitoring page masks emails (see
// useMonitoringData.maskEmailLike). Tiny copy because that helper isn't
// exported.
const maskEmail = (email: string): string => {
  const trimmed = (email || '').trim();
  const match = trimmed.match(/^([^@\s]{1,3})[^@\s]*@(.+)$/);
  if (!match) return trimmed;
  return `${match[1]}***@${match[2]}`;
};

const formatLastUsed = (epochSeconds: number, locale: string): string => {
  if (!epochSeconds || !Number.isFinite(epochSeconds)) return '—';
  try {
    return new Date(epochSeconds * 1000).toLocaleString(locale);
  } catch {
    return '—';
  }
};

// Quota cell colour mapping. Picked so the table can be skim-scanned at a
// glance: red means "out", amber means "running low", green means "plenty".
const quotaTone = (quota: number, unknown: boolean): 'good' | 'warn' | 'bad' | null => {
  if (unknown) return null;
  if (quota <= 0) return 'bad';
  if (quota <= 5) return 'warn';
  return 'good';
};

const statusTone = (status: ImagePoolAccountStatus): 'good' | 'warn' | 'bad' => {
  switch (status) {
    case 'active':
      return 'good';
    case 'fresh':
      return 'warn';
    case 'invalid':
    default:
      return 'bad';
  }
};

const toneClass = (tone: 'good' | 'warn' | 'bad' | null | undefined): string => {
  if (!tone) return '';
  return tone === 'good' ? styles.tonegood : tone === 'warn' ? styles.tonewarn : styles.tonebad;
};

const PAGE_SIZE = 20;

export function MonitoringImagePoolBlock() {
  const { t, i18n } = useTranslation();

  const [accounts, setAccounts] = useState<ImagePoolAccount[]>([]);
  const [loading, setLoading] = useState(false);
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [lastRefreshResult, setLastRefreshResult] = useState<ImagePoolRefreshResult | null>(null);
  const [sortMode, setSortMode] = useState<SortMode>('quota-desc');
  const [page, setPage] = useState(1);

  const fetchAccounts = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const data = await imagePoolApi.list();
      setAccounts(data.items || []);
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err);
      setError(msg);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void fetchAccounts();
  }, [fetchAccounts]);

  const handleRefreshClick = useCallback(async () => {
    setRefreshing(true);
    setError(null);
    try {
      const result = await imagePoolApi.refresh(true);
      setLastRefreshResult(result);
      await fetchAccounts();
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err);
      setError(msg);
    } finally {
      setRefreshing(false);
    }
  }, [fetchAccounts]);

  // Aggregate stats — derived from the in-memory list, no extra API calls.
  const stats = useMemo(() => {
    const total = accounts.length;
    const knownQuota = accounts.filter((a) => !a.quota_unknown);
    const unknownQuota = total - knownQuota.length;
    const totalRemaining = knownQuota.reduce((sum, a) => sum + (a.quota || 0), 0);
    const totalSuccess = accounts.reduce((sum, a) => sum + (a.success || 0), 0);
    const totalFail = accounts.reduce((sum, a) => sum + (a.fail || 0), 0);
    const totalInflight = accounts.reduce((sum, a) => sum + (a.inflight || 0), 0);
    const statusCounts: Record<ImagePoolAccountStatus, number> = {
      fresh: 0,
      active: 0,
      invalid: 0,
    };
    accounts.forEach((a) => {
      if (a.status in statusCounts) statusCounts[a.status]++;
    });
    return {
      total,
      knownQuota: knownQuota.length,
      unknownQuota,
      totalRemaining,
      totalSuccess,
      totalFail,
      totalInflight,
      statusCounts,
    };
  }, [accounts]);

  const sortedAccounts = useMemo(() => {
    const copy = [...accounts];
    switch (sortMode) {
      case 'quota-desc':
        copy.sort((a, b) => {
          if (a.quota_unknown && !b.quota_unknown) return 1;
          if (!a.quota_unknown && b.quota_unknown) return -1;
          return (b.quota || 0) - (a.quota || 0);
        });
        break;
      case 'success-desc':
        copy.sort((a, b) => (b.success || 0) - (a.success || 0));
        break;
      case 'fail-desc':
        copy.sort((a, b) => (b.fail || 0) - (a.fail || 0));
        break;
      case 'last-used-desc':
        copy.sort((a, b) => (b.last_used_at || 0) - (a.last_used_at || 0));
        break;
      case 'email-asc':
        copy.sort((a, b) => (a.email || '').localeCompare(b.email || ''));
        break;
    }
    return copy;
  }, [accounts, sortMode]);

  const totalPages = Math.max(1, Math.ceil(sortedAccounts.length / PAGE_SIZE));
  const currentPage = Math.min(Math.max(1, page), totalPages);
  const pageItems = sortedAccounts.slice(
    (currentPage - 1) * PAGE_SIZE,
    currentPage * PAGE_SIZE
  );

  // Reset to page 1 when the sort changes.
  useEffect(() => {
    setPage(1);
  }, [sortMode]);

  const sortOptions = useMemo(
    () => [
      { value: 'quota-desc', label: t('monitoring.image_pool_sort_quota_desc') },
      { value: 'success-desc', label: t('monitoring.image_pool_sort_success_desc') },
      { value: 'fail-desc', label: t('monitoring.image_pool_sort_fail_desc') },
      { value: 'last-used-desc', label: t('monitoring.image_pool_sort_last_used_desc') },
      { value: 'email-asc', label: t('monitoring.image_pool_sort_email_asc') },
    ],
    [t]
  );

  const statusLabel = (status: ImagePoolAccountStatus): string =>
    t(`monitoring.image_pool_status_${status}`);

  const remainingValueText = stats.unknownQuota
    ? `${stats.totalRemaining}`
    : `${stats.totalRemaining}`;

  // Inline style fragments. Kept here rather than in CSS so this component
  // stays drop-in; the colour values come from the page-level CSS variables.
  const refreshSummaryStyle: CSSProperties = {
    display: 'inline-flex',
    gap: 6,
    alignItems: 'center',
    fontSize: 12,
    color: 'var(--monitor-muted, #888)',
  };
  const refreshChipStyle = (tone: 'good' | 'warn' | 'bad' | null): CSSProperties => ({
    padding: '2px 8px',
    borderRadius: 999,
    fontWeight: 600,
    background:
      tone === 'good'
        ? 'color-mix(in srgb, var(--monitor-green) 12%, transparent)'
        : tone === 'warn'
          ? 'color-mix(in srgb, var(--monitor-amber) 12%, transparent)'
          : tone === 'bad'
            ? 'color-mix(in srgb, var(--monitor-red) 12%, transparent)'
            : 'transparent',
    color:
      tone === 'good'
        ? 'var(--monitor-green)'
        : tone === 'warn'
          ? 'var(--monitor-amber)'
          : tone === 'bad'
            ? 'var(--monitor-red)'
            : 'inherit',
  });
  const toolbarRowStyle: CSSProperties = {
    display: 'flex',
    gap: 12,
    alignItems: 'center',
    marginTop: 16,
    marginBottom: 8,
    flexWrap: 'wrap',
  };
  const tableCellTone: CSSProperties = { fontWeight: 600 };

  return (
    <MonitoringPanel
      title={t('monitoring.image_pool_title')}
      subtitle={t('monitoring.image_pool_subtitle')}
      extra={
        <div
          style={{
            display: 'flex',
            gap: 12,
            alignItems: 'center',
            flexWrap: 'wrap',
          }}
        >
          {lastRefreshResult ? (
            <span style={refreshSummaryStyle}>
              <span>{t('monitoring.image_pool_last_refresh_prefix')}</span>
              <span style={refreshChipStyle('good')}>
                ✓ {lastRefreshResult.refreshed}
              </span>
              <span
                style={refreshChipStyle(lastRefreshResult.invalidated ? 'warn' : null)}
              >
                ⊘ {lastRefreshResult.invalidated}
              </span>
              <span style={refreshChipStyle(lastRefreshResult.errors ? 'bad' : null)}>
                ⚠ {lastRefreshResult.errors}
              </span>
            </span>
          ) : null}
          <Button
            variant="primary"
            size="sm"
            onClick={() => void handleRefreshClick()}
            disabled={refreshing || loading}
          >
            {refreshing
              ? t('monitoring.image_pool_refreshing')
              : t('monitoring.image_pool_refresh_button')}
          </Button>
        </div>
      }
    >
      {error ? <div className={styles.errorBanner}>{error}</div> : null}

      {/* Summary cards — match the page's existing summary-card styling. */}
      <div className={styles.summarySub}>
        <Card className={`${styles.summaryCard} ${styles.summaryCardSecondary}`}>
          <span className={styles.summaryLabel}>
            {t('monitoring.image_pool_total_accounts')}
          </span>
          <strong className={styles.summaryValue}>{stats.total}</strong>
          <span className={styles.summaryMeta}>
            <span className={styles.tonegood}>● {stats.statusCounts.active}</span>{' '}
            <span className={styles.tonewarn}>● {stats.statusCounts.fresh}</span>{' '}
            <span className={styles.tonebad}>● {stats.statusCounts.invalid}</span>
          </span>
        </Card>
        <Card className={`${styles.summaryCard} ${styles.summaryCardSecondary}`}>
          <span className={styles.summaryLabel}>
            {t('monitoring.image_pool_total_remaining_quota')}
          </span>
          <strong
            className={`${styles.summaryValue} ${stats.totalRemaining > 0 ? styles.tonegood : ''}`}
          >
            {remainingValueText}
          </strong>
          <span className={styles.summaryMeta}>
            {t('monitoring.image_pool_quota_meta', {
              known: stats.knownQuota,
              unknown: stats.unknownQuota,
            })}
          </span>
        </Card>
        <Card className={`${styles.summaryCard} ${styles.summaryCardSecondary}`}>
          <span className={styles.summaryLabel}>
            {t('monitoring.image_pool_total_success')}
          </span>
          <strong className={`${styles.summaryValue} ${styles.tonegood}`}>
            {stats.totalSuccess}
          </strong>
          <span className={styles.summaryMeta}>
            {t('monitoring.image_pool_inflight', { count: stats.totalInflight })}
          </span>
        </Card>
        <Card className={`${styles.summaryCard} ${styles.summaryCardSecondary}`}>
          <span className={styles.summaryLabel}>
            {t('monitoring.image_pool_total_fail')}
          </span>
          <strong
            className={`${styles.summaryValue} ${stats.totalFail > 0 ? styles.tonebad : ''}`}
          >
            {stats.totalFail}
          </strong>
          <span className={styles.summaryMeta}>
            {stats.totalSuccess + stats.totalFail > 0
              ? `${((stats.totalSuccess / (stats.totalSuccess + stats.totalFail)) * 100).toFixed(1)}% ${t('monitoring.image_pool_success_rate')}`
              : '—'}
          </span>
        </Card>
      </div>

      {/* Sort + summary line */}
      <div style={toolbarRowStyle}>
        <label
          style={{ fontSize: 12, color: 'var(--monitor-muted, #888)', marginRight: -4 }}
        >
          {t('monitoring.image_pool_sort_label')}
        </label>
        <Select
          value={sortMode}
          options={sortOptions}
          onChange={(value) => setSortMode(value as SortMode)}
          ariaLabel={t('monitoring.image_pool_sort_label')}
        />
        <span style={{ marginLeft: 'auto', color: 'var(--monitor-muted, #888)', fontSize: 13 }}>
          {t('monitoring.image_pool_pagination_summary', {
            start: sortedAccounts.length === 0 ? 0 : (currentPage - 1) * PAGE_SIZE + 1,
            end: Math.min(currentPage * PAGE_SIZE, sortedAccounts.length),
            total: sortedAccounts.length,
          })}
        </span>
      </div>

      {/* Table */}
      <div className={styles.tableWrapper}>
        <table className={styles.table}>
          <thead>
            <tr>
              <th>{t('monitoring.image_pool_col_email')}</th>
              <th>{t('monitoring.image_pool_col_status')}</th>
              <th>{t('monitoring.image_pool_col_quota')}</th>
              <th>{t('monitoring.image_pool_col_success')}</th>
              <th>{t('monitoring.image_pool_col_fail')}</th>
              <th>{t('monitoring.image_pool_col_inflight')}</th>
              <th>{t('monitoring.image_pool_col_last_used')}</th>
            </tr>
          </thead>
          <tbody>
            {pageItems.length === 0 ? (
              <tr>
                <td
                  colSpan={7}
                  style={{
                    textAlign: 'center',
                    padding: 24,
                    color: 'var(--monitor-muted, #888)',
                  }}
                >
                  {loading ? t('common.loading') : t('monitoring.image_pool_empty')}
                </td>
              </tr>
            ) : (
              pageItems.map((acct) => {
                const sTone = statusTone(acct.status);
                const qTone = quotaTone(acct.quota, acct.quota_unknown);
                const failTone = acct.fail > 0 ? 'bad' : null;
                return (
                  <tr key={acct.file_name}>
                    <td title={acct.email}>{maskEmail(acct.email)}</td>
                    <td>
                      <span className={`${styles.statusBadge} ${toneClass(sTone)}`}>
                        {statusLabel(acct.status)}
                      </span>
                    </td>
                    <td>
                      <span
                        className={toneClass(qTone)}
                        style={tableCellTone}
                      >
                        {acct.quota_unknown ? '?' : acct.quota}
                      </span>
                    </td>
                    <td>
                      {acct.success > 0 ? (
                        <span className={styles.tonegood} style={tableCellTone}>
                          {acct.success}
                        </span>
                      ) : (
                        <span style={{ color: 'var(--monitor-muted, #888)' }}>0</span>
                      )}
                    </td>
                    <td>
                      <span className={toneClass(failTone)} style={tableCellTone}>
                        {acct.fail}
                      </span>
                    </td>
                    <td>{acct.inflight > 0 ? acct.inflight : '—'}</td>
                    <td>{formatLastUsed(acct.last_used_at, i18n.language)}</td>
                  </tr>
                );
              })
            )}
          </tbody>
        </table>
      </div>

      {/* Pagination */}
      {totalPages > 1 ? (
        <div
          style={{
            display: 'flex',
            gap: 8,
            justifyContent: 'center',
            alignItems: 'center',
            marginTop: 16,
          }}
        >
          <Button
            variant="ghost"
            size="sm"
            onClick={() => setPage((p) => Math.max(1, p - 1))}
            disabled={currentPage <= 1}
          >
            {t('monitoring.image_pool_prev_page')}
          </Button>
          <span style={{ fontSize: 13, color: 'var(--monitor-muted, #888)' }}>
            {t('monitoring.image_pool_page_x_of_y', {
              current: currentPage,
              total: totalPages,
            })}
          </span>
          <Button
            variant="ghost"
            size="sm"
            onClick={() => setPage((p) => Math.min(totalPages, p + 1))}
            disabled={currentPage >= totalPages}
          >
            {t('monitoring.image_pool_next_page')}
          </Button>
        </div>
      ) : null}
    </MonitoringPanel>
  );
}
