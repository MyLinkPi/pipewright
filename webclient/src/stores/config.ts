// 配置回显与部分更新:GET /api/config 构建草稿基线;PUT 只提交变更字段。
// 保存成功后用服务端回显重建 view(界面不做本地推断合并)。
import { defineStore } from 'pinia'
import { ref } from 'vue'
import { errorText, http } from '../api/http'
import type { ConfigSetRequest, ConfigView, ImportRequest } from '../api/types'
import { isPatchEmpty } from '../lib/configDraft'

export const useConfigStore = defineStore('config', () => {
  const view = ref<ConfigView | null>(null)
  const loading = ref(false)
  const error = ref('')

  async function load(): Promise<void> {
    loading.value = true
    try {
      view.value = await http.get<ConfigView>('/api/config')
      error.value = ''
    } catch (err) {
      error.value = errorText(err)
    } finally {
      loading.value = false
    }
  }

  // save 在无变更时直接返回当前回显;有变更时提交并更新 view。
  async function save(patch: ConfigSetRequest): Promise<ConfigView | null> {
    if (isPatchEmpty(patch)) return view.value
    const next = await http.put<ConfigView>('/api/config', patch)
    view.value = next
    return next
  }

  async function importYaml(content: string, replaceRoutes: boolean): Promise<ConfigView> {
    const body: ImportRequest = { content, replaceRoutes }
    const next = await http.post<ConfigView>('/api/import', body)
    view.value = next
    return next
  }

  return { view, loading, error, load, save, importYaml }
})
