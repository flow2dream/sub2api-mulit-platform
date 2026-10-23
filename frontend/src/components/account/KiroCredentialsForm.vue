<template>
  <div class="space-y-4" data-testid="kiro-credentials-form">
    <div>
      <label class="input-label">{{ t('admin.accounts.oauth.authMethod') }} <span class="text-red-500">（{{ t('common.required') }}）</span></label>
      <div class="mt-2 flex flex-wrap gap-4">
        <label v-if="accountType !== 'apikey'" class="flex cursor-pointer items-center gap-2 text-sm">
          <input v-model="inputMode" type="radio" value="manual" :name="formId + '-mode'" data-testid="kiro-mode-manual" />
          {{ t('admin.accounts.oauth.manualAuth') }}
        </label>
        <label class="flex cursor-pointer items-center gap-2 text-sm">
          <input v-model="inputMode" type="radio" value="token" :name="formId + '-mode'" data-testid="kiro-mode-token" />
          {{ t('admin.accounts.kiro.tokenMode') }}
        </label>
        <label class="flex cursor-pointer items-center gap-2 text-sm">
          <input v-model="inputMode" type="radio" value="json" :name="formId + '-mode'" data-testid="kiro-mode-json" />
          {{ t('admin.accounts.kiro.jsonMode') }}
        </label>
      </div>
    </div>
    <div v-if="inputMode === 'manual'" class="space-y-4 rounded-lg border border-blue-200 bg-blue-50 p-4 dark:border-blue-800 dark:bg-blue-900/20" data-testid="kiro-manual-authorization">
      <p class="text-sm text-blue-900 dark:text-blue-200">{{ t('admin.accounts.kiro.manualHint') }}</p>
      <ol class="list-decimal space-y-1 pl-5 text-sm text-blue-900 dark:text-blue-200">
        <li>{{ t('admin.accounts.kiro.loginStep1') }}</li>
        <li>{{ t('admin.accounts.kiro.loginStep2') }}</li>
        <li>{{ t('admin.accounts.kiro.loginStep3') }}</li>
      </ol>
      <label class="block">
        <span class="input-label">{{ t('admin.accounts.kiro.loginProvider') }} <span class="text-red-500">（{{ t('common.required') }}）</span></span>
        <select v-model="loginProvider" class="input" :disabled="busy">
          <option value="builder">AWS Builder ID</option>
          <option value="enterprise">AWS IAM Identity Center</option>
        </select>
      </label>
      <label v-if="loginProvider === 'enterprise'" class="block">
        <span class="input-label">{{ t('admin.accounts.kiro.startUrl') }} <span class="text-red-500">（{{ t('common.required') }}）</span></span>
        <input v-model="startURL" type="url" class="input" aria-required="true" autocomplete="off" placeholder="https://your-portal.awsapps.com/start" data-testid="kiro-start-url" />
      </label>
      <button type="button" class="btn btn-primary" :disabled="busy" data-testid="kiro-generate-url" @click="generateLogin">{{ busy ? t('common.loading') : t('admin.accounts.kiro.generateUrl') }}</button>
      <div v-if="authURL" class="space-y-3">
        <label class="block">
          <span class="input-label">{{ t('admin.accounts.kiro.loginUrl') }}（{{ t('admin.accounts.kiro.generatedField') }}）</span>
          <input :value="authURL" readonly class="input font-mono text-xs" data-testid="kiro-login-url" />
        </label>
        <p class="text-sm">{{ t('admin.accounts.kiro.userCode') }} <strong class="font-mono">{{ userCode }}</strong></p>
        <p class="text-sm">{{ t('admin.accounts.kiro.userCodeHint') }}</p>
        <div class="flex flex-wrap gap-3">
          <a :href="authURL" target="_blank" rel="noopener noreferrer" class="btn btn-secondary">{{ t('admin.accounts.kiro.openLogin') }}</a>
          <button type="button" class="btn btn-primary" :disabled="busy || !!authorizedCredentials" data-testid="kiro-check-authorization" @click="checkLogin">{{ t('admin.accounts.kiro.checkLogin') }}</button>
        </div>
      </div>
      <p v-if="statusMessage" role="status" class="text-sm" data-testid="kiro-login-status">{{ statusMessage }}</p>
      <p v-if="loginError" role="alert" class="text-sm text-red-600 dark:text-red-400">{{ loginError }}</p>
    </div>
    <template v-else>
      <p class="input-hint">{{ t('admin.accounts.kiro.blankTokensHint') }}</p>
      <div v-if="inputMode === 'json'">
        <label :for="formId + '-json'" class="input-label">{{ t('admin.accounts.kiro.importJson') }} <span class="text-red-500">（{{ t('common.required') }}）</span></label>
        <textarea :id="formId + '-json'" v-model="importJson" rows="4" class="input font-mono" aria-required="true" autocomplete="off" spellcheck="false" data-testid="kiro-import-json" :placeholder="t('admin.accounts.kiro.importPlaceholder')" />
      </div>
      <div v-else class="space-y-4">
        <label class="block">
          <span class="input-label">{{ t('admin.accounts.kiro.authMethod') }} <span class="text-red-500">（{{ t('common.required') }}）</span></span>
          <select v-model="authMethod" class="input" data-testid="kiro-token-auth-method">
            <option v-if="accountType !== 'apikey'" value="social">Social (Google / GitHub)</option>
            <option v-if="accountType !== 'apikey'" value="idc">AWS OIDC / Identity Center</option>
            <option v-if="accountType !== 'oauth'" value="api_key">Kiro API Key</option>
          </select>
        </label>
        <div v-for="field in visibleFields" :key="field.key">
          <label :for="formId + '-' + field.key" class="input-label">{{ field.label }} <span :class="isFieldRequired(field.key) ? 'text-red-500' : 'text-gray-500'">（{{ isFieldRequired(field.key) ? t('common.required') : t('admin.accounts.kiro.optionalField') }}）</span></label>
          <input :id="formId + '-' + field.key" v-model="fields[field.key]" :name="formId + '-' + field.key" :data-testid="'kiro-' + field.key" :aria-required="isFieldRequired(field.key)" :type="field.secret ? 'password' : 'text'" class="input" :autocomplete="field.secret ? 'new-password' : 'off'" data-lpignore="true" data-1p-ignore="true" spellcheck="false" :placeholder="field.secret && editing && !reauthorizing ? t('admin.accounts.kiro.keepSecret') : ''" />
          <p v-if="field.secret && editing && !isFieldRequired(field.key)" class="input-hint">{{ t('admin.accounts.kiro.keepSecret') }}</p>
          <p v-else-if="field.key === 'access_token'" class="input-hint">{{ t('admin.accounts.kiro.accessTokenOptionalHint') }}</p>
          <p v-else-if="field.key === 'profile_arn'" class="input-hint">{{ t('admin.accounts.kiro.profileHint') }}</p>
        </div>
      </div>
    </template>
    <p v-if="accountType === 'apikey'" class="input-hint">{{ t('admin.accounts.kiro.apiKeyHint') }}</p>
    <div class="grid grid-cols-2 gap-3">
      <label><span class="input-label">{{ t('admin.accounts.kiro.authRegion') }}（{{ t('admin.accounts.kiro.optionalField') }}）</span><input v-model="authRegion" class="input" data-testid="kiro-auth-region" /><span class="input-hint">{{ t('admin.accounts.kiro.authRegionOptionalHint') }}</span></label>
      <label><span class="input-label">{{ t('admin.accounts.kiro.apiRegion') }}（{{ t('admin.accounts.kiro.optionalField') }}）</span><input v-model="apiRegion" class="input" /><span class="input-hint">{{ t('admin.accounts.kiro.apiRegionOptionalHint') }}</span></label>
    </div>
    <label class="block">
      <span class="input-label">{{ t('admin.accounts.kiro.endpoint') }}（{{ t('admin.accounts.kiro.defaultSelection') }}）</span>
      <select v-model="endpoint" class="input" :disabled="accountType === 'apikey' || authMethod === 'api_key'">
        <option value="q">{{ t('admin.accounts.kiro.ideEndpoint') }}</option><option value="codewhisperer">CodeWhisperer</option><option value="runtime">Kiro runtime</option>
      </select>
      <p v-if="endpoint === 'q'" class="input-hint">{{ t('admin.accounts.kiro.ideEndpointHint') }}</p>
      <p v-if="endpoint === 'runtime' && authMethod !== 'api_key'" class="input-hint">{{ t('admin.accounts.kiro.profileHint') }}</p>
    </label>
    <div>
      <label :for="formId + '-mapping'" class="input-label">{{ t('admin.accounts.kiro.modelMapping') }}（{{ t('admin.accounts.kiro.optionalField') }}）</label>
      <textarea :id="formId + '-mapping'" v-model="modelMapping" rows="3" class="input font-mono" spellcheck="false" data-testid="kiro-model-mapping" :placeholder="t('admin.accounts.kiro.modelMappingHint')" />
      <p class="input-hint">{{ t('admin.accounts.kiro.modelMappingHint') }}</p>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, reactive, ref, useId, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { normalizeKiroCredentials } from '@/utils/kiroCredentials'
