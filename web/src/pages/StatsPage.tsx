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
          Auto-refresh {autoRefresh ? 'ON' : 'OFF'}
        </button>
      </div>

      {/* Stat cards */}
      <div className="grid grid-cols-2 lg:grid-cols-4 gap-4 mb-6">
        <StatCard label={t('stats.totalStorage')} value={formatSize(stats.totalSize)} />
        <StatCard label={t('stats.totalObjects')} value={String(stats.totalObjects)} />
        <StatCard label={t('stats.buckets')} value={String(stats.totalBuckets)} />
        <StatCard label={t('stats.uptime')} value={formatUptime(stats.uptimeSeconds)} />
      </div>

      {/* Disk capacity (cluster-wide totals when clustered) */}
      {ci && ci.totals.disk.totalBytes > 0 && (
        <div className="bg-white dark:bg-gray-800 rounded-xl border border-gray-200 dark:border-gray-700 p-4 mb-6">
          <div className="flex items-center justify-between mb-2">
            <h3 className="text-sm font-semibold text-gray-900 dark:text-white">{t('stats.storageCapacity')}</h3>
            <span className="text-xs text-gray-500 dark:text-gray-400">
              {ci.clustered
                ? `${ci.reachableNodes}/${ci.nodeCount} nodes`
                : `${ci.nodes[0]?.version} · ${ci.nodes[0]?.os}/${ci.nodes[0]?.arch}`}
            </span>
          </div>
          <div className="h-3 w-full rounded-full bg-gray-200 dark:bg-gray-700 overflow-hidden">
            <div
              className={`h-full rounded-full ${
                ci.totals.disk.usedBytes / ci.totals.disk.totalBytes > 0.9 ? 'bg-red-500' : 'bg-indigo-500'
              }`}
              style={{ width: `${Math.min(100, (ci.totals.disk.usedBytes / ci.totals.disk.totalBytes) * 100).toFixed(1)}%` }}
            />
          </div>
          <div className="mt-2 flex flex-wrap gap-x-6 gap-y-1 text-xs text-gray-600 dark:text-gray-400">
            <span><span className="font-medium text-gray-900 dark:text-white">{formatSize(ci.totals.disk.usedBytes)}</span> used on disk</span>
            <span><span className="font-medium text-gray-900 dark:text-white">{formatSize(ci.totals.disk.freeBytes)}</span> free</span>
            <span><span className="font-medium text-gray-900 dark:text-white">{formatSize(ci.totals.disk.totalBytes)}</span> total on disk</span>
            <span className="text-gray-400 dark:text-gray-500">{formatSize(ci.totals.objectBytes)} in {ci.totals.objectCount} object{ci.totals.objectCount !== 1 ? 's' : ''} (logical)</span>
          </div>
          {/* The two figures above measure different things and are routinely far
              apart, which reads as a bug when they sit side by side (issue #43). */}
          <p className="mt-2 text-[11px] leading-relaxed text-gray-400 dark:text-gray-500">
            Disk usage is read from the filesystems backing the data directories, so it counts every
            replica and erasure shard, non-current object versions, and anything else stored on those
            disks. Logical size counts each object&apos;s current version once, cluster-wide.
          </p>

          {ci.clustered && ci.nodeCount > 1 && (
            <div className="mt-3 border-t border-gray-100 dark:border-gray-700/50 pt-3 space-y-1.5">
              {ci.nodes.map((n) => (
                <div key={n.nodeId} className="flex items-center gap-3 text-xs">
                  <span className={`h-2 w-2 rounded-full shrink-0 ${n.reachable ? 'bg-green-500' : 'bg-red-500'}`} />
                  <span className="font-mono text-gray-900 dark:text-white w-32 truncate" title={n.nodeId}>{n.nodeId}</span>
                  {n.reachable ? (
                    <>
                      <span className="text-gray-500 dark:text-gray-400 w-16">{n.version}</span>
                      <span className="text-gray-600 dark:text-gray-300">{formatSize(n.disk.usedBytes)} / {formatSize(n.disk.totalBytes)}</span>
                    </>
                  ) : (
                    <span className="text-red-500 dark:text-red-400 truncate" title={n.error}>
                      unreachable{n.error ? ` — ${n.error}` : ''}
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
                      {formatSize(b.size)} &middot; {b.objectCount} object{b.objectCount !== 1 ? 's' : ''}
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
                      Quota: {b.maxSizeBytes ? formatSize(b.maxSizeBytes) : 'unlimited'} / {b.maxObjects ? `${b.maxObjects} objects` : 'unlimited'}
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
