<script setup lang="ts">
import { computed } from 'vue'
import { jobTypeAccent } from './jobConfigSchema'

const props = withDefaults(
  defineProps<{
    type: string
    /** Swatch edge length in px */
    size?: number
  }>(),
  { size: 30 },
)

const accent = computed(() => jobTypeAccent(props.type))

/** Inline SVG path(s) per type. Stroke icons, 24×24 viewBox, consistent weight. */
const ICONS: Record<string, string> = {
  git_source:
    '<circle cx="6" cy="6" r="2.4"/><circle cx="6" cy="18" r="2.4"/><circle cx="18" cy="9" r="2.4"/><path d="M6 8.4v7.2M18 11.4c0 3-2.4 4.2-5 4.2"/>',
  build_image:
    '<path d="M21 7.5 12 3 3 7.5 12 12l9-4.5Z"/><path d="M3 7.5v9l9 4.5 9-4.5v-9"/><path d="M12 12v9"/>',
  push_image:
    '<path d="M12 15V4"/><path d="m8 8 4-4 4 4"/><path d="M5 15v3a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2v-3"/>',
  // OSS 上传:云 + 上箭头(推送产物到对象存储)。
  oss_upload:
    '<path d="M6.5 19a4 4 0 0 1-.5-8A5.5 5.5 0 0 1 17 10.5a3.5 3.5 0 0 1 .5 7"/><path d="M12 21v-7"/><path d="m9 17 3-3 3 3"/>',
  // 集群推送:两节点 + 传递箭头(产物/镜像 → 对端节点)。
  mesh_push:
    '<circle cx="6" cy="12" r="2.6"/><circle cx="18" cy="12" r="2.6"/><path d="M8.6 12h6.8"/><path d="m13 9.5 2.5 2.5-2.5 2.5"/>',
  // 集群触发:播放三角 + 节点(启动对端工作流)。
  mesh_trigger:
    '<circle cx="17" cy="12" r="3"/><path d="M14 12H4"/><path d="m7 9-3 3 3 3"/><path d="M16.4 11v2l1.6 1"/>',
  // 被推送源:入站托盘(集群推送来的产物/镜像)。
  pushed_source:
    '<path d="M12 4v9"/><path d="m8.5 9.5 3.5 3.5 3.5-3.5"/><path d="M4 15.5v2A2.5 2.5 0 0 0 6.5 20h11a2.5 2.5 0 0 0 2.5-2.5v-2"/>',
  // 阿里云 ECS 开关机:电源符号(开机/关机;关机固定节省停机模式)。
  ecs_power:
    '<path d="M12 3v8"/><path d="M6.3 6.5a8 8 0 1 0 11.4 0"/>',
  deploy_ssh:
    '<rect x="3" y="4" width="18" height="6" rx="1.6"/><rect x="3" y="14" width="18" height="6" rx="1.6"/><path d="M7 7h.01M7 17h.01"/>',
  deploy_container:
    '<rect x="3" y="9" width="18" height="11" rx="1.6"/><path d="M7 9V6.5A1.5 1.5 0 0 1 8.5 5h2A1.5 1.5 0 0 1 12 6.5V9M12 9V6.5A1.5 1.5 0 0 1 13.5 5h2A1.5 1.5 0 0 1 17 6.5V9M7 13v3M12 13v3M17 13v3"/>',
  health_check: '<path d="M3 12h4l2-6 4 12 2-6h6"/>',
  // 等待节点:时钟(暂停固定秒数,运行中可手动跳过)。
  wait:
    '<circle cx="12" cy="12" r="8.4"/><path d="M12 7.2V12l3 2.4"/>',
  notify:
    '<path d="M18 8.5a6 6 0 0 0-12 0c0 6.5-2.5 8.5-2.5 8.5h17S18 15 18 8.5"/><path d="M13.6 21a2 2 0 0 1-3.2 0"/>',
  script:
    '<rect x="3" y="4.5" width="18" height="15" rx="1.8"/><path d="m7 10 3 2.5-3 2.5M13 15.5h4"/>',
  custom:
    '<rect x="3" y="4.5" width="18" height="15" rx="1.8"/><path d="m7 10 3 2.5-3 2.5M13 15.5h4"/>',
  // 按语言区分的构建模板:六边形 + 语言首字母式符号差异(统一 hexagon + 内部图形)。
  build_nodejs:
    '<path d="M12 3 4.5 7.2v9.6L12 21l7.5-4.2V7.2L12 3Z"/><path d="M9 9.5v4a1.6 1.6 0 0 1-3.2 0M13 15.5l4.5-6M17.5 15.5v-6"/>',
  build_java:
    '<path d="M12 3 4.5 7.2v9.6L12 21l7.5-4.2V7.2L12 3Z"/><path d="M9.5 8.5c4 0 6 1 6 2.5s-2 2.5-6 2.5c2.5 0 4 .8 4 2s-1.5 2-4 2"/>',
  build_golang:
    '<path d="M12 3 4.5 7.2v9.6L12 21l7.5-4.2V7.2L12 3Z"/><path d="M8 12h4.5a2 2 0 1 0-2-2.6M8 15.5h6"/>',
  build_python:
    '<path d="M12 3 4.5 7.2v9.6L12 21l7.5-4.2V7.2L12 3Z"/><path d="M12 7.5c-2.5 0-3.5.8-3.5 2h5c2.6 0 3.5 1 3.5 2.5s-1 2.5-3.5 2.5h-3"/>',
}

const path = computed(() => ICONS[props.type] ?? '<circle cx="12" cy="12" r="7"/><path d="M12 9v3M12 15h.01"/>')
const inner = computed(() => props.size * 0.5)
</script>

<template>
  <span
    class="type-icon"
    :class="`type-icon--${accent}`"
    :style="{ width: `${size}px`, height: `${size}px` }"
    aria-hidden="true"
  >
    <svg
      :width="inner"
      :height="inner"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      stroke-width="1.9"
      stroke-linecap="round"
      stroke-linejoin="round"
      v-html="path"
    />
  </span>
</template>

<style scoped>
.type-icon {
  display: inline-grid;
  place-items: center;
  border-radius: var(--rounded-md);
  flex-shrink: 0;
}
.type-icon--cyan    { background: var(--color-cyan-soft);    color: var(--color-cyan); }
.type-icon--primary { background: var(--color-primary-soft); color: var(--color-primary); }
.type-icon--green   { background: var(--color-green-soft);   color: var(--color-green); }
.type-icon--amber   { background: var(--color-amber-soft);   color: var(--color-amber); }
.type-icon--red     { background: var(--color-red-soft);     color: var(--color-red); }
.type-icon--neutral { background: var(--color-inset);        color: var(--color-faint); }
</style>
