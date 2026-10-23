import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { defineComponent } from 'vue'
import AccountTestModal from '../AccountTestModal.vue'

const { getAvailableModelsMock } = vi.hoisted(() => ({
  getAvailableModelsMock: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      getAvailableModels: getAvailableModelsMock
    }
  }
}))

vi.mock('@/composables/useClipboard', () => ({
  useClipboard: () => ({
    copyToClipboard: vi.fn()
  })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

const BaseDialogStub = defineComponent({
  name: 'BaseDialog',
  props: { show: { type: Boolean, default: false } },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>'
})

const SelectStub = defineComponent({
  name: 'SelectStub',
  props: {
    modelValue: { type: [String, Number, Boolean, null], default: '' },
    options: { type: Array, default: () => [] },
    valueKey: { type: String, default: 'value' },
    labelKey: { type: String, default: 'label' }
  },
  emits: ['update:modelValue'],
  template: `
    <select
      v-bind="$attrs"
      :value="modelValue"
      @change="$emit('update:modelValue', $event.target.value)"
    >
      <option
        v-for="option in options"
        :key="option[valueKey]"
        :value="option[valueKey]"
      >
        {{ option[labelKey] }}
      </option>
    </select>
  `
})

const TextAreaStub = defineComponent({
  name: 'TextArea',
  props: {
    modelValue: { type: String, default: '' }
  },
  emits: ['update:modelValue'],
  template: `
    <textarea
      v-bind="$attrs"
      :value="modelValue"
      @input="$emit('update:modelValue', $event.target.value)"
    />
  `
})

function buildAccount() {
  return {
    id: 1,
    name: 'OpenAI OAuth',
    platform: 'openai',
    type: 'oauth',
    status: 'active',
    credentials: {},
    extra: {},
    concurrency: 1,
    priority: 1,
    proxy_id: null,
    auto_pause_on_expired: false
  } as any
}

describe('AccountTestModal', () => {
  const originalFetch = global.fetch

  beforeEach(() => {
    getAvailableModelsMock.mockReset()
    getAvailableModelsMock.mockResolvedValue([
      { id: 'gpt-5.4', display_name: 'GPT-5.4' }
    ])
    global.fetch = vi.fn().mockResolvedValue({
      ok: true,
      body: {
        getReader: () => ({
          read: vi.fn().mockResolvedValue({ done: true, value: undefined })
        })
      }
    } as any)
    localStorage.setItem('auth_token', 'test-token')
  })

  afterEach(() => {
    global.fetch = originalFetch
    localStorage.clear()
  })

  function mountKiroModal() {
    return mount(AccountTestModal, {
      props: { show: true, account: { ...buildAccount(), platform: 'kiro', name: 'Kiro' } },
      global: { stubs: { BaseDialog: BaseDialogStub, Select: SelectStub, TextArea: TextAreaStub, Icon: true } }
    })
  }

  it('automatically loads Kiro models when mounted open and selects a real model', async () => {
    getAvailableModelsMock.mockResolvedValue([{ id: 'auto', display_name: 'Auto' }, { id: 'claude-sonnet-4.5', display_name: 'Claude Sonnet 4.5' }, { id: 'minimax-m2.5', display_name: 'MiniMax M2.5' }])
    const wrapper = mountKiroModal()
    await flushPromises()
    expect(getAvailableModelsMock).toHaveBeenCalledWith(1)
    expect(wrapper.get<HTMLSelectElement>('select').element.value).toBe('minimax-m2.5')
    expect(wrapper.text()).toContain('Claude Sonnet 4.5')
    expect(wrapper.find('[data-testid="account-models-error"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('shows model discovery errors and allows fetching again', async () => {
    getAvailableModelsMock.mockRejectedValueOnce({ response: { data: { message: 'Kiro returned HTTP 403' } } }).mockResolvedValueOnce([{ id: 'glm-5', display_name: 'GLM 5' }])
    const wrapper = mountKiroModal()
    await flushPromises()
    expect(wrapper.get('[data-testid="account-models-error"]').text()).toContain('403')
    await wrapper.get('[data-testid="refresh-account-models"]').trigger('click')
    await flushPromises()
    expect(wrapper.get<HTMLSelectElement>('select').element.value).toBe('glm-5')
    expect(wrapper.find('[data-testid="account-models-error"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('shows Kiro throttling without claiming a connection and enforces the retry countdown', async () => {
    getAvailableModelsMock.mockResolvedValue([{ id: 'claude-haiku-4.5', display_name: 'Haiku' }])
    const wrapper = mountKiroModal()
    await flushPromises()
    vi.useFakeTimers()
    try {
      const chunks = [new TextEncoder().encode(
        'data: {"type":"status","code":"kiro_connecting","text":"waiting"}\n\n' +
        'data: {"type":"error","code":"kiro_throttled","error":"fallback","data":{"retry_after_seconds":2}}\n\n'
      )]
      global.fetch = vi.fn().mockResolvedValue({ ok: true, body: { getReader: () => ({
        read: async () => chunks.length ? { done: false, value: chunks.shift() } : { done: true }
      }) } }) as any
      await (wrapper.vm as any).startTest()
      await flushPromises()
      expect(wrapper.text()).toContain('admin.accounts.kiro.throttled')
      expect(wrapper.text()).not.toContain('admin.accounts.connectedToApi')
      const retry = wrapper.findAll('button').find(button => button.text().includes('kiro.retryCountdown'))!
      expect(retry.attributes('disabled')).toBeDefined()
      await (wrapper.vm as any).startTest()
      expect(global.fetch).toHaveBeenCalledTimes(1)
      await vi.advanceTimersByTimeAsync(2100)
      expect((wrapper.vm as any).retrySeconds).toBe(0)
      expect(vi.getTimerCount()).toBe(0)
    } finally {
      wrapper.unmount()
      vi.useRealTimers()
    }
  })

  it('does not overwrite another account with an old Kiro model response', async () => {
    let resolve!: (models: unknown[]) => void
    getAvailableModelsMock.mockImplementationOnce(() => new Promise(done => { resolve = done }))
    const wrapper = mountKiroModal()
    getAvailableModelsMock.mockResolvedValueOnce([{ id: 'new-model', display_name: 'New account model' }])
    await wrapper.setProps({ account: { ...buildAccount(), id: 2, platform: 'kiro' } })
    await flushPromises()
    resolve([{ id: 'old-model', display_name: 'Old account model' }])
    await flushPromises()
    expect(wrapper.get<HTMLSelectElement>('select').element.value).toBe('new-model')
    expect(wrapper.text()).not.toContain('Old account model')
    wrapper.unmount()
  })

  it('posts compact mode for OpenAI compact probe', async () => {
    const wrapper = mount(AccountTestModal, {
      props: {
        show: true,
        account: buildAccount()
      },
      global: {
        stubs: {
          BaseDialog: BaseDialogStub,
          Select: SelectStub,
          TextArea: TextAreaStub,
          Icon: true
        }
      }
    })

    await flushPromises()
    ;(wrapper.vm as any).selectedModelId = 'gpt-5.4'
    ;(wrapper.vm as any).testMode = 'compact'
    await (wrapper.vm as any).startTest()
    await flushPromises()

    expect(global.fetch).toHaveBeenCalledTimes(1)
    const [, options] = (global.fetch as any).mock.calls[0]
    expect(JSON.parse(options.body)).toMatchObject({
      model_id: 'gpt-5.4',
      mode: 'compact'
    })
  })

  it('renders Chat Completions path status from test SSE', async () => {
    const encoder = new TextEncoder()
    const chunks = [
      encoder.encode('data: {"type":"status","text":"已通过 /v1/chat/completions 验证"}\n\n'),
      encoder.encode('data: {"type":"test_complete","success":true}\n\n')
    ]
    global.fetch = vi.fn().mockResolvedValue({
      ok: true,
      body: {
        getReader: () => ({
          read: vi.fn().mockImplementation(() => Promise.resolve(
            chunks.length > 0
              ? { done: false, value: chunks.shift() }
              : { done: true, value: undefined }
          ))
        })
      }
    } as any)

    const wrapper = mount(AccountTestModal, {
      props: {
        show: true,
        account: buildAccount()
      },
      global: {
        stubs: {
          BaseDialog: BaseDialogStub,
          Select: SelectStub,
          TextArea: TextAreaStub,
          Icon: true
        }
      }
    })

    await flushPromises()
    ;(wrapper.vm as any).selectedModelId = 'gpt-5.4'
    await (wrapper.vm as any).startTest()
    await flushPromises()

    expect(wrapper.text()).toContain('已通过 /v1/chat/completions 验证')
  })
})
