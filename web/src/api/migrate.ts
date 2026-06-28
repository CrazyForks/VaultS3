import { apiFetch } from './client'

export interface MigrateJob {
  id: string
  endpoint: string
  buckets: string[]
  status: string
  total: number
  copied: number
  failed: number
  error?: string
  started_at: number
  finished_at?: number
}

export interface MigrateSource {
  endpoint: string
  accessKey: string
  secretKey: string
  region?: string
  buckets?: string[]
}

export function testMigrateSource(s: MigrateSource): Promise<{ buckets: string[] }> {
  return apiFetch('/migrate/test', { method: 'POST', body: JSON.stringify(s) })
}

export function startMigration(s: MigrateSource): Promise<{ jobId: string }> {
  return apiFetch('/migrate', { method: 'POST', body: JSON.stringify(s) })
}

export function listMigrateJobs(): Promise<MigrateJob[]> {
  return apiFetch('/migrate/jobs')
}

export function cancelMigration(jobId: string): Promise<{ status: string }> {
  return apiFetch('/migrate/cancel', { method: 'POST', body: JSON.stringify({ jobId }) })
}
