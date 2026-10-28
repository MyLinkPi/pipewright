<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted } from 'vue'
import { NConfigProvider, NDialogProvider, NMessageProvider, darkTheme } from 'naive-ui'
import AppShell from './components/AppShell.vue'
import { useStatusStore } from './stores/status'
import { useLocalStore } from './stores/local'
import { useUiStore } from './stores/ui'

const status = useStatusStore()
const local = useLocalStore()
const ui = useUiStore()

const naiveTheme = computed(() => (ui.theme === 'dark' ? darkTheme : null))

onMounted(() => {
  ui.apply()
  status.start()
  void local.load()
})

onBeforeUnmount(() => {
  status.stop()
})
</script>

<template>
  <n-config-provider :theme="naiveTheme">
    <n-message-provider>
      <n-dialog-provider>
        <AppShell />
      </n-dialog-provider>
    </n-message-provider>
  </n-config-provider>
</template>
