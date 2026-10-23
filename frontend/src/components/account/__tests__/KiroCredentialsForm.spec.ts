import { mount, flushPromises } from '@vue/test-utils'
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import KiroCredentialsForm from '../KiroCredentialsForm.vue'
import { normalizeKiroCredentials } from '@/utils/kiroCredentials'

const loginMocks = vi.hoisted(() => ({ start: vi.fn(), check: vi.fn() }))
vi.mock('@/api/admin/kiro', () => ({ startKiroAuthorization: loginMocks.start, checkKiroAuthorization: loginMocks.check }))

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

describe('Kiro credentials', () => {
  beforeEach(() => vi.clearAllMocks())
  afterEach(() => vi.useRealTimers())

  it('imports kiro.rs IDE credentials without overriding an explicit endpoint', () => {
    const exported = { authMethod: 'builder-id', endpoint: 'ide', clientId: 'client', clientSecret: 'secret', refreshToken: 'refresh' }
    expect(normalizeKiroCredentials(exported)).toMatchObject({ auth_method: 'idc', endpoint_family: 'q', client_id: 'client', client_secret: 'secret', refresh_token: 'refresh' })
    expect(normalizeKiroCredentials({ ...exported, endpointFamily: 'runtime' })).toMatchObject({ endpoint_family: 'runtime' })
    expect(exported.authMethod).toBe('builder-id')
  })

  it('starts on manual authorization without reading or pre-filling any tokens', async () => {
    const wrapper = mount(KiroCredentialsForm, { props: { accountType: 'oauth', credentials: { accessToken: 'old-access', refreshToken: 'old-refresh' } } })
    expect(wrapper.get('[data-testid="kiro-mode-manual"]').element).toHaveProperty('checked', true)
    expect(loginMocks.start).not.toHaveBeenCalled()
    expect(() => wrapper.vm.buildCredentials()).toThrow('completeLoginFirst')
    await wrapper.get('[data-testid="kiro-mode-token"]').setValue(true)
    for (const key of ['access_token', 'refresh_token']) {
      const field = wrapper.get<HTMLInputElement>(`[data-testid="kiro-${key}"]`)
      expect(field.element.value).toBe('')
      expect(field.attributes('autocomplete')).toBe('new-password')
    }
    expect(() => wrapper.vm.buildCredentials()).toThrow('credentialsRequired')
    await wrapper.get('[data-testid="kiro-refresh_token"]').setValue('user-entered-refresh')
    expect(wrapper.vm.buildCredentials()).toMatchObject({ refresh_token: 'user-entered-refresh' })
    expect(wrapper.vm.buildCredentials()).not.toHaveProperty('access_token')
    wrapper.unmount()
  })

  it('generates a link using the selected proxy and only accepts credentials after authorization', async () => {
    loginMocks.start.mockResolvedValue({ status: 'pending', session_id: 'login-session', auth_url: 'https://view.awsapps.com/start/#/device', user_code: 'ABCD-EFGH' })
    loginMocks.check.mockResolvedValueOnce({ status: 'pending', retry_after: 5 }).mockResolvedValueOnce({ status: 'completed', credentials: { auth_method: 'idc', auth_region: 'us-west-2', access_token: 'granted-access', refresh_token: 'granted-refresh', client_id: 'granted-client', client_secret: 'granted-secret' } })
    const wrapper = mount(KiroCredentialsForm, { props: { accountType: 'oauth', proxyId: 7 } })
    await wrapper.get('[data-testid="kiro-auth-region"]').setValue('us-west-2')
    await wrapper.get('[data-testid="kiro-generate-url"]').trigger('click')
    await flushPromises()
    expect(loginMocks.start).toHaveBeenCalledWith({ auth_region: 'us-west-2', proxy_id: 7 })
    expect(wrapper.get<HTMLInputElement>('[data-testid="kiro-login-url"]').element.value).toContain('https://view.awsapps.com/')
    expect(() => wrapper.vm.buildCredentials()).toThrow('authorizationPending')
    await wrapper.get('[data-testid="kiro-check-authorization"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-testid="kiro-login-status"]').text()).toContain('loginPending')
    await wrapper.get('[data-testid="kiro-check-authorization"]').trigger('click')
    await flushPromises()
    expect(loginMocks.check).toHaveBeenCalledWith('login-session')
    expect(wrapper.vm.buildCredentials()).toMatchObject({ auth_method: 'idc', refresh_token: 'granted-refresh', client_secret: 'granted-secret' })
    await wrapper.get('[data-testid="kiro-mode-token"]').setValue(true)
    expect(wrapper.get<HTMLInputElement>('[data-testid="kiro-refresh_token"]').element.value).toBe('')
    expect(() => wrapper.vm.buildCredentials()).toThrow('credentialsRequired')
    wrapper.unmount()
  })

  it('automatically checks using the server interval and stops once authorized', async () => {
    vi.useFakeTimers()
    loginMocks.start.mockResolvedValue({ session_id: 'auto-session', auth_url: 'https://view.awsapps.com/start/', user_code: 'CODE', retry_after: 5 })
    loginMocks.check.mockResolvedValueOnce({ status: 'pending', retry_after: 10 }).mockResolvedValueOnce({ status: 'completed', credentials: { auth_method: 'idc', refresh_token: 'refresh', access_token: 'access', client_id: 'client', client_secret: 'secret' } })
    const wrapper = mount(KiroCredentialsForm, { props: { accountType: 'oauth' } })
    await wrapper.get('[data-testid="kiro-generate-url"]').trigger('click')
    await flushPromises()
    await vi.advanceTimersByTimeAsync(4999)
    expect(loginMocks.check).not.toHaveBeenCalled()
    await vi.advanceTimersByTimeAsync(1)
    expect(loginMocks.check).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(9999)
    expect(loginMocks.check).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(1)
    expect(wrapper.get('[data-testid="kiro-login-status"]').text()).toContain('loginCompleted')
    expect(wrapper.vm.buildCredentials()).toMatchObject({ refresh_token: 'refresh' })
    await vi.advanceTimersByTimeAsync(60000)
    expect(loginMocks.check).toHaveBeenCalledTimes(2)
    wrapper.unmount()
  })

  it('waits for an in-flight check on submit without consuming credentials twice', async () => {
    let resolve!: (value: unknown) => void
    loginMocks.start.mockResolvedValue({ session_id: 'submit-session', auth_url: 'https://view.awsapps.com/start/', user_code: 'CODE' })
    loginMocks.check.mockImplementation(() => new Promise(done => { resolve = done }))
    const wrapper = mount(KiroCredentialsForm, { props: { accountType: 'oauth' } })
    await wrapper.get('[data-testid="kiro-generate-url"]').trigger('click')
    await flushPromises()
    await wrapper.get('[data-testid="kiro-check-authorization"]').trigger('click')
    const preparing = wrapper.vm.prepareCredentials()
    expect(loginMocks.check).toHaveBeenCalledTimes(1)
    resolve({ status: 'completed', credentials: { auth_method: 'idc', refresh_token: 'refresh', access_token: 'access', client_id: 'client', client_secret: 'secret' } })
    await expect(preparing).resolves.toMatchObject({ refresh_token: 'refresh' })
    await expect(wrapper.vm.prepareCredentials()).resolves.toMatchObject({ refresh_token: 'refresh' })
    expect(loginMocks.check).toHaveBeenCalledTimes(1)
    wrapper.unmount()
  })

  it('reports pending authorization instead of asking for another link and cancels polling on unmount', async () => {
    vi.useFakeTimers()
    loginMocks.start.mockResolvedValue({ session_id: 'pending-session', auth_url: 'https://view.awsapps.com/start/', user_code: 'CODE' })
    loginMocks.check.mockResolvedValue({ status: 'pending', retry_after: 2 })
    const wrapper = mount(KiroCredentialsForm, { props: { accountType: 'oauth' } })
    await wrapper.get('[data-testid="kiro-generate-url"]').trigger('click')
    await flushPromises()
    await expect(wrapper.vm.prepareCredentials()).rejects.toThrow('authorizationPending')
    expect(() => wrapper.vm.buildCredentials()).toThrow('authorizationPending')
    wrapper.unmount()
    await vi.advanceTimersByTimeAsync(60000)
    expect(loginMocks.check).toHaveBeenCalledTimes(1)
  })

  it('stops checks after expiry and discards credentials received after changing modes', async () => {
    vi.useFakeTimers()
    loginMocks.start.mockResolvedValue({ session_id: 'expiring-session', auth_url: 'https://view.awsapps.com/start/', user_code: 'CODE', expires_at: Math.floor(Date.now() / 1000) + 3 })
    const wrapper = mount(KiroCredentialsForm, { props: { accountType: 'oauth' } })
    await wrapper.get('[data-testid="kiro-generate-url"]').trigger('click')
    await flushPromises()
    await vi.advanceTimersByTimeAsync(5000)
    expect(loginMocks.check).not.toHaveBeenCalled()
    expect(wrapper.get('[role="alert"]').text()).toContain('loginExpired')
    let resolve!: (value: unknown) => void
    loginMocks.start.mockResolvedValue({ session_id: 'new-session', auth_url: 'https://view.awsapps.com/start/', user_code: 'NEW' })
    loginMocks.check.mockImplementation(() => new Promise(done => { resolve = done }))
    await wrapper.get('[data-testid="kiro-generate-url"]').trigger('click')
    await flushPromises()
    const preparing = wrapper.vm.prepareCredentials()
    const rejected = expect(preparing).rejects.toThrow('authorizationPending')
    await wrapper.get('[data-testid="kiro-mode-token"]').setValue(true)
    resolve({ status: 'completed', credentials: { auth_method: 'idc', refresh_token: 'old-refresh', access_token: 'access', client_id: 'client', client_secret: 'secret' } })
    await rejected
    expect(() => wrapper.vm.buildCredentials()).toThrow('credentialsRequired')
    wrapper.unmount()
  })

  it('discards stale login responses after switching modes or proxies', async () => {
    let resolve!: (value: unknown) => void
    loginMocks.start.mockImplementation(() => new Promise(done => { resolve = done }))
    const wrapper = mount(KiroCredentialsForm, { props: { accountType: 'oauth' } })
    await wrapper.get('[data-testid="kiro-generate-url"]').trigger('click')
    await wrapper.setProps({ proxyId: 9 })
    resolve({ status: 'pending', session_id: 'stale', auth_url: 'https://view.awsapps.com/start/', user_code: 'OLD' })
    await flushPromises()
    expect(wrapper.find('[data-testid="kiro-login-url"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="kiro-generate-url"]').attributes('disabled')).toBeUndefined()
    wrapper.unmount()
  })

  it('shows denied or expired login states without accepting credentials', async () => {
    for (const status of ['denied', 'expired']) {
      loginMocks.start.mockResolvedValue({ session_id: 'session', auth_url: 'https://view.awsapps.com/start/', user_code: 'CODE' })
      loginMocks.check.mockResolvedValue({ status })
      const wrapper = mount(KiroCredentialsForm, { props: { accountType: 'oauth' } })
      await wrapper.get('[data-testid="kiro-generate-url"]').trigger('click')
      await flushPromises()
      await wrapper.get('[data-testid="kiro-check-authorization"]').trigger('click')
      await flushPromises()
      expect(wrapper.get('[role="alert"]').text()).toContain(status === 'denied' ? 'loginDenied' : 'loginExpired')
      expect(() => wrapper.vm.buildCredentials()).toThrow('completeLoginFirst')
      wrapper.unmount()
    }
  })

  it('keeps API keys and reauthorization tokens empty even when supplied by stored credentials', async () => {
    const key = mount(KiroCredentialsForm, { props: { accountType: 'apikey', credentials: { api_key: 'stored-key' } } })
    expect(key.find('[data-testid="kiro-mode-manual"]').exists()).toBe(false)
    expect(key.get<HTMLInputElement>('[data-testid="kiro-api_key"]').element.value).toBe('')
    expect(() => key.vm.buildCredentials()).toThrow('credentialsRequired')
    key.unmount()
    const reauth = mount(KiroCredentialsForm, { props: { accountType: 'oauth', editing: true, reauthorizing: true, credentials: { refresh_token: 'stored-refresh' } } })
    await reauth.get('[data-testid="kiro-mode-token"]').setValue(true)
    expect(() => reauth.vm.buildCredentials()).toThrow('credentialsRequired')
    reauth.unmount()
  })
  it('imports exported OIDC credentials and preserves the separate regions', async () => {
    const wrapper = mount(KiroCredentialsForm)
    await wrapper.get('[data-testid="kiro-mode-json"]').setValue(true)
    await wrapper.get('[data-testid="kiro-import-json"]').setValue(JSON.stringify({ authMethod: 'IdC', refreshToken: 'refresh', clientId: 'id', clientSecret: 'secret', authRegion: 'eu-west-1', profileArn: 'arn:aws:codewhisperer:us-west-2:123:profile/test' }))
    const result = wrapper.vm.buildCredentials()
    expect(result.auth_method).toBe('idc')
    expect(result.auth_region).toBe('eu-west-1')
    expect(result.api_region).toBe('us-west-2')
    expect(result.client_secret).toBe('secret')
    expect(result).not.toHaveProperty('refreshToken')
    wrapper.unmount()
  })

  it('never fills secret inputs when editing a redacted account', () => {
    const wrapper = mount(KiroCredentialsForm, { props: { editing: true, accountType: 'oauth', credentials: { auth_method: 'idc', client_id: 'id', auth_region: 'us-east-1', api_region: 'eu-west-1' } } })
    expect(wrapper.findAll('input[type="password"]').every(input => (input.element as HTMLInputElement).value === '')).toBe(true)
    const result = wrapper.vm.buildCredentials()
    expect(result).not.toHaveProperty('client_secret')
    expect(result).not.toHaveProperty('refresh_token')
    expect(result.client_id).toBe('id')
    wrapper.unmount()
  })

  it('uses runtime for native API keys and rejects a type change during reauthorization', async () => {
    const wrapper = mount(KiroCredentialsForm)
    await wrapper.get('[data-testid="kiro-mode-json"]').setValue(true)
    await wrapper.get('[data-testid="kiro-import-json"]').setValue('{"apiKey":"test-key"}')
    expect(wrapper.vm.buildCredentials()).toMatchObject({ auth_method: 'api_key', api_key: 'test-key', endpoint_family: 'runtime' })
    wrapper.unmount()
    const reauth = mount(KiroCredentialsForm, { props: { editing: true, accountType: 'oauth' } })
    await reauth.get('[data-testid="kiro-mode-json"]').setValue(true)
    await reauth.get('[data-testid="kiro-import-json"]').setValue('{"apiKey":"test-key"}')
    expect(() => reauth.vm.buildCredentials()).toThrow('typeMismatch')
    reauth.unmount()
  })

  it('rejects array imports and invalid model mappings', async () => {
    const wrapper = mount(KiroCredentialsForm)
    await wrapper.get('[data-testid="kiro-mode-json"]').setValue(true)
    await wrapper.get('[data-testid="kiro-import-json"]').setValue('[]')
    expect(() => wrapper.vm.buildCredentials()).toThrow('invalidJson')
    await wrapper.get('[data-testid="kiro-mode-json"]').setValue(true)
    await wrapper.get('[data-testid="kiro-import-json"]').setValue('{"refreshToken":"r"}')
    await wrapper.get('[data-testid="kiro-model-mapping"]').setValue('{"model":3}')
    expect(() => wrapper.vm.buildCredentials()).toThrow('invalidMapping')
    wrapper.unmount()
  })

  it('normalizes a copy and preserves canonical fields', () => {
    const raw = { refresh_token: 'current', refreshToken: 'old', authMethod: 'IdC' }
    expect(normalizeKiroCredentials(raw)).toEqual({ refresh_token: 'current', auth_method: 'idc' })
    expect(raw.refreshToken).toBe('old')
  })
})
