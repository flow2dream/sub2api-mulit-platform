import { onScopeDispose, ref, watch } from 'vue'

export interface KiroTestEvent {
  code?: string
  data?: { retry_after_seconds?: number }
}

// Both account test dialogs consume the same sanitized server error codes.
export function useKiroTestFeedback(accountId: () => number | undefined) {
  const retrySeconds = ref(0)
  let timer: ReturnType<typeof setInterval> | undefined
  function clearCooldown() {
    if (timer !== undefined) clearInterval(timer)
    timer = undefined
    retrySeconds.value = 0
  }
  watch(accountId, clearCooldown)
  onScopeDispose(clearCooldown)

  function feedbackKey(event: KiroTestEvent): string | undefined {
    const keys: Record<string, string> = {
      kiro_connecting: 'connecting',
      kiro_throttled: 'throttled',
      kiro_cooldown: 'cooldown',
      kiro_test_busy: 'testBusy',
      kiro_payment_required: 'paymentRequired',
      kiro_unauthorized: 'unauthorized',
      kiro_forbidden: 'forbidden',
      kiro_invalid_model: 'invalidModel',
      kiro_auto_endpoint: 'autoEndpoint',
      kiro_runtime_profile: 'runtimeProfile',
      kiro_unavailable: 'unavailable'
    }
    const seconds = event.data?.retry_after_seconds
    if (event.code?.startsWith('kiro_') && typeof seconds === 'number' && Number.isFinite(seconds) && seconds > 0) {
      clearCooldown()
      const until = Date.now() + Math.min(seconds, 86400) * 1000
      retrySeconds.value = Math.ceil((until - Date.now()) / 1000)
      timer = setInterval(() => {
        retrySeconds.value = Math.max(0, Math.ceil((until - Date.now()) / 1000))
        if (retrySeconds.value === 0) clearCooldown()
      }, 250)
    }
    const key = event.code && keys[event.code]
    return key ? `admin.accounts.kiro.${key}` : undefined
  }

  return { retrySeconds, feedbackKey }
}
