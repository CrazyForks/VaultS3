import { useState, useEffect, useMemo, useCallback } from 'react'
import { useI18n } from '../i18n'
import { getStats, getClusterInfo, type Stats, type ClusterInfo } from '../api/stats'
import { getActivity, type ActivityEntry } from '../api/activity'
import BarChart from '../components/BarChart'
import DonutChart from '../components/DonutChart'
import Sparkline from '../components/Sparkline'

const REFRESH_KEY = 'vaults3_stats_autorefresh'
const REFRESH_INTERVAL = 30000 // 30s

export default function StatsPage() {
  const { t } = useI18n()
  const [stats, setStats] = useState<Stats | null>(null)
  const [ci, setCi] = useState<ClusterInfo | null>(null)
  const [activity, setActivity] = useState<ActivityEntry[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [autoRefresh, setAutoRefresh] = useState(() => localStorage.getItem(REFRESH_KEY) !== 'false')

  const fetchData = useCallback(async () => {
    try {
      const [s, a, c] = await Promise.all([getStats(), getActivity(100), getClusterInfo().catch(() => null)])
      setStats(s)
      setCi(c)
      setActivity(a || [])
      setError('')
    } catch (err) {
      setError(err instanceof Error ? err.message : t('stats.loadFailed'))
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => { fetchData() }, [fetchData])

  useEffect(() => {
    if (!autoRefresh) return
    const interval = setInterval(fetchData, REFRESH_INTERVAL)
    return () => clearInterval(interval)
  }, [autoRefresh, fetchData])

  const toggleAutoRefresh = () => {
    setAutoRefresh(prev => {
      const next = !prev
      localStorage.setItem(REFRESH_KEY, String(next))
      return next
    })
  }

  // Build sparkline data: group activity entries into time buckets (last 100 entries -> 20 buckets of 5)
  const sparklineData = useMemo(() => {
    if (activity.length < 2) return []
    const bucketCount = 20
    const chunkSize = Math.ceil(activity.length / bucketCount)
    const buckets: number[] = []
    for (let i = 0; i < bucketCount; i++) {
      const chunk = activity.slice(i * chunkSize, (i + 1) * chunkSize)
      buckets.push(chunk.length)
    }
    return buckets.reverse() // oldest first
  }, [activity])

  if (loading) {
    return (
      <div className="flex items-center justify-center h-64">
        <div className="animate-spin rounded-full h-8 w-8 border-b-2 border-indigo-600" />
      </div>
    )
  }

  if (error || !stats) {
    return (
      <div className="p-3 rounded-lg bg-red-50 dark:bg-red-900/20 text-red-700 dark:text-red-400 text-sm">
        {error || t('stats.loadFailed')}
      </div>
    )
  }

  return (
    <div>
      <div className="flex items-center justify-between mb-6">
        <h2 className="text-xl font-semibold text-gray-900 dark:text-white">{t('stats.storageStats')}</h2>
        <button
          onClick={toggleAutoRefresh}
          className={`flex items-center gap-2 px-3 py-1.5 rounded-lg text-xs font-medium transition-colors ${
            autoRefresh
              ? 'bg-green-100 dark:bg-green-900/30 text-green-700 dark:text-green-400'
              : 'bg-gray-100 dark:bg-gray-700 text-gray-600 dark:text-gray-400'
          }`}
        >
          {autoRefresh && (
            <span className="relative flex h-2 w-2">
              <span className="animate-ping absolute inline-flex h-full w-full rounded-full bg-green-400 opacity-75" />
              <span className="relative inline-flex rounded-full h-2 w-2 bg-green-500" />
            </span>
          )}
          {t('stats.autoRefresh')} {autoRefresh ? t('stats.on') : t('stats.off')}
        </button>
      </div>

      {/* Stat cards */}
      <div className="grid grid-cols-2 lg:grid-cols-4 gap-4 mb-6">
        <StatCard label={t('stats.totalStorage')} value={formatSize(stats.totalSize)} />
        <StatCard label={t('stats.totalObjects')} value={String(stats.totalObjects)} />
        <StatCard label={t('stats.buckets')} value={String(stats.totalBuckets)} />
        <StatCard label={t('stats.uptime')} value={formatUptime(stats.uptimeSeconds)} />
      </div>

      {/* Storage (cluster-wide totals when clustered). Three sizes are shown
          separately and stacked on one bar, because reading one as another is
          what made a 258 GB cluster look like it was holding 2.27 TB (#43). */}
      {ci && ci.totals.disk.totalBytes > 0 && (
        <div className="bg-white dark:bg-gray-800 rounded-xl border border-gray-200 dark:border-gray-700 p-4 mb-6">
          <div className="flex items-center justify-between mb-3">
            <h3 className="text-sm font-semibold text-gray-900 dark:text-white">{t('stats.storageCapacity')}</h3>
            <span className="text-xs text-gray-500 dark:text-gray-400">
              {ci.clustered
                ? t('stats.nodesReachable', { reachable: ci.reachableNodes, total: ci.nodeCount })
                : `${ci.nodes[0]?.version} · ${ci.nodes[0]?.os}/${ci.nodes[0]?.arch}`}
            </span>
          </div>

          {/* VaultS3's share of the used space, then everything else on the same
              volumes. The gap between the two segments IS the answer to "why is
              disk usage so much bigger than my objects". */}
          <div className="h-3 w-full rounded-full bg-gray-200 dark:bg-gray-700 overflow-hidden flex">
            <div
              className="h-full bg-indigo-500"
              style={{ width: `${pctOf(vaultShare(ci), ci.totals.disk.totalBytes)}%` }}
              title={t('stats.vaultOnDisk')}
            />
            <div
              className={`h-full ${nearlyFull(ci) ? 'bg-red-500' : 'bg-gray-400 dark:bg-gray-500'}`}
              style={{ width: `${pctOf(ci.totals.disk.usedBytes - vaultShare(ci), ci.totals.disk.totalBytes)}%` }}
              title={t('stats.otherData')}
            />
          </div>

          <dl className="mt-3 space-y-1.5 text-xs">
            <Measurement
              swatch=""
              label={t('stats.logicalObjects')}
              value={formatSize(ci.totals.objectBytes)}
              hint={t('stats.logicalObjectsHint', { count: ci.totals.objectCount })}
            />
            <Measurement
              swatch="bg-indigo-500"
              label={t('stats.vaultOnDisk')}
              value={ci.totals.measuredNodes > 0 ? formatSize(ci.totals.vaultBytes) : '--'}
              hint={vaultHint(ci, t)}
            />
            <Measurement
              swatch={nearlyFull(ci) ? 'bg-red-500' : 'bg-gray-400 dark:bg-gray-500'}
              label={t('stats.otherData')}
              value={ci.totals.measuredNodes === ci.reachableNodes && ci.totals.measuredNodes > 0
                ? formatSize(Math.max(0, ci.totals.disk.usedBytes - vaultShare(ci)))
                : '--'}
              hint={t('stats.otherDataHint')}
            />
            <Measurement
              swatch=""
              label={t('stats.filesystems')}
              value={formatSize(ci.totals.disk.usedBytes)}
              hint={t('stats.filesystemsHint', {
                total: formatSize(ci.totals.disk.totalBytes),
                free: formatSize(ci.totals.disk.freeBytes),
              })}
            />
          </dl>

          <p className="mt-3 text-[11px] leading-relaxed text-gray-400 dark:text-gray-500">
            {t('stats.storageExplain')}
          </p>

          {/* Per-directory split: the quickest way to tell object data apart from
              metadata and Raft logs when the footprint looks larger than expected. */}
          {ci.nodes[0]?.usage && ci.nodes[0].usage.dirs.length > 0 && (
            <div className="mt-3 border-t border-gray-100 dark:border-gray-700/50 pt-3">
              {/* The age matters: the walk is cached, so a number read right
                  after a large upload legitimately lags behind it. */}
              <p className="text-xs font-medium text-gray-700 dark:text-gray-300 mb-1.5">
                {t('stats.thisNodeDirs')}{' '}
                <span className="font-normal text-gray-400 dark:text-gray-500">
                  {t('stats.measuredAgo', { age: shortAge(ci.nodes[0].usage!.scannedAt) })}
                </span>
              </p>
              <div className="space-y-1">
                {ci.nodes[0].usage.dirs.map((d) => (
                  <div key={d.path} className="flex items-center justify-between gap-3 text-xs">
                    <span className="font-mono text-gray-600 dark:text-gray-400 truncate" title={d.path}>{d.path}</span>
                    {d.error ? (
                      <span className="text-red-500 dark:text-red-400 shrink-0" title={d.error}>{t('stats.unreadable')}</span>
                    ) : (
                      <span className="text-gray-900 dark:text-white shrink-0">
                        {formatSize(d.bytes)} <span className="text-gray-400 dark:text-gray-500">{t('stats.filesCount', { count: d.files })}</span>
                      </span>
                    )}
                  </div>
                ))}
              </div>
            </div>
          )}

          {ci.clustered && ci.nodeCount > 1 && (
            <div className="mt-3 border-t border-gray-100 dark:border-gray-700/50 pt-3 space-y-1.5">
              {ci.nodes.map((n) => (
                <div key={n.nodeId} className="flex items-center gap-3 text-xs">
                  <span className={`h-2 w-2 rounded-full shrink-0 ${n.reachable ? 'bg-green-500' : 'bg-red-500'}`} />
                  <span className="font-mono text-gray-900 dark:text-white w-32 truncate" title={n.nodeId}>{n.nodeId}</span>
                  {n.reachable ? (
                    <>
                      <span className="text-gray-500 dark:text-gray-400 w-16">{n.version}</span>
                      <span className="text-gray-600 dark:text-gray-300">
                        {n.usage ? formatSize(n.usage.bytes) : t('stats.measuring')}
                        <span className="text-gray-400 dark:text-gray-500"> / {formatSize(n.disk.usedBytes)} {t('stats.usedOnFilesystem')}</span>
                      </span>
                    </>
                  ) : (
                    <span className="text-red-500 dark:text-red-400 truncate" title={n.error}>
                      {t('stats.unreachable')}{n.error ? `: ${n.error}` : ''}
                    </span>
                  )}
                </div>
              ))}
            </div>
          )}
        </div>
      )}

      {/* Request stat cards */}
      <div className="grid grid-cols-2 lg:grid-cols-4 gap-4 mb-6">
        <StatCard label={t('stats.requests')} value={stats.totalRequests.toLocaleString()} />
        <StatCard label={t('stats.errors')} value={stats.totalErrors.toLocaleString()} />
        <StatCard label={t('stats.bytesIn')} value={formatSize(stats.bytesIn)} />
        <StatCard label={t('stats.bytesOut')} value={formatSize(stats.bytesOut)} />
      </div>

      {/* Runtime + Sparkline */}
      <div className="grid grid-cols-1 lg:grid-cols-3 gap-4 mb-6">
        <div className="bg-white dark:bg-gray-800 rounded-xl border border-gray-200 dark:border-gray-700 p-4">
          <p className="text-xs text-gray-500 dark:text-gray-400 uppercase tracking-wider font-medium mb-1">{t('stats.goroutines')}</p>
          <p className="text-2xl font-semibold text-gray-900 dark:text-white">{stats.goroutines}</p>
        </div>
        <div className="bg-white dark:bg-gray-800 rounded-xl border border-gray-200 dark:border-gray-700 p-4">
          <p className="text-xs text-gray-500 dark:text-gray-400 uppercase tracking-wider font-medium mb-1">{t('stats.memory')}</p>
          <p className="text-2xl font-semibold text-gray-900 dark:text-white">{stats.memoryMB.toFixed(1)} MB</p>
        </div>
        <div className="bg-white dark:bg-gray-800 rounded-xl border border-gray-200 dark:border-gray-700 p-4">
          <p className="text-xs text-gray-500 dark:text-gray-400 uppercase tracking-wider font-medium mb-2">{t('stats.requestActivity')}</p>
          {sparklineData.length > 1 ? (
            <Sparkline data={sparklineData} height={36} />
          ) : (
            <p className="text-xs text-gray-400 dark:text-gray-500">{t('stats.noActivityData')}</p>
          )}
        </div>
      </div>

      {/* Charts */}
      <div className="grid grid-cols-1 lg:grid-cols-2 gap-4 mb-6">
        {/* Donut chart -- request method distribution */}
        {stats.requestsByMethod && stats.requestsByMethod.length > 0 && (
          <div className="bg-white dark:bg-gray-800 rounded-xl border border-gray-200 dark:border-gray-700 p-5">
            <h3 className="text-sm font-semibold text-gray-900 dark:text-white mb-4">{t('stats.requestsByMethod')}</h3>
            <DonutChart
              items={stats.requestsByMethod.map(r => ({
                label: r.method,
                value: r.count,
              }))}
            />
          </div>
        )}

        {/* Bar chart -- per-bucket sizes */}
        {stats.buckets.length > 0 && (
          <div className="bg-white dark:bg-gray-800 rounded-xl border border-gray-200 dark:border-gray-700 p-5">
            <h3 className="text-sm font-semibold text-gray-900 dark:text-white mb-4">{t('stats.bucketSizes')}</h3>
            <BarChart
              items={stats.buckets.map(b => ({
                label: b.name,
                value: b.size,
              }))}
              formatValue={formatSize}
            />
          </div>
        )}
      </div>

      {/* Per-bucket breakdown */}
      {stats.buckets.length > 0 && (
        <div className="bg-white dark:bg-gray-800 rounded-xl border border-gray-200 dark:border-gray-700 p-6">
          <h3 className="text-sm font-semibold text-gray-900 dark:text-white mb-4">{t('stats.perBucketStorage')}</h3>
          <div className="space-y-3">
            {stats.buckets.map((b) => {
              const maxSize = Math.max(...stats.buckets.map(x => x.size), 1)
              return (
                <div key={b.name}>
                  <div className="flex items-center justify-between text-sm mb-1">
                    <span className="text-gray-700 dark:text-gray-300 font-medium">{b.name}</span>
                    <span className="text-gray-500 dark:text-gray-400">
                      {formatSize(b.size)} &middot; {t('stats.objectsCount', { count: b.objectCount })}
                    </span>
                  </div>
                  <div className="w-full bg-gray-100 dark:bg-gray-700 rounded-full h-2">
                    <div
                      className="bg-indigo-600 h-2 rounded-full transition-all"
                      style={{ width: `${Math.max((b.size / maxSize) * 100, 1)}%` }}
                    />
                  </div>
                  {(b.maxSizeBytes || b.maxObjects) && (
                    <p className="text-xs text-gray-400 dark:text-gray-500 mt-0.5">
                      {t('stats.quota', {
                        size: b.maxSizeBytes ? formatSize(b.maxSizeBytes) : t('stats.unlimited'),
                        objects: b.maxObjects ? t('stats.objectsCount', { count: b.maxObjects }) : t('stats.unlimited'),
                      })}
                    </p>
                  )}
                </div>
              )
            })}
          </div>
        </div>
      )}
    </div>
  )
}

/** One row of the storage breakdown: colour key, what it measures, how much. */
function Measurement({ swatch, label, value, hint }: { swatch: string; label: string; value: string; hint: string }) {
  return (
    <div className="flex items-baseline gap-2">
      <span className={`h-2 w-2 rounded-full shrink-0 translate-y-[-1px] ${swatch || 'bg-transparent'}`} />
      <dt className="text-gray-600 dark:text-gray-400 w-32 shrink-0">{label}</dt>
      <dd className="font-medium text-gray-900 dark:text-white w-24 shrink-0">{value}</dd>
      <span className="text-gray-400 dark:text-gray-500">{hint}</span>
    </div>
  )
}

/** VaultS3's measured footprint, never more than the filesystem says is used. */
function vaultShare(ci: ClusterInfo): number {
  if (ci.totals.measuredNodes === 0 || ci.totals.measuredNodes !== ci.reachableNodes) return 0
  return Math.min(ci.totals.vaultBytes, ci.totals.disk.usedBytes)
}

function vaultHint(ci: ClusterInfo, t: (k: string, v?: Record<string, string | number>) => string): string {
  if (ci.totals.measuredNodes === 0) {
    return ci.nodes[0]?.usageScanning ? t('stats.measuring') : t('stats.measurementOff')
  }
  const parts: string[] = []
  if (ci.clustered) {
    parts.push(t('stats.measuredOnNodes', { measured: ci.totals.measuredNodes, total: ci.reachableNodes }))
  }
  if (ci.totals.objectBytes > 0 && ci.totals.measuredNodes === ci.reachableNodes) {
    parts.push(t('stats.timesLogical', { ratio: (ci.totals.vaultBytes / ci.totals.objectBytes).toFixed(2) }))
  }
  return parts.join(', ') || t('stats.vaultOnDiskHint')
}

function shortAge(iso: string): string {
  const secs = Math.max(0, Math.round((Date.now() - new Date(iso).getTime()) / 1000))
  if (secs < 60) return `${secs}s`
  if (secs < 3600) return `${Math.round(secs / 60)}m`
  return `${Math.round(secs / 3600)}h`
}

function nearlyFull(ci: ClusterInfo): boolean {
  return ci.totals.disk.usedBytes / ci.totals.disk.totalBytes > 0.9
}

function pctOf(part: number, whole: number): string {
  if (whole <= 0) return '0'
  return Math.max(0, Math.min(100, (part / whole) * 100)).toFixed(1)
}

function StatCard({ label, value }: { label: string; value: string }) {
  return (
    <div className="bg-white dark:bg-gray-800 rounded-xl border border-gray-200 dark:border-gray-700 p-4">
      <p className="text-xs text-gray-500 dark:text-gray-400 uppercase tracking-wider font-medium mb-1">{label}</p>
      <p className="text-2xl font-semibold text-gray-900 dark:text-white">{value}</p>
    </div>
  )
}

function formatSize(bytes: number): string {
  if (bytes === 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.floor(Math.log(bytes) / Math.log(1024))
  return `${(bytes / Math.pow(1024, i)).toFixed(i > 0 ? 1 : 0)} ${units[i]}`
}

function formatUptime(seconds: number): string {
  const d = Math.floor(seconds / 86400)
  const h = Math.floor((seconds % 86400) / 3600)
  const m = Math.floor((seconds % 3600) / 60)
  if (d > 0) return `${d}d ${h}h`
  if (h > 0) return `${h}h ${m}m`
  return `${m}m`
}