import { startKiroAuthorization, checkKiroAuthorization } from '@/api/admin/kiro'
import { extractApiErrorMessage } from '@/utils/apiError'

const props = defineProps<{ credentials?: Record<string, unknown>; accountType?: string; editing?: boolean; reauthorizing?: boolean; proxyId?: number | null }>()
const { t } = useI18n()
const formId = useId()
// Stored secrets must never become hidden defaults or populate token inputs.
const initial = normalizeKiroCredentials(props.credentials || {})
for (const key of ['access_token', 'refresh_token', 'api_key', 'client_secret', 'expires_at']) delete initial[key]
const inputMode = ref(props.accountType === 'apikey' || (props.editing && !props.reauthorizing) ? 'token' : 'manual')
const importJson = ref('')
const authMethod = ref(String(initial.auth_method || (props.accountType === 'apikey' ? 'api_key' : 'social')))
const authRegion = ref(String(initial.auth_region || 'us-east-1'))
const apiRegion = ref(String(initial.api_region || 'us-east-1'))
const endpoint = ref(String(initial.endpoint_family || (authMethod.value === 'api_key' ? 'runtime' : 'q')))
const modelMapping = ref(initial.model_mapping ? JSON.stringify(initial.model_mapping, null, 2) : '')
const fields = reactive<Record<string, string>>({ access_token: '', refresh_token: '', api_key: '', client_id: String(initial.client_id || ''), client_secret: '', profile_arn: String(initial.profile_arn || '') })
function isFieldRequired(key: string): boolean {
  if (key === 'client_id') return true
  if (key === 'client_secret') return !props.editing
  return (key === 'refresh_token' || key === 'api_key') && (!props.editing || !!props.reauthorizing)
}
const visibleFields = computed(() => authMethod.value === 'api_key'
  ? [{ key: 'api_key', label: 'Kiro API Key', secret: true }]
  : [
      { key: 'refresh_token', label: 'Refresh Token', secret: true },
      { key: 'access_token', label: 'Access Token', secret: true },
      ...(authMethod.value === 'idc' ? [{ key: 'client_id', label: 'Client ID', secret: false }, { key: 'client_secret', label: 'Client Secret', secret: true }] : []),
      { key: 'profile_arn', label: 'Profile ARN', secret: false }
    ])
