<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import {
  NButton,
  NCard,
  NInput,
  NSwitch,
  NTag,
  useMessage,
} from 'naive-ui'
import { useConfigStore } from '../stores/config'
import { useStatusStore } from '../stores/status'
import {
  buildEasytierPatch,
  buildJoinPatch,
  easytierDraft,
  joinDraft,
  type EasytierDraft,
  type JoinDraft,
} from '../lib/configDraft'
import { validatePeers, validateVirtualIP } from '../lib/validate'

const router = useRouter()
const message = useMessage()
const config = useConfigStore()
const status = useStatusStore()

const join = ref<JoinDraft>({
  networkName: '',
  peers: [],
  virtualIp: '',
  hostname: '',
  networkSecret: '',
  credential: '',
})
const baseJoin = ref<JoinDraft>({ ...join.value })
const easytier = ref<EasytierDraft>({ binDir: '', rpcAddr: '' })
const baseEasytier = ref<EasytierDraft>({ binDir: '', rpcAddr: '' })

const importText = ref('')
const replaceRoutes = ref(true)
const importing = ref(false)
const savingJoin = ref(false)
const savingEasytier = ref(false)

function syncDraft(): void {
  const view = config.view
  if (!view) return
  const draft = joinDraft(view.join)
  join.value = { ...draft, peers: [...draft.peers] }
  baseJoin.value = { ...draft, peers: [...draft.peers] }
  const advanced = easytierDraft(view.easytier)
  easytier.value = { ...advanced }
  baseEasytier.value = { ...advanced }
}

onMounted(async () => {
  if (!config.view) await config.load()
  syncDraft()
})

const joinDirty = computed(
  () => Object.keys(buildJoinPatch(join.value, baseJoin.value)).length > 0,
)
const easytierDirty = computed(
  () => Object.keys(buildEasytierPatch(easytier.value, baseEasytier.value)).length > 0,
)

function addPeer(): void {
  join.value.peers = [...join.value.peers, '']
}

function removePeer(index: number): void {
  join.value.peers = join.value.peers.filter((_, i) => i !== index)
}

async function saveJoin(): Promise<void> {
  const peerErr = validatePeers(join.value.peers)
  if (peerErr) {
    message.error(peerErr)
    return
  }
  const ipErr = validateVirtualIP(join.value.virtualIp)
  if (ipErr) {
    message.error(ipErr)
    return
  }
  savingJoin.value = true
  try {
    await config.save(buildJoinPatch(join.value, baseJoin.value))
    syncDraft()
    await status.refresh()
    message.success('已保存并应用（保存即生效，组网可能短暂重连）')
  } catch (e) {
    message.error(e instanceof Error ? e.message : String(e))
  } finally {
    savingJoin.value = false
  }
}

async function saveEasytier(): Promise<void> {
  savingEasytier.value = true
  try {
    await config.save(buildEasytierPatch(easytier.value, baseEasytier.value))
    syncDraft()
    await status.refresh()
    message.success('已保存并应用')
  } catch (e) {
    message.error(e instanceof Error ? e.message : String(e))
  } finally {
    savingEasytier.value = false
  }
}

async function pickFile(event: Event): Promise<void> {
  const input = event.target as HTMLInputElement
  const file = input.files?.[0]
  if (file) importText.value = await file.text()
  input.value = ''
}

async function onDrop(event: DragEvent): Promise<void> {
  const file = event.dataTransfer?.files?.[0]
  if (file) importText.value = await file.text()
}

async function doImport(): Promise<void> {
  const content = importText.value.trim()
  if (!content) {
    message.error('请先粘贴或选择 pipewright-client.yaml')
    return
  }
  importing.value = true
  try {
    await config.importYaml(content, replaceRoutes.value)
    syncDraft()
    await status.refresh()
    message.success('已导入集群配置并自动连接')
    importText.value = ''
    await router.push('/overview')
  } catch (e) {
    message.error(e instanceof Error ? e.message : String(e))
  } finally {
    importing.value = false
  }
}
</script>

