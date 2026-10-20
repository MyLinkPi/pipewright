<!--
  CodeEditor.vue — 可编辑代码编辑器(Monaco 动态 import,按文件名高亮)。

  与 CodeViewer 同源的加载纪律(NFR-4 体积铁律):Monaco 经 `import('monaco-editor')`
  **动态**加载 → Vite 拆为独立 chunk,不进主 bundle、不抬首屏;仅首次打开文件时
  下载。Monaco 加载失败 → 退化为可编辑 <textarea>(仍可改可存)。

  与数据解耦:内容经 props 传入、经 getValue() 取出,不关心来源(项目源码 /
  服务器文件);保存动作由外层驱动。readonly 时退化为纯查看(Monaco readOnly)。
-->
<script setup lang="ts">
import { ref, watch, onUnmounted, nextTick } from 'vue'
import { languageForPath } from './monacoLang'
import { installMonacoEnvironment } from './monacoEnv'
import { useThemeStore } from '../../stores/theme'

// Monaco 类型仅作类型标注(type-only import 不进运行时 bundle)。
import type * as MonacoNS from 'monaco-editor'

const props = defineProps<{
  /** 当前文件路径(取 basename 判语言;null ⇒ 空态)。 */
  path: string | null
  /** 初始内容(props 变化即整体重置,调用方在重新加载文件时更换引用)。 */
  content: string
  readonly?: boolean
}>()

const themeStore = useThemeStore()

const editorHost = ref<HTMLElement | null>(null)
const textareaHost = ref<HTMLTextAreaElement | null>(null)
let monaco: typeof MonacoNS | null = null
let editor: MonacoNS.editor.IStandaloneCodeEditor | null = null
const monacoFailed = ref(false)
/** 当前是否处于 Monaco 降级(textarea)模式。 */
const fallback = ref(false)

function monacoTheme(): string {
  return themeStore.current === 'light' ? 'vs' : 'vs-dark'
}

/** 懒加载 Monaco;失败置 monacoFailed 并回退 textarea。 */
async function ensureMonaco(): Promise<typeof MonacoNS | null> {
  if (monaco) return monaco
  if (monacoFailed.value) return null
  try {
    installMonacoEnvironment()
    monaco = await import('monaco-editor')
    return monaco
  } catch {
    monacoFailed.value = true
    return null
  }
}

function disposeEditor(): void {
  if (editor) {
    editor.dispose()
    editor = null
  }
}

/** getValue 取当前编辑内容(Monaco 或降级 textarea;未就绪返回最近内容)。 */
function getValue(): string {
  if (editor) return editor.getValue()
  return textareaHost.value?.value ?? props.content
}

/** isModified 报告内容相对 props.content 是否已被修改。 */
function isModified(): boolean {
  return getValue() !== props.content
}

async function render(): Promise<void> {
  if (!props.path) {
    disposeEditor()
    return
  }
  const m = await ensureMonaco()
  if (!m) {
    fallback.value = true
    disposeEditor()
    await nextTick()
    if (textareaHost.value) textareaHost.value.value = props.content
    return
  }
  await nextTick()
  const host = editorHost.value
  if (!host) {
    fallback.value = true
    return
  }
  disposeEditor()
  fallback.value = false
  editor = m.editor.create(host, {
    value: props.content,
    language: languageForPath(props.path),
    theme: monacoTheme(),
    readOnly: props.readonly === true,
    automaticLayout: true,
    minimap: { enabled: false },
    scrollBeyondLastLine: false,
    fontFamily: 'JetBrains Mono, ui-monospace, Menlo, monospace',
    fontSize: 13,
    lineNumbers: 'on',
    renderWhitespace: 'none',
    wordWrap: 'off',
    contextmenu: true,
  })
}

// 路径/内容变化即重渲染(调用方在加载新文件后替换 props)。
watch(
  () => [props.path, props.content, props.readonly] as const,
  () => {
    void render()
  },
  { immediate: true },
)

// 主题切换 → 同步 Monaco 主题。
watch(
  () => themeStore.current,
  () => {
    if (monaco && editor) monaco.editor.setTheme(monacoTheme())
  },
)

onUnmounted(disposeEditor)

defineExpose({ getValue, isModified })
</script>

<template>
  <div class="code-editor">
    <div v-show="!fallback && path" ref="editorHost" class="code-editor-host"></div>
    <textarea
      v-if="fallback && path"
      ref="textareaHost"
      class="code-editor-fallback"
      spellcheck="false"
      :readonly="readonly === true"
      :aria-label="path ?? ''"
    ></textarea>
    <div v-if="!path" class="code-editor-empty"></div>
  </div>
</template>

<style scoped>
.code-editor {
  display: flex;
  flex-direction: column;
  min-height: 0;
  flex: 1;
  height: 100%;
}
.code-editor-host {
  flex: 1;
  min-height: 240px;
  height: 100%;
}
.code-editor-fallback {
  flex: 1;
  min-height: 240px;
  width: 100%;
  resize: none;
  border: none;
  outline: none;
  padding: 10px 12px;
  font-family: 'JetBrains Mono', ui-monospace, Menlo, monospace;
  font-size: 13px;
  line-height: 1.55;
  background: var(--color-code);
  color: #d4d4d4;
  tab-size: 4;
}
.code-editor-empty {
  flex: 1;
}
</style>
