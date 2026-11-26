import { onUnmounted, ref } from 'vue'

import { messageFor } from '../../shared/errors'
import { HttpError } from '../../shared/http'
import { t } from '../../shared/i18n'
import { notify } from '../../shared/notifications'
import { RetryBackoff } from '../../shared/retryBackoff'
import { sessionApi } from './sessionApi'
import type { ComposerDefaults, ComposerOptions } from './sessionApi'

export function useComposerCatalog(unauthorized: () => void) {
  const catalog = ref<ComposerOptions>({ defaultModel: '', models: [], modelsLoading: false, mcpServers: [] })
  const catalogRequest = new AbortController()
  const savingDefaults = ref(false)
  const catalogBackoff = new RetryBackoff()
  let catalogRetry: ReturnType<typeof setTimeout> | undefined
  let catalogFetching = false
  let catalogReady = false
  let defaultsVersion = 0

  async function saveComposerDefaults(input: ComposerDefaults) {
    if (savingDefaults.value) return
    savingDefaults.value = true
    const signal = catalogRequest.signal
    try {
      const setComposerDefaults = await sessionApi.saveComposerDefaults(input, signal)
      if (signal.aborted) return
      defaultsVersion++
      catalog.value.defaults = setComposerDefaults
      notify.success(t('composer.defaultsSaved'))
    } catch (error) {
      if (!signal.aborted) notify.error(messageFor(error, 'composer.defaultsFailed'))
    } finally {
      savingDefaults.value = false
    }
  }

  function retryComposerOptions(error?: unknown) {
    const delay = catalogBackoff.next()
    if (delay !== undefined) {
      if (!document.hidden) {
        catalogRetry = setTimeout(() => {
          void loadComposerOptions()
        }, delay)
      }
    } else {
      notify.error(messageFor(error, 'composer.optionsFailed'))
    }
  }

  async function loadComposerOptions() {
    const { signal } = catalogRequest
    if (catalogFetching || document.hidden || signal.aborted) return
    catalogFetching = true
    const version = defaultsVersion
    try {
      const composerOptions = await sessionApi.composerOptions(signal)
      if (signal.aborted) return
      if (defaultsVersion !== version) composerOptions.defaults = catalog.value.defaults
      catalog.value = composerOptions
      catalogReady = !composerOptions.modelsLoading
      if (catalogReady) catalogBackoff.reset()
      else retryComposerOptions()
    } catch (error) {
      if (signal.aborted) return
      if (error instanceof HttpError && error.status === 401) {
        unauthorized()
      } else {
        retryComposerOptions(error)
      }
    } finally {
      catalogFetching = false
    }
  }

  function resume() {
    clearTimeout(catalogRetry)
    if (document.hidden || catalogReady) return
    catalogBackoff.reset()
    void loadComposerOptions()
  }

  onUnmounted(() => {
    catalogRequest.abort()
    clearTimeout(catalogRetry)
  })

  return { catalog, savingDefaults, saveComposerDefaults, resume }
}
