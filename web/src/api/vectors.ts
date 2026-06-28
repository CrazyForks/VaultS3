import { apiFetch } from './client'

export interface VectorMatch {
  bucket: string
  key: string
  score: number
}

export interface VectorStatus {
  enabled: boolean
  vectors?: number
}

export function getVectorStatus(): Promise<VectorStatus> {
  return apiFetch<VectorStatus>('/vectors/status')
}

export function queryVectors(query: string, topK = 20, bucket?: string): Promise<{ results: VectorMatch[] }> {
  return apiFetch<{ results: VectorMatch[] }>('/vectors/query', {
    method: 'POST',
    body: JSON.stringify({ query, topK, bucket: bucket || '' }),
  })
}
