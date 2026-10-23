import { apiClient } from '../client'

export interface KiroAuthorizationResult {
  status: 'pending' | 'completed' | 'denied' | 'expired'
  session_id?: string
  auth_url?: string
  user_code?: string
  expires_at?: number
  retry_after?: number
  credentials?: Record<string, unknown>
}

export async function startKiroAuthorization(payload: { auth_region: string; start_url?: string; proxy_id?: number }): Promise<KiroAuthorizationResult> {
  const { data } = await apiClient.post<KiroAuthorizationResult>('/admin/kiro/oauth/auth-url', payload, { timeout: 60_000 })
  return data
}

export async function checkKiroAuthorization(sessionId: string): Promise<KiroAuthorizationResult> {
  const { data } = await apiClient.post<KiroAuthorizationResult>('/admin/kiro/oauth/check', { session_id: sessionId }, { timeout: 30_000 })
  return data
}
