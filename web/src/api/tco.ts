import { apiFetch } from './client'

export interface TcoProvider {
  name: string
  storageRatePerGb: number
  egressRatePerGb: number
  storageCost: number
  egressCost: number
  monthlyTotal: number
}

export interface Tco {
  storageBytes: number
  storageGb: number
  egressGb: number
  providers: TcoProvider[]
  note: string
}

export function getTCO(egressGb?: number): Promise<Tco> {
  const q = egressGb !== undefined ? `?egress_gb=${egressGb}` : ''
  return apiFetch<Tco>(`/tco${q}`)
}
