import { inject, onScopeDispose, type InjectionKey, type Ref } from 'vue'
import { authFetch } from './api'

export interface CardAccount {
  id: number
  name: string
  api_key_configured: boolean
  api_key_hint: string
  webhook_secret_configured: boolean
  webhook_secret_hint: string
}
export const accountBusyKey: InjectionKey<Ref<number>> = Symbol('card-account-busy')
export const accountKey: InjectionKey<Ref<number>> = Symbol('card-account')

// Capture the page's account at mount, never read a mutable global selection
// after an await. AccountBoundary remounts the page when its selection changes.
export function useAccountFetch() {
  const selected = inject(accountKey)
  const accountId = selected?.value
  const busy = inject(accountBusyKey)
  if (!accountId) throw new Error('Card account context required')
  const controller = new AbortController()
  onScopeDispose(() => controller.abort())
  const fetchForAccount = async (input: RequestInfo | URL, init: RequestInit = {}) => {
    const headers = new Headers(init.headers)
    headers.set('X-Card-Account-ID', String(accountId))
    if (controller.signal.aborted) throw new DOMException('页面已离开', 'AbortError')
    const mutation = !['GET', 'HEAD'].includes((init.method || 'GET').toUpperCase())
    if (mutation && busy) busy.value++
    try { return await authFetch(input, { ...init, headers, signal: controller.signal }) }
    finally { if (mutation && busy) busy.value-- }
  }
  return { authFetch: fetchForAccount, accountId }
}
