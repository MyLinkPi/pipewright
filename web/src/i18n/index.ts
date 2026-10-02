/**
 * i18n 接线 + 语言侦测。
 *
 * 默认语言取自浏览器(`navigator.languages`),用户手动切换后写 localStorage 覆盖,
 * 下次启动优先用存储值。任何一项不可用都安静回退到 zh-CN(产品默认语言)。
 *
 * 组件内用 `useI18n()`;纯函数 / api 层(无 setup 上下文)用本模块导出的 `t`。
 */
import { createI18n } from 'vue-i18n'
import zhCN from './locales/zh-CN'
import zhHK from './locales/zh-HK'
import en from './locales/en'
import ja from './locales/ja'
import ko from './locales/ko'
import es from './locales/es'
import fr from './locales/fr'
import de from './locales/de'

export type LocaleCode = 'zh-CN' | 'zh-HK' | 'en' | 'ja' | 'ko' | 'es' | 'fr' | 'de'

/** 选择器里展示的语言列表(label 用各语言的母语名)。 */
export const SUPPORTED_LOCALES: ReadonlyArray<{ code: LocaleCode; label: string }> = [
  { code: 'zh-CN', label: '简体中文' },
  { code: 'zh-HK', label: '繁體中文' },
  { code: 'en', label: 'English' },
  { code: 'ja', label: '日本語' },
  { code: 'ko', label: '한국어' },
  { code: 'es', label: 'Español' },
  { code: 'fr', label: 'Français' },
  { code: 'de', label: 'Deutsch' },
]

const STORAGE_KEY = 'pipewright-locale'
const DEFAULT_LOCALE: LocaleCode = 'zh-CN'

function isSupported(code: string): code is LocaleCode {
  return SUPPORTED_LOCALES.some((l) => l.code === code)
}

/** 把任意 BCP-47 标签粗匹配到受支持语言。 */
function matchLocale(tag: string): LocaleCode | null {
  const lower = tag.toLowerCase()
  if (lower.startsWith('zh')) {
    // 繁体:zh-Hant / zh-TW / zh-HK / zh-MO 一律归到 zh-HK;其余 zh* 视作简体。
    if (/hant|tw|hk|mo/.test(lower)) return 'zh-HK'
    return 'zh-CN'
  }
  if (lower.startsWith('en')) return 'en'
  if (lower.startsWith('ja')) return 'ja'
  if (lower.startsWith('ko')) return 'ko'
  if (lower.startsWith('es')) return 'es'
  if (lower.startsWith('fr')) return 'fr'
  if (lower.startsWith('de')) return 'de'
  return null
}

/**
 * 侦测初始语言:存储覆盖 > 浏览器偏好 > 默认。
 * localStorage 不可用(隐私模式 / 安全策略)时安静跳过。
 */
export function detectLocale(): LocaleCode {
  try {
    const stored = localStorage.getItem(STORAGE_KEY)
    // 旧版存过 'zh-TW',迁移到新代码,避免老用户语言悄悄回退。
    const normalized = stored === 'zh-TW' ? 'zh-HK' : stored
    if (normalized && isSupported(normalized)) return normalized
  } catch {
    // ignore — private mode / SecurityError
  }
  const prefs = typeof navigator !== 'undefined' ? navigator.languages ?? [navigator.language] : []
  for (const tag of prefs) {
    const m = tag && matchLocale(tag)
    if (m) return m
  }
  return DEFAULT_LOCALE
}

// 基础词条(阶段一:外壳/登录/概览/公共)。每种语言一个文件;具体类型由此推断。
const messages = { 'zh-CN': zhCN, 'zh-HK': zhHK, en, ja, ko, es, fr, de }

// 按页面拆分的命名空间:`locales/<lang>/<page>.ts` 自动并入,文件名即顶层命名空间。
// 阶段二起每个页面在此各放 8 个语言文件,互不触碰基础文件 → 可并行扩展、零冲突。
const pageModules = import.meta.glob<{ default: Record<string, unknown> }>('./locales/*/*.ts', {
  eager: true,
})
const bag = messages as Record<string, Record<string, unknown>>
for (const [path, mod] of Object.entries(pageModules)) {
  const m = path.match(/\.\/locales\/([^/]+)\/([^/]+)\.ts$/)
  if (!m) continue
  const [, lang, ns] = m
  if (!isSupported(lang)) continue
  bag[lang][ns] = mod.default
}

export const i18n = createI18n({
  legacy: false,
  locale: detectLocale(),
  fallbackLocale: DEFAULT_LOCALE,
  messages,
  // 词条含 `server:<id>` / `<base>` 这类占位符记号(非 HTML),且项目不把 i18n 文本喂给
  // v-html → 关掉「检测到 HTML」误报,保持浏览器控制台干净。
  warnHtmlMessage: false,
})

function applyDocumentLang(code: LocaleCode): void {
  if (typeof document !== 'undefined') document.documentElement.lang = code
}

// 初始即同步 <html lang>(无障碍 / 搜索引擎)。
applyDocumentLang(i18n.global.locale.value as LocaleCode)

/** 切换当前语言:更新 vue-i18n、持久化、同步 <html lang>。 */
export function setLocale(code: LocaleCode): void {
  i18n.global.locale.value = code
  applyDocumentLang(code)
  try {
    localStorage.setItem(STORAGE_KEY, code)
  } catch {
    // ignore — degrade to in-memory only
  }
}

export function currentLocale(): LocaleCode {
  return i18n.global.locale.value as LocaleCode
}

/** 供纯函数 / api 层使用的全局翻译器(组件内请用 `useI18n()`)。 */
export const t = i18n.global.t