<template>
  <div class="page">
    <n-card title="从平台导入（捷径）" size="small">
      <template #header-extra>
        <n-tag v-if="config.view" size="small">下方也可逐项修改，无需重新导入</n-tag>
      </template>
      <p class="faint hint">
        从平台「设置 → 集群 → 导出客户端配置」复制 YAML 粘贴到下面，或用文件选择/拖拽导入。
        导入会写入网络名、认证、对端地址；网段路由按开关决定是否覆盖本机列表。
      </p>
      <div
        class="drop"
        @dragover.prevent
        @drop.prevent="onDrop"
      >
        <n-input
          v-model:value="importText"
          type="textarea"
          :rows="5"
          placeholder="粘贴 pipewright-client.yaml 全文"
        />
        <div class="import-actions">
          <label class="file">
            <input type="file" accept=".yaml,.yml" @change="pickFile" />
            <span>选择文件…</span>
          </label>
          <span class="faint grow">也可以直接把文件拖到上方文本框</span>
          <span class="faint">用文件网段替换本机路由</span>
          <n-switch v-model:value="replaceRoutes" size="small" />
          <n-button
            size="small"
            type="primary"
            :loading="importing"
            :disabled="importText.trim() === ''"
            @click="doImport"
          >
            导入
          </n-button>
        </div>
      </div>
    </n-card>

    <n-card title="连接参数" size="small">
      <template #header-extra>
        <n-tag v-if="joinDirty" size="small" type="warning">有未保存修改</n-tag>
      </template>
      <div class="form">
        <label class="field">
          <span class="faint">网络名</span>
          <n-input v-model:value="join.networkName" placeholder="集群网络名（导入配置时自动写入）" />
        </label>
        <label class="field">
          <span class="faint">虚拟 IP</span>
          <n-input v-model:value="join.virtualIp" placeholder="留空 = DHCP 自动分配" />
        </label>
        <label class="field">
          <span class="faint">主机名</span>
          <n-input v-model:value="join.hostname" placeholder="留空 = 计算机名" />
        </label>
      </div>

      <div class="field-block">
        <div class="field-head">
          <span class="faint">对端地址（至少一个公网可达入口）</span>
          <n-button size="tiny" @click="addPeer">添加</n-button>
        </div>
        <div v-if="join.peers.length > 0" class="peers">
          <div v-for="(_, index) in join.peers" :key="index" class="peer-row">
            <n-input
              :value="join.peers[index]"
              placeholder="tcp://1.2.3.4:11010"
              @update:value="(v: string) => (join.peers[index] = v)"
            />
            <n-button size="small" quaternary @click="removePeer(index)">删除</n-button>
          </div>
        </div>
        <div v-else class="faint hint">未配置对端地址</div>
      </div>

      <div class="form">
        <label class="field">
          <span class="faint">长期密钥</span>
          <n-input
            v-model:value="join.networkSecret"
            type="password"
            show-password-on="click"
            :placeholder="config.view?.join.hasSecret ? '已配置，留空保持不变' : '未配置'"
          />
        </label>
        <label class="field">
          <span class="faint">临时凭据</span>
          <n-input
            v-model:value="join.credential"
            type="password"
            show-password-on="click"
            :placeholder="config.view?.join.hasCredential ? '已配置，留空保持不变' : '未配置（优先于长期密钥）'"
          />
        </label>
      </div>

      <template #action>
        <div class="actions">
          <n-button size="small" @click="syncDraft">重置</n-button>
          <n-button
            size="small"
            type="primary"
            :loading="savingJoin"
            :disabled="!joinDirty"
            @click="saveJoin"
          >
            保存连接参数
          </n-button>
        </div>
      </template>
    </n-card>

    <n-card title="高级（EasyTier）" size="small">
      <template #header-extra>
        <n-tag v-if="easytierDirty" size="small" type="warning">有未保存修改</n-tag>
      </template>
      <div class="form">
        <label class="field">
          <span class="faint">二进制目录</span>
          <n-input v-model:value="easytier.binDir" placeholder="留空 = <数据目录>/bin" />
        </label>
        <label class="field">
          <span class="faint">本地 RPC 地址</span>
          <n-input v-model:value="easytier.rpcAddr" placeholder="留空 = 127.0.0.1:15889" />
        </label>
      </div>
      <template #action>
        <div class="actions">
          <n-button size="small" @click="syncDraft">重置</n-button>
          <n-button
            size="small"
            type="primary"
            :loading="savingEasytier"
            :disabled="!easytierDirty"
            @click="saveEasytier"
          >
            保存高级设置
          </n-button>
        </div>
      </template>
    </n-card>
  </div>
</template>

<style scoped>
.page {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.hint {
  margin: 0 0 10px;
  font-size: 13px;
}

.drop {
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.import-actions {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
}

.file {
  position: relative;
  overflow: hidden;
  display: inline-flex;
  align-items: center;
  padding: 4px 12px;
  border-radius: 6px;
  border: 1px solid var(--color-border-strong);
  font-size: 13px;
  cursor: pointer;
}

.file input {
  position: absolute;
  inset: 0;
  opacity: 0;
  cursor: pointer;
}

.grow {
  flex: 1;
}

.form {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
  gap: 12px;
}

.field {
  display: flex;
  flex-direction: column;
  gap: 4px;
  font-size: 13px;
}

.field-block {
  margin-top: 14px;
}

.field-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 6px;
  font-size: 13px;
}

.peers {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.peer-row {
  display: flex;
  gap: 8px;
}

.actions {
  display: flex;
  justify-content: flex-end;
  gap: 8px;
}
</style>
