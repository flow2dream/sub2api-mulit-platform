import { mount, flushPromises } from '@vue/test-utils'
import { describe, expect, it, vi, beforeEach } from 'vitest'
import type { Account } from '@/types'
import ReAuthAccountModal from '../ReAuthAccountModal.vue'

const mocks = vi.hoisted(() => ({ update: vi.fn(), clearError: vi.fn(), showError: vi.fn(), showSuccess: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { accounts: { update: mocks.update, clearError: mocks.clearError } } }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError: mocks.showError, showSuccess: mocks.showSuccess }) }))
vi.mock('vue-i18n', async (importOriginal) => ({ ...await importOriginal<typeof import('vue-i18n')>(), useI18n: () => ({ t: (key: string) => key }) }))

describe('Kiro reauthorization in the account management dialog', () => {
  beforeEach(() => vi.clearAllMocks())

  it('updates the existing native account and emits the returned account', async () => {
    const account = { id: 7, name: 'Kiro', platform: 'kiro', type: 'oauth', credentials: { auth_method: 'social' } } as Account
    const updated = { ...account, status: 'active' }
    mocks.update.mockResolvedValue(updated)
    mocks.clearError.mockResolvedValue(updated)
    const wrapper = mount(ReAuthAccountModal, {
      props: { show: true, account },
      global: { stubs: { BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' }, Icon: true, OAuthAuthorizationFlow: { template: '<div data-testid="browser-oauth" />' } } }
    })
    expect(wrapper.find('[data-testid="browser-oauth"]').exists()).toBe(false)
    await wrapper.get('[data-testid="kiro-mode-json"]').setValue(true)
    await wrapper.get('[data-testid="kiro-import-json"]').setValue('{"refreshToken":"test-new-refresh"}')
    const save = wrapper.findAll('button').find(button => button.text() === 'common.save')!
    await save.trigger('click')
    await flushPromises()
    expect(mocks.update).toHaveBeenCalledWith(7, { credentials: expect.objectContaining({ refresh_token: 'test-new-refresh', auth_method: 'social' }) })
    expect(mocks.clearError).toHaveBeenCalledWith(7)
    expect(wrapper.emitted('reauthorized')?.[0]).toEqual([updated])
    expect(mocks.showError).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('keeps the dialog open when credential persistence fails', async () => {
    mocks.update.mockRejectedValue(new Error('save failed'))
    const wrapper = mount(ReAuthAccountModal, {
      props: { show: true, account: { id: 7, platform: 'kiro', type: 'oauth', credentials: { auth_method: 'social' } } as Account },
      global: { stubs: { BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' }, Icon: true, OAuthAuthorizationFlow: true } }
    })
    await wrapper.get('[data-testid="kiro-mode-json"]').setValue(true)
    await wrapper.get('[data-testid="kiro-import-json"]').setValue('{"refreshToken":"test-new-refresh"}')
    await wrapper.findAll('button').find(button => button.text() === 'common.save')!.trigger('click')
    await flushPromises()
    expect(mocks.clearError).not.toHaveBeenCalled()
    expect(wrapper.emitted('reauthorized')).toBeUndefined()
    expect(wrapper.emitted('close')).toBeUndefined()
    expect(mocks.showError).toHaveBeenCalledWith('save failed')
    wrapper.unmount()
  })
})
