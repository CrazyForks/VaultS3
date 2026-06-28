import { useState, useEffect, useCallback, useRef } from 'react'
import { testMigrateSource, startMigration, listMigrateJobs, type MigrateJob } from '../api/migrate'

export default function MigrationPage() {
  const [endpoint, setEndpoint] = useState('')
  const [accessKey, setAccessKey] = useState('')
  const [secretKey, setSecretKey] = useState('')
  const [region, setRegion] = useState('us-east-1')

  const [buckets, setBuckets] = useState<string[]>([])
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [tested, setTested] = useState(false)

  const [testing, setTesting] = useState(false)
  const [starting, setStarting] = useState(false)
  const [error, setError] = useState('')
  const [jobs, setJobs] = useState<MigrateJob[]>([])

  const pollRef = useRef<number | null>(null)

  const refreshJobs = useCallback(async () => {
    try {
      const data = await listMigrateJobs()
      setJobs((data || []).sort((a, b) => b.started_at - a.started_at))
    } catch {
      /* ignore transient poll errors */
    }
  }, [])

  useEffect(() => {
    refreshJobs()
    pollRef.current = window.setInterval(refreshJobs, 2000)
    return () => {
      if (pollRef.current) window.clearInterval(pollRef.current)
    }
  }, [refreshJobs])

  const source = () => ({ endpoint: endpoint.trim(), accessKey: accessKey.trim(), secretKey: secretKey.trim(), region: region.trim() })

  const handleTest = async () => {
    if (!endpoint.trim()) return
    setTesting(true)
    setError('')
    setTested(false)
    try {
      const { buckets } = await testMigrateSource(source())
      setBuckets(buckets || [])
      setSelected(new Set(buckets || []))
      setTested(true)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Connection failed')
    } finally {
      setTesting(false)
    }
  }

  const handleStart = async () => {
    setStarting(true)
    setError('')
    try {
      await startMigration({ ...source(), buckets: Array.from(selected) })
      await refreshJobs()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not start migration')
    } finally {
      setStarting(false)
    }
  }

  const toggle = (b: string) => {
    setSelected(prev => {
      const next = new Set(prev)
      next.has(b) ? next.delete(b) : next.add(b)
      return next
    })
  }

  const input = "w-full px-3 py-2.5 rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800 text-gray-900 dark:text-white text-sm"
  const label = "block text-xs font-medium text-gray-500 dark:text-gray-400 mb-1"

  return (
    <div>
      <div className="mb-6">
        <h2 className="text-xl font-semibold text-gray-900 dark:text-white">Migrate from S3</h2>
        <p className="text-sm text-gray-500 dark:text-gray-400 mt-0.5">
          Import buckets and objects from MinIO, AWS S3, or any S3-compatible source.
        </p>
      </div>

      {error && (
        <div className="mb-4 p-3 rounded-lg bg-red-50 dark:bg-red-900/20 text-red-700 dark:text-red-400 text-sm">
          {error}
        </div>
      )}

      {/* Step 1: source */}
      <div className="bg-white dark:bg-gray-800 rounded-xl border border-gray-200 dark:border-gray-700 p-5 mb-5">
        <h3 className="text-sm font-semibold text-gray-900 dark:text-white mb-4">1. Source endpoint</h3>
        <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
          <div className="md:col-span-2">
            <label className={label}>Endpoint URL</label>
            <input className={input} placeholder="http://old-minio:9000" value={endpoint} onChange={e => setEndpoint(e.target.value)} />
          </div>
          <div>
            <label className={label}>Access Key</label>
            <input className={input} value={accessKey} onChange={e => setAccessKey(e.target.value)} />
          </div>
          <div>
            <label className={label}>Secret Key</label>
            <input className={input} type="password" value={secretKey} onChange={e => setSecretKey(e.target.value)} />
          </div>
          <div>
            <label className={label}>Region</label>
            <input className={input} value={region} onChange={e => setRegion(e.target.value)} />
          </div>
        </div>
        <button onClick={handleTest} disabled={testing || !endpoint.trim()}
          className="mt-4 px-5 py-2.5 rounded-lg bg-indigo-600 hover:bg-indigo-700 disabled:bg-indigo-400 text-white text-sm font-medium transition-colors">
          {testing ? 'Connecting...' : 'Connect & list buckets'}
        </button>
      </div>

      {/* Step 2: select buckets */}
      {tested && (
        <div className="bg-white dark:bg-gray-800 rounded-xl border border-gray-200 dark:border-gray-700 p-5 mb-5">
          <h3 className="text-sm font-semibold text-gray-900 dark:text-white mb-4">2. Select buckets to import</h3>
          {buckets.length === 0 ? (
            <p className="text-sm text-gray-400">No buckets found on the source.</p>
          ) : (
            <div className="grid grid-cols-2 md:grid-cols-3 gap-2 mb-4">
              {buckets.map(b => (
                <label key={b} className="flex items-center gap-2 px-3 py-2 rounded-lg border border-gray-200 dark:border-gray-700 cursor-pointer hover:bg-gray-50 dark:hover:bg-gray-700/30">
                  <input type="checkbox" checked={selected.has(b)} onChange={() => toggle(b)} className="rounded" />
                  <span className="text-sm text-gray-700 dark:text-gray-300 truncate">{b}</span>
                </label>
              ))}
            </div>
          )}
          <button onClick={handleStart} disabled={starting || selected.size === 0}
            className="px-5 py-2.5 rounded-lg bg-emerald-600 hover:bg-emerald-700 disabled:bg-emerald-400 text-white text-sm font-medium transition-colors">
            {starting ? 'Starting...' : `Migrate ${selected.size} bucket${selected.size !== 1 ? 's' : ''}`}
          </button>
        </div>
      )}

      {/* Jobs */}
      {jobs.length > 0 && (
        <div className="bg-white dark:bg-gray-800 rounded-xl border border-gray-200 dark:border-gray-700 overflow-hidden">
          <div className="px-5 py-3 border-b border-gray-200 dark:border-gray-700">
            <h3 className="text-sm font-semibold text-gray-900 dark:text-white">Migrations</h3>
          </div>
          <div className="divide-y divide-gray-100 dark:divide-gray-700/50">
            {jobs.map(job => {
              const pct = job.total > 0 ? Math.round(((job.copied + job.failed) / job.total) * 100) : (job.status === 'completed' ? 100 : 0)
              const color = job.status === 'failed' ? 'bg-red-500' : job.status === 'completed' ? 'bg-emerald-500' : 'bg-indigo-500'
              return (
                <div key={job.id} className="px-5 py-4">
                  <div className="flex items-center justify-between mb-2">
                    <div className="text-sm text-gray-700 dark:text-gray-300 font-mono truncate">{job.endpoint}</div>
                    <span className={`text-xs px-2 py-0.5 rounded-full ${
                      job.status === 'completed' ? 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-400'
                      : job.status === 'failed' ? 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-400'
                      : 'bg-indigo-100 text-indigo-700 dark:bg-indigo-900/30 dark:text-indigo-400'}`}>
                      {job.status}
                    </span>
                  </div>
                  <div className="w-full h-2 rounded-full bg-gray-200 dark:bg-gray-700 overflow-hidden mb-1.5">
                    <div className={`h-full ${color} transition-all`} style={{ width: `${pct}%` }} />
                  </div>
                  <div className="flex items-center justify-between text-xs text-gray-500 dark:text-gray-400">
                    <span>{job.copied} copied{job.failed > 0 ? ` · ${job.failed} failed` : ''}{job.total > 0 ? ` / ${job.total}` : ''}</span>
                    <span>{job.buckets.length} bucket{job.buckets.length !== 1 ? 's' : ''}</span>
                  </div>
                  {job.error && <div className="mt-1.5 text-xs text-red-500">{job.error}</div>}
                </div>
              )
            })}
          </div>
        </div>
      )}
    </div>
  )
}
