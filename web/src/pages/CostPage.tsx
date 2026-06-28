import { useState, useEffect, useMemo } from 'react'
import { getTCO, type TcoProvider } from '../api/tco'

function usd(n: number): string {
  return '$' + n.toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 })
}

function formatSize(gb: number): string {
  if (gb >= 1024) return `${(gb / 1024).toFixed(2)} TB`
  if (gb > 0 && gb < 1) return `${(gb * 1024).toFixed(1)} MB`
  return `${gb.toFixed(2)} GB`
}

const PRESETS = [
  { label: '1 TB', gb: 1024 },
  { label: '10 TB', gb: 10 * 1024 },
  { label: '100 TB', gb: 100 * 1024 },
  { label: '1 PB', gb: 1024 * 1024 },
]

export default function CostPage() {
  const [rates, setRates] = useState<TcoProvider[]>([])
  const [liveGb, setLiveGb] = useState(0)
  const [storageGb, setStorageGb] = useState(0)
  const [egressGb, setEgressGb] = useState(0)
  const [error, setError] = useState('')
  const [loaded, setLoaded] = useState(false)

  useEffect(() => {
    getTCO()
      .then(d => {
        setRates(d.providers) // per-provider rates from the server (single source of truth)
        setLiveGb(d.storageGb)
        // Default to your live data; if it's tiny, start at 1 TB so the numbers are meaningful.
        const start = d.storageGb >= 1 ? d.storageGb : 1024
        setStorageGb(start)
        setEgressGb(start)
        setLoaded(true)
      })
      .catch(err => setError(err instanceof Error ? err.message : 'Failed to load'))
  }, [])

  const rows = useMemo(
    () =>
      rates.map(p => {
        const storageCost = storageGb * p.storageRatePerGb
        const egressCost = egressGb * p.egressRatePerGb
        return { name: p.name, storageCost, egressCost, monthly: storageCost + egressCost }
      }),
    [rates, storageGb, egressGb],
  )

  const maxMonthly = useMemo(() => rows.reduce((m, r) => Math.max(m, r.monthly), 0), [rows])

  if (error) {
    return <div className="p-3 rounded-lg bg-red-50 dark:bg-red-900/20 text-red-700 dark:text-red-400 text-sm">{error}</div>
  }
  if (!loaded) {
    return <div className="flex items-center justify-center h-64"><div className="animate-spin rounded-full h-8 w-8 border-b-2 border-indigo-600" /></div>
  }

  const setPreset = (gb: number) => { setStorageGb(gb); setEgressGb(gb) }
  const inputClass = 'mt-1 w-full px-3 py-2 rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-900 text-gray-900 dark:text-white text-sm'

  return (
    <div>
      <div className="mb-6">
        <h2 className="text-xl font-semibold text-gray-900 dark:text-white">Cost Estimator</h2>
        <p className="text-sm text-gray-500 dark:text-gray-400 mt-0.5">
          What your data would cost on managed clouds vs. self-hosting with VaultS3. Adjust the numbers to model any scale.
        </p>
      </div>

      {/* Presets */}
      <div className="flex flex-wrap items-center gap-2 mb-4">
        <span className="text-xs text-gray-500 dark:text-gray-400 mr-1">Quick scenario:</span>
        {liveGb >= 0.001 && (
          <button onClick={() => setPreset(liveGb)}
            className="px-3 py-1 rounded-full text-xs font-medium bg-emerald-50 dark:bg-emerald-900/30 text-emerald-700 dark:text-emerald-400 hover:bg-emerald-100">
            Your data ({formatSize(liveGb)})
          </button>
        )}
        {PRESETS.map(p => (
          <button key={p.label} onClick={() => setPreset(p.gb)}
            className="px-3 py-1 rounded-full text-xs font-medium bg-gray-100 dark:bg-gray-700 text-gray-700 dark:text-gray-300 hover:bg-gray-200 dark:hover:bg-gray-600">
            {p.label}
          </button>
        ))}
      </div>

      {/* Inputs */}
      <div className="grid grid-cols-1 sm:grid-cols-2 gap-4 mb-6">
        <div className="bg-white dark:bg-gray-800 rounded-xl border border-gray-200 dark:border-gray-700 p-4">
          <label className="text-xs text-gray-500 dark:text-gray-400">Stored data (GB)</label>
          <input type="number" min={0} value={storageGb}
            onChange={e => setStorageGb(Math.max(0, Number(e.target.value)))} className={inputClass} />
          <div className="text-xs text-gray-400 mt-1">Your live size: {formatSize(liveGb)}</div>
        </div>
        <div className="bg-white dark:bg-gray-800 rounded-xl border border-gray-200 dark:border-gray-700 p-4">
          <label className="text-xs text-gray-500 dark:text-gray-400">Egress per month (GB)</label>
          <input type="number" min={0} value={egressGb}
            onChange={e => setEgressGb(Math.max(0, Number(e.target.value)))} className={inputClass} />
          <div className="text-xs text-gray-400 mt-1">How much you serve/download per month</div>
        </div>
      </div>

      {/* Comparison */}
      <div className="bg-white dark:bg-gray-800 rounded-xl border border-gray-200 dark:border-gray-700 overflow-hidden">
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b border-gray-200 dark:border-gray-700 text-left">
              <th className="px-4 py-3 text-xs font-medium text-gray-500 dark:text-gray-400 uppercase tracking-wider">Provider</th>
              <th className="px-4 py-3 text-xs font-medium text-gray-500 dark:text-gray-400 uppercase tracking-wider text-right">Storage / mo</th>
              <th className="px-4 py-3 text-xs font-medium text-gray-500 dark:text-gray-400 uppercase tracking-wider text-right">Egress / mo</th>
              <th className="px-4 py-3 text-xs font-medium text-gray-500 dark:text-gray-400 uppercase tracking-wider text-right">Monthly</th>
              <th className="px-4 py-3 text-xs font-medium text-gray-500 dark:text-gray-400 uppercase tracking-wider text-right">Per year</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-gray-100 dark:divide-gray-700/50">
            <tr className="bg-emerald-50 dark:bg-emerald-900/20">
              <td className="px-4 py-3 font-semibold text-emerald-700 dark:text-emerald-400">VaultS3 (self-hosted)</td>
              <td className="px-4 py-3 text-right text-emerald-700 dark:text-emerald-400">{usd(0)}</td>
              <td className="px-4 py-3 text-right text-emerald-700 dark:text-emerald-400">{usd(0)} <span className="text-xs">(egress-free)</span></td>
              <td className="px-4 py-3 text-right font-semibold text-emerald-700 dark:text-emerald-400">{usd(0)}</td>
              <td className="px-4 py-3 text-right font-semibold text-emerald-700 dark:text-emerald-400">{usd(0)}</td>
            </tr>
            {rows.map(r => (
              <tr key={r.name}>
                <td className="px-4 py-3 font-medium text-gray-900 dark:text-white">{r.name}</td>
                <td className="px-4 py-3 text-right text-gray-500 dark:text-gray-400">{usd(r.storageCost)}</td>
                <td className="px-4 py-3 text-right text-gray-500 dark:text-gray-400">{usd(r.egressCost)}</td>
                <td className="px-4 py-3 text-right font-semibold text-gray-900 dark:text-white">{usd(r.monthly)}</td>
                <td className="px-4 py-3 text-right text-gray-700 dark:text-gray-300">{usd(r.monthly * 12)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      {/* Savings callout */}
      {maxMonthly > 0 && (
        <div className="mt-4 p-4 rounded-xl bg-indigo-600 text-white">
          <div className="text-sm">
            Self-hosting VaultS3 saves up to <strong>{usd(maxMonthly)}/mo</strong> (<strong>{usd(maxMonthly * 12)}/yr</strong>)
            versus the priciest managed option above — with no egress fees, ever.
          </div>
        </div>
      )}

      <p className="mt-4 text-xs text-gray-400 dark:text-gray-500">
        Estimated monthly cost at public list prices (mid-2026); actual pricing varies by region, storage class, and committed volume. Self-hosted VaultS3 is egress-free ($0).
      </p>
    </div>
  )
}