watch(authMethod, (method) => { if (method === 'api_key') endpoint.value = 'runtime' })

const loginProvider = ref('builder')
const startURL = ref('')
const authURL = ref('')
const userCode = ref('')
const sessionID = ref('')
const busy = ref(false)
const statusMessage = ref('')
const loginError = ref('')
const authorizedCredentials = ref<Record<string, unknown> | null>(null)
let generation = 0
let pollTimer: ReturnType<typeof setTimeout> | undefined
let checkRequest: Promise<void> | null = null
let expiresAt = 0
function stopPolling() {
  if (pollTimer !== undefined) clearTimeout(pollTimer)
  pollTimer = undefined
}
function scheduleCheck(seconds: number) {
  stopPolling()
  if (!sessionID.value || authorizedCredentials.value) return
  pollTimer = setTimeout(() => { void checkLogin() }, Math.max(1, Math.min(60, seconds || 5)) * 1000)
}
function resetLogin() {
  stopPolling()
  generation++
  checkRequest = null
  expiresAt = 0
  authURL.value = userCode.value = sessionID.value = statusMessage.value = loginError.value = ''
  busy.value = false
  authorizedCredentials.value = null
}
watch([inputMode, authRegion, loginProvider, startURL, () => props.proxyId], resetLogin)
onBeforeUnmount(resetLogin)

async function generateLogin() {
  resetLogin()
  if (loginProvider.value === 'enterprise' && !startURL.value.trim()) { loginError.value = t('admin.accounts.kiro.portalRequired'); return }
  const current = generation
  busy.value = true
  try {
    const result = await startKiroAuthorization({ auth_region: authRegion.value.trim(), ...(props.proxyId ? { proxy_id: props.proxyId } : {}), ...(loginProvider.value === 'enterprise' ? { start_url: startURL.value.trim() } : {}) })
    if (current !== generation) return
    if (!result.session_id || !result.auth_url || !result.user_code) throw new Error(t('admin.accounts.kiro.invalidLoginResponse'))
    sessionID.value = result.session_id
    authURL.value = result.auth_url
    userCode.value = result.user_code
    expiresAt = result.expires_at || Math.floor(Date.now() / 1000) + 600
    statusMessage.value = t('admin.accounts.kiro.loginPending', { seconds: result.retry_after || 5 })
    scheduleCheck(result.retry_after || 5)
  } catch (error) {
    if (current === generation) loginError.value = extractApiErrorMessage(error, t('admin.accounts.kiro.loginFailed'))
  } finally { if (current === generation) busy.value = false }
}

