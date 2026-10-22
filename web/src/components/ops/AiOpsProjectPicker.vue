<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { Project } from '../../api/projects'
import type { Server } from '../../api/servers'
import AiOpsServerPicker from './AiOpsServerPicker.vue'
const props = defineProps<{
  projects: Project[]
  projectId: string
  servers: Server[]
  selectedServers: string[]
  serverLimit: number
  disabled: boolean
}>()
const emit = defineEmits<{
  select: [projectId: string]
  changeServers: [ids: string[]]
}>()
const { t } = useI18n()
const projectName = computed(
  () =>
    props.projects.find((p) => p.id === props.projectId)?.name ??
    (props.projectId ? t('opsChat.missingProject') : ''),
)
</script>
<template>
  <section class="ops-section project-navigation">
    <label class="project-choice">
      <span class="ops-grow">{{ t('opsChat.project') }}</span>
      <select
        class="project-select"
        :value="projectId"
        :disabled="disabled"
        :aria-label="t('opsChat.selectProject')"
        @change="
          emit('select', ($event.target as HTMLSelectElement).value)
        "
      >
        <option value="" :disabled="!disabled">
          {{ t('opsChat.selectProject') }}
        </option>
        <option v-for="p in projects" :key="p.id" :value="p.id">
          {{ p.name }}
        </option>
      </select>
    </label>
    <p class="target-summary ops-muted ops-wrap" tabindex="0">
      {{ projectName || t('opsChat.noProject') }}
    </p>
    <AiOpsServerPicker
      :servers="servers"
      :selected="selectedServers"
      :limit="serverLimit"
      :disabled="disabled"
      @change="emit('changeServers', $event)"
    />
  </section>
</template>
<style scoped src="./opsChat.css"></style>
<style scoped>
.project-navigation {
  display: flex;
  flex-direction: column;
  gap: 4px;
  flex: none;
  min-height: 0;
}
.project-choice {
  display: flex;
  gap: 10px;
  align-items: center;
  min-height: 36px;
}
.project-select {
  flex: 1;
  min-width: 0;
  max-width: 60%;
}
.target-summary {
  margin: 0;
  overflow-wrap: anywhere;
}
.project-choice:focus-visible,
.target-summary:focus-visible {
  outline: 2px solid var(--color-primary);
  outline-offset: -2px;
}
</style>
