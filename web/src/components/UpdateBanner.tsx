import { useState, useEffect } from 'react'
import { useI18n } from '../i18n'
import { getVersion, type VersionStatus } from '../api/version'

const DISMISS_KEY = 'vaults3_dismissed_update'

export default function UpdateBanner() {
  const { t } = useI18n()
  const [status, setStatus] = useState<VersionStatus | null>(null)
  const [dismissed, setDismissed] = useState(false)

  useEffect(() => {
    getVersion().then(setStatus).catch(() => setStatus(null))
  }, [])

  if (!status || !status.updateAvailable || dismissed) return null
  // Respect a per-version dismissal so the banner doesn't nag for the same release.
  if (localStorage.getItem(DISMISS_KEY) === status.latest) return null

  const dismiss = () => {
    if (status.latest) localStorage.setItem(DISMISS_KEY, status.latest)
    setDismissed(true)
  }

  return (
    <div className="flex items-center justify-between gap-3 px-4 py-2 bg-indigo-600 text-white text-sm">
      <div className="flex items-center gap-2">
        <svg className="w-4 h-4 flex-shrink-0" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
          <path strokeLinecap="round" strokeLinejoin="round" d="M12 8v4m0 4h.01M21 12a9 9 0 11-18 0 9 9 0 0118 0z" />
        </svg>
        <span>
          {t('updatebanner.newVersion')} <strong>{status.latest}</strong> {t('updatebanner.available', { current: status.current })}
        </span>
        <a
          href="https://github.com/Kodiqa-Solutions/VaultS3/releases/latest"
          target="_blank"
          rel="noopener noreferrer"
          className="underline underline-offset-2 hover:text-indigo-100"
        >
          {t('updatebanner.releaseNotes')}
        </a>
      </div>
      <button onClick={dismiss} className="text-indigo-100 hover:text-white" aria-label={t('updatebanner.dismiss')}>
        <svg className="w-4 h-4" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
          <path strokeLinecap="round" strokeLinejoin="round" d="M6 18L18 6M6 6l12 12" />
        </svg>
      </button>
    </div>
  )
}