function checkLogin(): Promise<void> {
  if (checkRequest) return checkRequest
  if (!sessionID.value || busy.value || authorizedCredentials.value) return Promise.resolve()
  stopPolling()
  if (expiresAt && Date.now() >= expiresAt * 1000) {
    resetLogin()
    loginError.value = t('admin.accounts.kiro.loginExpired')
    return Promise.resolve()
  }
  const current = generation
  busy.value = true
  loginError.value = ''
  checkRequest = (async () => {
    try {
      const result = await checkKiroAuthorization(sessionID.value)
      if (current !== generation) return
      if (result.status === 'completed') {
        const credentials = normalizeKiroCredentials(result.credentials || {})
        if (credentials.auth_method !== 'idc' || !credentials.refresh_token || !credentials.access_token || !credentials.client_id || !credentials.client_secret) throw new Error(t('admin.accounts.kiro.invalidLoginResponse'))
        authorizedCredentials.value = credentials
        statusMessage.value = t('admin.accounts.kiro.loginCompleted')
      } else if (result.status === 'pending') {
        statusMessage.value = t('admin.accounts.kiro.loginPending', { seconds: result.retry_after || 5 })
        scheduleCheck(result.retry_after || 5)
      } else if (result.status === 'denied' || result.status === 'expired') {
        resetLogin()
        loginError.value = t(result.status === 'denied' ? 'admin.accounts.kiro.loginDenied' : 'admin.accounts.kiro.loginExpired')
      } else {
        throw new Error(t('admin.accounts.kiro.invalidLoginResponse'))
      }
    } catch (error) {
      if (current === generation) {
        loginError.value = extractApiErrorMessage(error, t('admin.accounts.kiro.loginFailed'))
        scheduleCheck(10)
      }
    } finally {
      if (current === generation) { busy.value = false; checkRequest = null }
    }
  })()
  return checkRequest
}

async function prepareCredentials(): Promise<Record<string, unknown>> {
  const current = generation
  if (inputMode.value === 'manual' && !authorizedCredentials.value) {
    if (!sessionID.value) throw new Error(loginError.value || t('admin.accounts.kiro.completeLoginFirst'))
    await checkLogin()
    if (current !== generation || !authorizedCredentials.value) {
      throw new Error(loginError.value || t('admin.accounts.kiro.authorizationPending'))
    }
  }
  return buildCredentials()
}

function buildCredentials(): Record<string, unknown> {
  const base: Record<string, unknown> = { ...initial, auth_method: authMethod.value, auth_region: authRegion.value.trim(), api_region: apiRegion.value.trim(), endpoint_family: authMethod.value === 'api_key' ? 'runtime' : endpoint.value }
  let entered: Record<string, unknown> = {}
  if (inputMode.value === 'manual') {
    if (!authorizedCredentials.value) throw new Error(t(sessionID.value ? 'admin.accounts.kiro.authorizationPending' : 'admin.accounts.kiro.completeLoginFirst'))
    entered = authorizedCredentials.value
  } else if (inputMode.value === 'json') {
    let value: unknown
    try { value = JSON.parse(importJson.value) } catch { throw new Error(t('admin.accounts.kiro.invalidJson')) }
    if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error(t('admin.accounts.kiro.invalidJson'))
    entered = normalizeKiroCredentials(value as Record<string, unknown>)
    if (!('auth_method' in entered)) entered.auth_method = entered.api_key ? 'api_key' : entered.client_id ? 'idc' : authMethod.value
    if (entered.auth_method === 'api_key' && !entered.endpoint_family) entered.endpoint_family = 'runtime'
  } else {
    for (const [key, value] of Object.entries(fields)) { if (value.trim()) entered[key] = value.trim() }
  }
  if (modelMapping.value.trim()) {
    let mapping: unknown
    try { mapping = JSON.parse(modelMapping.value) } catch { throw new Error(t('admin.accounts.kiro.invalidMapping')) }
    if (!mapping || Array.isArray(mapping) || typeof mapping !== 'object' || Object.entries(mapping).some(([key, value]) => !key.trim() || typeof value !== 'string' || !value.trim())) throw new Error(t('admin.accounts.kiro.invalidMapping'))
    base.model_mapping = mapping
  } else if (props.editing) { delete base.model_mapping }
  const result = { ...base, ...entered }
  if (props.accountType && (result.auth_method === 'api_key') !== (props.accountType === 'apikey')) throw new Error(t('admin.accounts.kiro.typeMismatch'))
  const requireNew = !props.editing || props.reauthorizing
  if (requireNew && (result.auth_method === 'api_key' ? !result.api_key : !result.refresh_token)) throw new Error(t('admin.accounts.kiro.credentialsRequired'))
  if (result.auth_method === 'idc' && (!result.client_id || (!result.client_secret && !props.editing))) throw new Error(t('admin.accounts.kiro.oidcRequired'))
  return result
}

defineExpose({ buildCredentials, prepareCredentials })
</script>
