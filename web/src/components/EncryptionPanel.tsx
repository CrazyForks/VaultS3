import { useState, useEffect, useCallback } from 'react'
import { useI18n } from '../i18n'
import { getBucketEncryption, bucketEncryptionAction, type BucketEncryption } from '../api/buckets'
import { useToast } from '../hooks/useToast'

const btn =
  'px-3 py-1.5 text-sm rounded-lg border border-gray-300 dark:border-gray-600 hover:bg-gray-100 dark:hover:bg-gray-700 disabled:opacity-40 disabled:cursor-not-allowed transition-colors'

export default function EncryptionPanel({ bucket }: { bucket: string }) {
  const { t } = useI18n()
  const [enc, setEnc] = useState<BucketEncryption | null>(null)
  const [busy, setBusy] = useState(false)
  const [confirmShred, setConfirmShred] = useState(false)
  const { addToast } = useToast()

  const load = useCallback(() => {
    getBucketEncryption(bucket).then(setEnc).catch(() => setEnc(null))
  }, [bucket])
  useEffect(() => { load() }, [load])

  const act = async (action: 'enable' | 'rotate' | 'shred') => {
    setBusy(true)
    try {
      await bucketEncryptionAction(bucket, action)
      addToast('success', t('encryption.succeeded', { action }))
      setConfirmShred(false)
      load()
    } catch (e) {
      addToast('error', e instanceof Error ? e.message : t('encryption.failed', { action }))
    } finally {
      setBusy(false)
    }
  }

  if (!enc) return null

  return (
    <div className="bg-white dark:bg-gray-800 rounded-xl border border-gray-200 dark:border-gray-700 p-5 mb-4">
      <h3 className="text-sm font-semibold text-gray-900 dark:text-white mb-3">{t('encryption.encryption')}</h3>

      {!enc.available ? (
        <p className="text-sm text-gray-500 dark:text-gray-400">
          Per-bucket encryption is not configured on this server (set{' '}
          <code className="px-1 rounded bg-gray-100 dark:bg-gray-700">encryption.per_bucket</code>).
        </p>
      ) : enc.enabled ? (
        <div className="space-y-3">
          <p className="text-sm text-gray-700 dark:text-gray-300">
            <span className="inline-block w-2 h-2 rounded-full bg-green-500 mr-2 align-middle" />
            Encrypted with a per-bucket key — version {enc.keyVersion}
          </p>
          <div className="flex flex-wrap items-center gap-2">
            <button className={btn} onClick={() => act('rotate')} disabled={busy}>{t('encryption.rotateKey')}</button>
            {!confirmShred ? (
              <button
                className="px-3 py-1.5 text-sm rounded-lg border border-red-300 dark:border-red-800 text-red-600 dark:text-red-400 hover:bg-red-50 dark:hover:bg-red-900/20 transition-colors"
                onClick={() => setConfirmShred(true)}
                disabled={busy}
              >
                {t('encryption.shredKey')}
              </button>
            ) : (
              <span className="flex items-center gap-2 text-sm">
                <span className="text-red-600 dark:text-red-400">{t('encryption.irreversibleAllObjectsBecomeUnrecoverabl')}</span>
                <button
                  className="px-3 py-1.5 text-sm rounded-lg bg-red-600 text-white hover:bg-red-700 transition-colors"
                  onClick={() => act('shred')}
                  disabled={busy}
                >
                  {t('encryption.confirmShred')}
                </button>
                <button className={btn} onClick={() => setConfirmShred(false)} disabled={busy}>{t('encryption.cancel')}</button>
              </span>
            )}
          </div>
        </div>
      ) : (
        <div className="space-y-3">
          <p className="text-sm text-gray-700 dark:text-gray-300">
            <span className="inline-block w-2 h-2 rounded-full bg-gray-400 mr-2 align-middle" />
            {t('encryption.notEncryptedObjectsAreStoredAs')}
          </p>
          <button className={btn} onClick={() => act('enable')} disabled={busy}>{t('encryption.enableEncryption')}</button>
        </div>
      )}
    </div>
  )
}
