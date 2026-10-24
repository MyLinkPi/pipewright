<script setup lang="ts">
/*
  Databases.vue — 数据库在线管理(phpMyAdmin 式;顶部导航「数据库」)。

  左栏:实例选择 + 库列表(建库/删库);右侧按选中对象切内容:
    - schema:表列表 → 表详情(数据浏览 / 结构 两个子页签)
    - sql:SQL 控制台(选库执行;只读模式下写语句被服务端拒绝并提示解锁)
    - users:数据库账号管理(建号/改密/授权/回收/删除)
    - import:.sql 多语句导入(须已解锁)
  实例默认只读;解锁写模式是显式确认动作(服务端强校验 + 审计)。
  只读时写操作入口统一走 guardWrite:点击立刻弹解锁框(不让用户填完表单才被拒),
  解锁成功后自动继续原操作。
  导出(表/整库 × SQL/CSV)走流式附件下载。
*/
import { ref, computed, watch, onMounted } from 'vue'
import { useRoute } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { NIcon } from 'naive-ui'
import {
  Database, Lock, LockOpen, PlugConnected, Plus, Trash, Refresh,
  Pencil, Table as TableIcon, ListDetails, Code, Users, FileImport, Download,
  ChevronLeft, ChevronRight,
} from '@vicons/tabler'
import {
  listDBInstances, setDBInstanceReadonly, getDBServerInfo, testDBInstance,
  listDBDatabases, createDBDatabase, dropDBDatabase,
  listDBTables, getDBTableStructure, browseDBTable, getDBRow, insertDBRow, updateDBRow, deleteDBRow,
  dropDBTable, truncateDBTable, runDBQuery, importDBSQL, downloadDBExport,
  listDBUsers, createDBUser, setDBUserPassword, dropDBUser, getDBUserGrants, grantDBUser, revokeDBUser,
  type DBInstance, type DBServerInfo, type DatabaseInfo, type TableInfo, type TableStructure,
  type BrowseResult, type RowKey, type QueryResult, type DBUser, type GrantsView, type GrantTemplate,
} from '../api/dbAdmin'
import { HttpError } from '../api/http'
import { useToast } from '../composables/useToast'

const { t } = useI18n()
const toast = useToast()
const route = useRoute()

const errMsg = (err: unknown, fallback: string): string =>
  err instanceof HttpError ? (err.apiError?.message ?? fallback) : fallback

// ─── 实例与信息 ────────────────────────────────────────────────────────────────
const instances = ref<DBInstance[]>([])
const currentId = ref('')
const current = computed<DBInstance | null>(() => instances.value.find((i) => i.id === currentId.value) ?? null)
const info = ref<DBServerInfo | null>(null)
const loadingInstances = ref(true)
const testing = ref(false)

async function loadInstances(): Promise<void> {
  loadingInstances.value = true
  try {
    const res = await listDBInstances()
    instances.value = res.items
    const wanted = String(route.query.instance ?? '')
    currentId.value = instances.value.some((i) => i.id === wanted) ? wanted : (instances.value[0]?.id ?? '')
  } catch (err) {
    toast.error(t('dbAdmin.errLoadInstances'), { detail: errMsg(err, t('dbAdmin.errNetwork')) })
  } finally {
    loadingInstances.value = false
  }
}

async function runTest(): Promise<void> {
  if (!current.value || testing.value) return
  testing.value = true
  try {
    const res = await testDBInstance(current.value.id)
    if (res.ok) toast.success(t('dbAdmin.testOk'), { detail: `${res.version} · ${res.latencyMs}ms` })
    else toast.error(t('dbAdmin.testFail'), { detail: res.error })
  } catch (err) {
    toast.error(t('dbAdmin.testFail'), { detail: errMsg(err, t('dbAdmin.errNetwork')) })
  } finally {
    testing.value = false
  }
}

function formatUptime(sec: number): string {
  if (!sec) return '—'
  const d = Math.floor(sec / 86400)
  const h = Math.floor((sec % 86400) / 3600)
  const m = Math.floor((sec % 3600) / 60)
  if (d > 0) return t('dbAdmin.uptimeDays', { d, h })
  if (h > 0) return t('dbAdmin.uptimeHours', { h, m })
  return t('dbAdmin.uptimeMinutes', { m })
}

function formatBytes(n: number): string {
  if (!n) return '—'
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KiB`
  if (n < 1024 * 1024 * 1024) return `${(n / 1024 / 1024).toFixed(1)} MiB`
  return `${(n / 1024 / 1024 / 1024).toFixed(2)} GiB`
}

// ─── 解锁/上锁写模式 ───────────────────────────────────────────────────────────
const unlockOpen = ref(false)
const readonlyBusy = ref(false)
// 只读时被拦下的写操作:解锁确认后自动继续,不让用户白点/白填。
const pendingWrite = ref<(() => void) | null>(null)
const pendingWriteLabel = ref('')

function askUnlock(): void {
  unlockOpen.value = true
}

// 写操作入口统一守卫:只读 → 立刻弹解锁框(而非放行后提交时才 403)。
function guardWrite(action: () => void, label: string): void {
  if (!current.value) return
  if (current.value.readonly) {
    pendingWrite.value = action
    pendingWriteLabel.value = label
    unlockOpen.value = true
    return
  }
  action()
}

function cancelUnlock(): void {
  unlockOpen.value = false
  pendingWrite.value = null
  pendingWriteLabel.value = ''
}

async function confirmUnlock(): Promise<void> {
  if (!current.value) return
  readonlyBusy.value = true
  try {
    const updated = await setDBInstanceReadonly(current.value.id, false)
    const idx = instances.value.findIndex((i) => i.id === updated.id)
    if (idx >= 0) instances.value[idx] = updated
    unlockOpen.value = false
    const next = pendingWrite.value
    pendingWrite.value = null
    pendingWriteLabel.value = ''
    toast.success(t('dbAdmin.unlocked'))
    if (next) next()
  } catch (err) {
    toast.error(t('dbAdmin.unlockFail'), { detail: errMsg(err, t('dbAdmin.errNetwork')) })
  } finally {
    readonlyBusy.value = false
  }
}

async function relock(): Promise<void> {
  if (!current.value || readonlyBusy.value) return
  readonlyBusy.value = true
  try {
    const updated = await setDBInstanceReadonly(current.value.id, true)
    const idx = instances.value.findIndex((i) => i.id === updated.id)
    if (idx >= 0) instances.value[idx] = updated
    toast.success(t('dbAdmin.relocked'))
  } catch (err) {
    toast.error(t('dbAdmin.relockFail'), { detail: errMsg(err, t('dbAdmin.errNetwork')) })
  } finally {
    readonlyBusy.value = false
  }
}

// ─── 库列表 ────────────────────────────────────────────────────────────────────
const tab = ref<'schema' | 'sql' | 'users' | 'import'>('schema')
const databases = ref<DatabaseInfo[]>([])
const currentDb = ref('')
const loadingDbs = ref(false)

async function loadDatabases(): Promise<void> {
  if (!current.value) return
  loadingDbs.value = true
  try {
    const res = await listDBDatabases(current.value.id)
    databases.value = res.items
    if (!res.items.some((d) => d.name === currentDb.value)) currentDb.value = ''
  } catch (err) {
    databases.value = []
    toast.error(t('dbAdmin.errLoadDatabases'), { detail: errMsg(err, t('dbAdmin.errNetwork')) })
  } finally {
    loadingDbs.value = false
  }
}

const createDbOpen = ref(false)
const createDbName = ref('')
const createDbBusy = ref(false)

async function confirmCreateDb(): Promise<void> {
  if (!current.value || !createDbName.value.trim()) return
  createDbBusy.value = true
  try {
    await createDBDatabase(current.value.id, createDbName.value.trim())
    createDbOpen.value = false
    createDbName.value = ''
    await loadDatabases()
    toast.success(t('dbAdmin.dbCreated'))
  } catch (err) {
    toast.error(t('dbAdmin.dbCreateFail'), { detail: errMsg(err, t('dbAdmin.errNetwork')) })
  } finally {
    createDbBusy.value = false
  }
}

const dropDbTarget = ref<DatabaseInfo | null>(null)
const dropDbBusy = ref(false)
const dropDbConfirmName = ref('')

async function confirmDropDb(): Promise<void> {
  if (!current.value || !dropDbTarget.value) return
  if (dropDbConfirmName.value.trim() !== dropDbTarget.value.name) return
  dropDbBusy.value = true
  const dropped = dropDbTarget.value.name
  try {
    await dropDBDatabase(current.value.id, dropped)
    dropDbTarget.value = null
    dropDbConfirmName.value = ''
    if (currentDb.value === dropped) {
      currentDb.value = ''
      currentTable.value = ''
      structure.value = null
      browse.value = null
    }
    await loadDatabases()
    toast.success(t('dbAdmin.dbDropped'))
  } catch (err) {
    toast.error(t('dbAdmin.dbDropFail'), { detail: errMsg(err, t('dbAdmin.errNetwork')) })
  } finally {
    dropDbBusy.value = false
  }
}

// ─── 表列表与表详情 ────────────────────────────────────────────────────────────
const tables = ref<TableInfo[]>([])
const currentTable = ref('')
const tableTab = ref<'data' | 'structure'>('data')
const structure = ref<TableStructure | null>(null)
const loadingTables = ref(false)

async function selectDb(name: string): Promise<void> {
  currentDb.value = name
  currentTable.value = ''
  structure.value = null
  await loadTables()
}

async function loadTables(): Promise<void> {
  if (!current.value || !currentDb.value) return
  loadingTables.value = true
  try {
    const res = await listDBTables(current.value.id, currentDb.value)
    tables.value = res.items
  } catch (err) {
    tables.value = []
    toast.error(t('dbAdmin.errLoadTables'), { detail: errMsg(err, t('dbAdmin.errNetwork')) })
  } finally {
    loadingTables.value = false
  }
}

async function selectTable(ref: string): Promise<void> {
  currentTable.value = ref
  tableTab.value = 'data'
  page.value = 0
  sortCol.value = ''
  where.value = ''
  await loadTableDetail()
}

async function loadTableDetail(): Promise<void> {
  if (!current.value || !currentDb.value || !currentTable.value) return
  try {
    structure.value = await getDBTableStructure(current.value.id, currentDb.value, currentTable.value)
  } catch (err) {
    structure.value = null
    toast.error(t('dbAdmin.errLoadStructure'), { detail: errMsg(err, t('dbAdmin.errNetwork')) })
    return
  }
  if (tableTab.value === 'data') await loadBrowse()
}

// ─── 数据浏览 ──────────────────────────────────────────────────────────────────
const browse = ref<BrowseResult | null>(null)
const browsing = ref(false)
const pageSize = ref(50)
const page = ref(0)
const sortCol = ref('')
const sortDir = ref<'asc' | 'desc'>('asc')
const where = ref('')

const totalPages = computed(() => (browse.value ? Math.max(1, Math.ceil(browse.value.total / pageSize.value)) : 1))

async function loadBrowse(): Promise<void> {
  if (!current.value || !currentDb.value || !currentTable.value) return
  browsing.value = true
  try {
    browse.value = await browseDBTable(current.value.id, currentDb.value, currentTable.value, {
      limit: pageSize.value,
      offset: page.value * pageSize.value,
      ...(sortCol.value ? { sort: sortCol.value, dir: sortDir.value } : {}),
      ...(where.value.trim() ? { where: where.value.trim() } : {}),
    })
  } catch (err) {
    browse.value = null
    toast.error(t('dbAdmin.errBrowse'), { detail: errMsg(err, t('dbAdmin.errNetwork')) })
  } finally {
    browsing.value = false
  }
}

function toggleSort(col: string): void {
  if (sortCol.value === col) {
    sortDir.value = sortDir.value === 'asc' ? 'desc' : 'asc'
  } else {
    sortCol.value = col
    sortDir.value = 'asc'
  }
  page.value = 0
  void loadBrowse()
}

function applyFilter(): void {
  page.value = 0
  void loadBrowse()
}

function rowKeyFor(row: unknown[]): RowKey | null {
  if (!structure.value || structure.value.primaryKeys.length === 0) return null
  const cols = structure.value.columns.map((c) => c.name)
  const values: string[] = []
  for (const pk of structure.value.primaryKeys) {
    const i = cols.indexOf(pk)
    if (i < 0 || row[i] === null || row[i] === undefined) return null
    values.push(String(row[i]))
  }
  return { columns: [...structure.value.primaryKeys], values }
}

function displayCell(v: unknown): string {
  if (v === null || v === undefined) return 'NULL'
  if (typeof v === 'boolean') return v ? 'true' : 'false'
  return String(v)
}

// ─── 行编辑(插入/更新)与删除 ─────────────────────────────────────────────────
interface RowField {
  name: string
  dataType: string
  nullable: boolean
  isNull: boolean
  value: string
}

const rowEditorOpen = ref(false)
const rowEditorMode = ref<'insert' | 'update'>('insert')
const rowFields = ref<RowField[]>([])
const rowEditorBusy = ref(false)
const rowEditorKey = ref<RowKey | null>(null)

function fieldsFromStructure(): RowField[] {
  return (structure.value?.columns ?? []).map((c) => ({
    name: c.name,
    dataType: c.dataType,
    nullable: c.nullable,
    isNull: false,
    value: '',
  }))
}

function openInsert(): void {
  rowEditorMode.value = 'insert'
  rowFields.value = fieldsFromStructure()
  rowEditorKey.value = null
  rowEditorOpen.value = true
}

async function openEdit(row: unknown[]): Promise<void> {
  const key = rowKeyFor(row)
  if (!current.value || !key) return
  try {
    const res = await getDBRow(current.value.id, currentDb.value, currentTable.value, key)
    rowEditorMode.value = 'update'
    rowEditorKey.value = key
    rowFields.value = (structure.value?.columns ?? []).map((c) => {
      const v = res.values[c.name]
      const isNull = v === null || v === undefined
      return {
        name: c.name,
        dataType: c.dataType,
        nullable: c.nullable,
        isNull,
        value: isNull ? '' : displayCell(v),
      }
    })
    rowEditorOpen.value = true
  } catch (err) {
    toast.error(t('dbAdmin.errLoadRow'), { detail: errMsg(err, t('dbAdmin.errNetwork')) })
  }
}

async function submitRow(): Promise<void> {
  if (!current.value) return
  const values: Record<string, unknown> = {}
  for (const f of rowFields.value) {
    if (f.isNull) values[f.name] = null
    else if (f.value.trim() !== '') values[f.name] = f.value
    // 空串且未勾 NULL:跳过(插入走默认值;更新不改动该列)。
  }
  if (Object.keys(values).length === 0) return
  rowEditorBusy.value = true
  try {
    if (rowEditorMode.value === 'insert') {
      await insertDBRow(current.value.id, currentDb.value, currentTable.value, values)
      toast.success(t('dbAdmin.rowInserted'))
    } else if (rowEditorKey.value) {
      await updateDBRow(current.value.id, currentDb.value, currentTable.value, rowEditorKey.value, values)
      toast.success(t('dbAdmin.rowUpdated'))
    }
    rowEditorOpen.value = false
    await loadBrowse()
  } catch (err) {
    toast.error(t('dbAdmin.rowWriteFail'), { detail: errMsg(err, t('dbAdmin.errNetwork')) })
  } finally {
    rowEditorBusy.value = false
  }
}

const deleteRowTarget = ref<RowKey | null>(null)
const deleteRowBusy = ref(false)

async function confirmDeleteRow(): Promise<void> {
  if (!current.value || !deleteRowTarget.value) return
  deleteRowBusy.value = true
  try {
    await deleteDBRow(current.value.id, currentDb.value, currentTable.value, deleteRowTarget.value)
    deleteRowTarget.value = null
    toast.success(t('dbAdmin.rowDeleted'))
    await loadBrowse()
  } catch (err) {
    toast.error(t('dbAdmin.rowDeleteFail'), { detail: errMsg(err, t('dbAdmin.errNetwork')) })
  } finally {
    deleteRowBusy.value = false
  }
}

// ─── 表级操作(清空/删除/导出)────────────────────────────────────────────────
const tableAction = ref<'truncate' | 'drop' | null>(null)
const tableActionBusy = ref(false)

async function confirmTableAction(): Promise<void> {
  if (!current.value || !tableAction.value) return
  tableActionBusy.value = true
  try {
    if (tableAction.value === 'truncate') {
      await truncateDBTable(current.value.id, currentDb.value, currentTable.value)
      toast.success(t('dbAdmin.tableTruncated'))
    } else {
      await dropDBTable(current.value.id, currentDb.value, currentTable.value)
      toast.success(t('dbAdmin.tableDropped'))
      currentTable.value = ''
      structure.value = null
      browse.value = null
      await loadTables()
    }
    tableAction.value = null
    if (currentTable.value) await loadBrowse()
  } catch (err) {
    toast.error(t('dbAdmin.tableActionFail'), { detail: errMsg(err, t('dbAdmin.errNetwork')) })
  } finally {
    tableActionBusy.value = false
  }
}

const exporting = ref(false)

async function doExport(format: 'sql' | 'csv', table?: string): Promise<void> {
  if (!current.value || !currentDb.value || exporting.value) return
  exporting.value = true
  try {
    await downloadDBExport(current.value.id, { database: currentDb.value, ...(table ? { table } : {}), format })
    toast.success(t('dbAdmin.exportDone'))
  } catch (err) {
    toast.error(t('dbAdmin.exportFail'), { detail: errMsg(err, t('dbAdmin.errNetwork')) })
  } finally {
    exporting.value = false
  }
}

// ─── SQL 控制台 ────────────────────────────────────────────────────────────────
const sqlText = ref('')
const sqlDb = ref('')
const sqlRunning = ref(false)
const sqlResult = ref<QueryResult | null>(null)
const sqlError = ref('')

async function runSql(): Promise<void> {
  if (!current.value || !sqlText.value.trim() || sqlRunning.value) return
  sqlRunning.value = true
  sqlResult.value = null
  sqlError.value = ''
  try {
    sqlResult.value = await runDBQuery(current.value.id, sqlDb.value, sqlText.value)
  } catch (err) {
    sqlError.value = errMsg(err, t('dbAdmin.errNetwork'))
  } finally {
    sqlRunning.value = false
  }
}

// ─── 用户管理 ──────────────────────────────────────────────────────────────────
const users = ref<DBUser[]>([])
const loadingUsers = ref(false)
const selectedUser = ref<DBUser | null>(null)
const grantsView = ref<GrantsView | null>(null)
const grantsDb = ref('')

async function loadUsers(): Promise<void> {
  if (!current.value) return
  loadingUsers.value = true
  try {
    const res = await listDBUsers(current.value.id)
    users.value = res.items
  } catch (err) {
    users.value = []
    toast.error(t('dbAdmin.errLoadUsers'), { detail: errMsg(err, t('dbAdmin.errNetwork')) })
  } finally {
    loadingUsers.value = false
  }
}

async function selectUser(u: DBUser): Promise<void> {
  selectedUser.value = u
  grantsView.value = null
  if (!current.value) return
  try {
    grantsView.value = await getDBUserGrants(current.value.id, u.name, u.host)
    if (engine.value === 'postgres' && grantsView.value.rows?.length && !grantsDb.value) {
      grantsDb.value = grantsView.value.rows[0].database
    }
  } catch (err) {
    toast.error(t('dbAdmin.errLoadGrants'), { detail: errMsg(err, t('dbAdmin.errNetwork')) })
  }
}

const engine = computed(() => current.value?.engine ?? 'mysql')

const userModal = ref<'create' | 'password' | null>(null)
const userForm = ref({ name: '', host: '%', password: '' })
const userBusy = ref(false)

function openUserModal(mode: 'create' | 'password', u?: DBUser): void {
  userModal.value = mode
  userForm.value = { name: u?.name ?? '', host: u?.host ?? (engine.value === 'mysql' ? '%' : ''), password: '' }
}

function genPassword(): string {
  const chars = 'ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz23456789'
  let out = ''
  const buf = new Uint32Array(24)
  crypto.getRandomValues(buf)
  for (let i = 0; i < 24; i++) out += chars[buf[i] % chars.length]
  return out
}

async function submitUserModal(): Promise<void> {
  if (!current.value || !userForm.value.name.trim() || !userForm.value.password) return
  userBusy.value = true
  try {
    if (userModal.value === 'create') {
      await createDBUser(current.value.id, {
        name: userForm.value.name.trim(),
        host: engine.value === 'mysql' ? userForm.value.host.trim() : undefined,
        password: userForm.value.password,
      })
      toast.success(t('dbAdmin.userCreated'))
      await loadUsers()
    } else {
      await setDBUserPassword(current.value.id, {
        name: userForm.value.name.trim(),
        host: engine.value === 'mysql' ? userForm.value.host.trim() : undefined,
        password: userForm.value.password,
      })
      toast.success(t('dbAdmin.userPasswordSet'))
    }
    userModal.value = null
  } catch (err) {
    toast.error(t('dbAdmin.userOpFail'), { detail: errMsg(err, t('dbAdmin.errNetwork')) })
  } finally {
    userBusy.value = false
  }
}

const grantBusy = ref(false)
const grantTemplate = ref<GrantTemplate>('read')

async function doGrant(template: GrantTemplate): Promise<void> {
  if (!current.value || !selectedUser.value || !grantsDb.value) return
  grantBusy.value = true
  try {
    await grantDBUser(current.value.id, {
      name: selectedUser.value.name,
      host: selectedUser.value.host,
      database: grantsDb.value,
      template,
    })
    toast.success(t('dbAdmin.grantDone'))
    await selectUser(selectedUser.value)
  } catch (err) {
    toast.error(t('dbAdmin.grantFail'), { detail: errMsg(err, t('dbAdmin.errNetwork')) })
  } finally {
    grantBusy.value = false
  }
}

async function doRevoke(): Promise<void> {
  if (!current.value || !selectedUser.value || !grantsDb.value) return
  grantBusy.value = true
  try {
    await revokeDBUser(current.value.id, {
      name: selectedUser.value.name,
      host: selectedUser.value.host,
      database: grantsDb.value,
    })
    toast.success(t('dbAdmin.revokeDone'))
    await selectUser(selectedUser.value)
  } catch (err) {
    toast.error(t('dbAdmin.revokeFail'), { detail: errMsg(err, t('dbAdmin.errNetwork')) })
  } finally {
    grantBusy.value = false
  }
}

const dropUserTarget = ref<DBUser | null>(null)
const dropUserBusy = ref(false)

async function confirmDropUser(): Promise<void> {
  if (!current.value || !dropUserTarget.value) return
  dropUserBusy.value = true
  try {
    await dropDBUser(current.value.id, { name: dropUserTarget.value.name, host: dropUserTarget.value.host })
    dropUserTarget.value = null
    selectedUser.value = null
    grantsView.value = null
    toast.success(t('dbAdmin.userDropped'))
    await loadUsers()
  } catch (err) {
    toast.error(t('dbAdmin.userDropFail'), { detail: errMsg(err, t('dbAdmin.errNetwork')) })
  } finally {
    dropUserBusy.value = false
  }
}

// ─── 导入 ──────────────────────────────────────────────────────────────────────
const importDb = ref('')
const importText = ref('')
const importRunning = ref(false)
const importMessage = ref('')

function onImportFile(ev: Event): void {
  const file = (ev.target as HTMLInputElement).files?.[0]
  if (!file) return
  const reader = new FileReader()
  reader.onload = () => {
    importText.value = String(reader.result ?? '')
  }
  reader.readAsText(file)
}

async function runImport(): Promise<void> {
  if (!current.value || !importDb.value || !importText.value.trim() || importRunning.value) return
  importRunning.value = true
  importMessage.value = ''
  try {
    const res = await importDBSQL(current.value.id, importDb.value, importText.value)
    importMessage.value = res.message
    toast.success(t('dbAdmin.importDone'))
    await loadDatabases()
  } catch (err) {
    importMessage.value = errMsg(err, t('dbAdmin.errNetwork'))
    toast.error(t('dbAdmin.importFail'), { detail: importMessage.value })
  } finally {
    importRunning.value = false
  }
}

// ─── 生命周期与联动 ────────────────────────────────────────────────────────────
onMounted(async () => {
  await loadInstances()
})

watch(currentId, async (id) => {
  info.value = null
  databases.value = []
  currentDb.value = ''
  currentTable.value = ''
  structure.value = null
  browse.value = null
  users.value = []
  selectedUser.value = null
  grantsView.value = null
  sqlResult.value = null
  pendingWrite.value = null
  pendingWriteLabel.value = ''
  if (!id) return
  try {
    info.value = await getDBServerInfo(id)
  } catch (err) {
    // 容器可能仍在初始化:信息条显示失败原因,不打断页面。
    toast.error(t('dbAdmin.errInfo'), { detail: errMsg(err, t('dbAdmin.errNetwork')) })
  }
  await loadDatabases()
  sqlDb.value = ''
})

watch(tab, (v) => {
  if (v === 'users' && users.value.length === 0) void loadUsers()
  if (v === 'import' && !importDb.value && currentDb.value) importDb.value = currentDb.value
})

watch(currentDb, (db) => {
  if (db && !sqlDb.value) sqlDb.value = db
  if (db && !importDb.value) importDb.value = db
})
</script>

<template>
  <div class="dba-view">
    <header class="view-header">
      <div class="header-main">
        <h1 class="view-title">{{ t('dbAdmin.title') }}</h1>
        <p class="view-sub">{{ t('dbAdmin.subtitle') }}</p>
      </div>
      <div v-if="current" class="header-tools">
        <span v-if="info" class="info-chip mono">{{ info.version }} · {{ formatUptime(info.uptimeSeconds) }}</span>
        <span class="mode-chip" :class="current.readonly ? 'mode-chip--ro' : 'mode-chip--rw'">
          <NIcon :size="13"><Lock v-if="current.readonly" /><LockOpen v-else /></NIcon>
          {{ current.readonly ? t('dbAdmin.readonlyMode') : t('dbAdmin.writeMode') }}
        </span>
        <button v-if="current.readonly" class="btn-secondary" @click="askUnlock">{{ t('dbAdmin.unlock') }}</button>
        <button v-else class="btn-secondary" :disabled="readonlyBusy" @click="relock">{{ t('dbAdmin.relock') }}</button>
        <button class="btn-secondary" :disabled="testing" @click="runTest">
          <NIcon :size="13"><PlugConnected /></NIcon>
          {{ t('dbAdmin.test') }}
        </button>
      </div>
    </header>

    <!-- 无实例:引导去设置页登记 -->
    <div v-if="!loadingInstances && instances.length === 0" class="empty-panel">
      <NIcon :size="26"><Database /></NIcon>
      <p class="empty-title">{{ t('dbAdmin.noInstances') }}</p>
      <p class="empty-desc">{{ t('dbAdmin.noInstancesHint') }}</p>
      <router-link class="btn-primary" to="/settings/databases">{{ t('dbAdmin.goRegister') }}</router-link>
    </div>

    <template v-else-if="current">
      <!-- 实例选择条 -->
      <div class="instance-bar">
        <label class="instance-label">{{ t('dbAdmin.instance') }}</label>
        <select v-model="currentId" class="instance-select">
          <option v-for="i in instances" :key="i.id" :value="i.id">
            {{ i.name }}({{ i.engine }} · {{ i.host }}:{{ i.port }})
          </option>
        </select>
        <span class="instance-meta mono">{{ current.username }}@{{ current.host }}:{{ current.port }}</span>
      </div>

      <div class="dba-layout">
        <!-- 左栏:库列表 -->
        <aside class="db-sidebar">
          <div class="sidebar-head">
            <span>{{ t('dbAdmin.databases') }}</span>
            <button class="icon-btn" :title="t('dbAdmin.createDb')" @click="guardWrite(() => { createDbOpen = true }, t('dbAdmin.createDb'))">
              <NIcon :size="14"><Plus /></NIcon>
            </button>
          </div>
          <div v-if="loadingDbs" class="sidebar-empty">{{ t('common.refresh') }}…</div>
          <div v-else-if="databases.length === 0" class="sidebar-empty">{{ t('dbAdmin.noDatabases') }}</div>
          <ul v-else class="db-list">
            <li v-for="d in databases" :key="d.name">
              <button
                class="db-item"
                :class="{ 'db-item--active': d.name === currentDb }"
                @click="selectDb(d.name)"
              >
                <NIcon :size="13"><Database /></NIcon>
                <span class="db-item-name mono">{{ d.name }}</span>
                <span class="db-item-meta">{{ formatBytes(d.sizeBytes) }}</span>
              </button>
              <button
                v-if="d.name === currentDb"
                class="icon-btn icon-btn--danger"
                :title="t('dbAdmin.dropDb')"
                @click="guardWrite(() => { dropDbTarget = d }, t('dbAdmin.dropDb'))"
              >
                <NIcon :size="12"><Trash /></NIcon>
              </button>
            </li>
          </ul>
        </aside>

        <!-- 主区:页签 -->
        <section class="dba-main">
          <nav class="dba-tabs" role="tablist">
            <button class="dba-tab" :class="{ 'dba-tab--active': tab === 'schema' }" :disabled="!currentDb" @click="tab = 'schema'">
              <NIcon :size="14"><TableIcon /></NIcon>{{ t('dbAdmin.tabSchema') }}
            </button>
            <button class="dba-tab" :class="{ 'dba-tab--active': tab === 'sql' }" @click="tab = 'sql'">
              <NIcon :size="14"><Code /></NIcon>{{ t('dbAdmin.tabSql') }}
            </button>
            <button class="dba-tab" :class="{ 'dba-tab--active': tab === 'users' }" @click="tab = 'users'">
              <NIcon :size="14"><Users /></NIcon>{{ t('dbAdmin.tabUsers') }}
            </button>
            <button class="dba-tab" :class="{ 'dba-tab--active': tab === 'import' }" @click="tab = 'import'">
              <NIcon :size="14"><FileImport /></NIcon>{{ t('dbAdmin.tabImport') }}
            </button>
          </nav>

          <!-- 表列表 + 表详情 -->
          <div v-if="tab === 'schema'" class="tab-body">
            <div v-if="!currentDb" class="tab-empty">{{ t('dbAdmin.pickDatabase') }}</div>
            <template v-else>
              <div v-if="!currentTable" class="tables-wrap">
                <div class="tables-toolbar">
                  <span class="tables-title">{{ t('dbAdmin.tables') }}({{ tables.length }})</span>
                  <div class="tables-actions">
                    <button class="btn-secondary btn-sm" :disabled="exporting" @click="doExport('sql')">
                      <NIcon :size="13"><Download /></NIcon>{{ t('dbAdmin.exportDbSql') }}
                    </button>
                    <button class="icon-btn" :title="t('common.refresh')" @click="loadTables">
                      <NIcon :size="14"><Refresh /></NIcon>
                    </button>
                  </div>
                </div>
                <div v-if="loadingTables" class="tab-empty">{{ t('common.refresh') }}…</div>
                <div v-else-if="tables.length === 0" class="tab-empty">{{ t('dbAdmin.noTables') }}</div>
                <table v-else class="data-table">
                  <thead>
                    <tr>
                      <th>{{ t('dbAdmin.colTable') }}</th>
                      <th>{{ t('dbAdmin.colRows') }}</th>
                      <th>{{ t('dbAdmin.colSize') }}</th>
                      <th v-if="engine === 'mysql'">{{ t('dbAdmin.colEngine') }}</th>
                      <th v-else>{{ t('dbAdmin.colSchema') }}</th>
                      <th>{{ t('dbAdmin.colComment') }}</th>
                    </tr>
                  </thead>
                  <tbody>
                    <tr v-for="tb in tables" :key="tb.ref" class="row-click" @click="selectTable(tb.ref)">
                      <td class="mono cell-strong">{{ tb.name }}</td>
                      <td>{{ tb.rows }}</td>
                      <td>{{ formatBytes(tb.sizeBytes) }}</td>
                      <td v-if="engine === 'mysql'">{{ tb.engine || '—' }}</td>
                      <td v-else class="mono">{{ tb.schema }}</td>
                      <td class="cell-dim">{{ tb.comment || '—' }}</td>
                    </tr>
                  </tbody>
                </table>
              </div>

              <!-- 表详情:数据 / 结构 -->
              <div v-else class="table-detail">
                <div class="detail-head">
                  <button class="btn-link" @click="currentTable = ''; structure = null; browse = null">← {{ t('dbAdmin.backToTables') }}</button>
                  <strong class="mono detail-name">{{ currentTable }}</strong>
                  <nav class="subtabs">
                    <button class="subtab" :class="{ 'subtab--active': tableTab === 'data' }" @click="tableTab = 'data'; loadBrowse()">{{ t('dbAdmin.subData') }}</button>
                    <button class="subtab" :class="{ 'subtab--active': tableTab === 'structure' }" @click="tableTab = 'structure'">{{ t('dbAdmin.subStructure') }}</button>
                  </nav>
                  <span class="detail-actions">
                    <button class="btn-secondary btn-sm" @click="guardWrite(openInsert, t('dbAdmin.insertRow'))">
                      <NIcon :size="13"><Plus /></NIcon>{{ t('dbAdmin.insertRow') }}
                    </button>
                    <button class="btn-secondary btn-sm" :disabled="exporting" @click="doExport('sql', currentTable)">SQL</button>
                    <button class="btn-secondary btn-sm" :disabled="exporting" @click="doExport('csv', currentTable)">CSV</button>
                    <button class="btn-secondary btn-sm btn-danger-ghost" @click="guardWrite(() => { tableAction = 'truncate' }, t('dbAdmin.truncate'))">{{ t('dbAdmin.truncate') }}</button>
                    <button class="btn-secondary btn-sm btn-danger-ghost" @click="guardWrite(() => { tableAction = 'drop' }, t('dbAdmin.dropTable'))">{{ t('dbAdmin.dropTable') }}</button>
                  </span>
                </div>

                <!-- 数据浏览 -->
                <div v-if="tableTab === 'data'" class="browse-wrap">
                  <div class="filter-bar">
                    <input
                      v-model="where"
                      class="filter-input mono"
                      type="text"
                      :placeholder="t('dbAdmin.wherePlaceholder')"
                      @keyup.enter="applyFilter"
                    />
                    <button class="btn-secondary btn-sm" @click="applyFilter">{{ t('dbAdmin.applyFilter') }}</button>
                    <select v-model="pageSize" class="page-size" @change="page = 0; loadBrowse()">
                      <option :value="25">25</option>
                      <option :value="50">50</option>
                      <option :value="100">100</option>
                      <option :value="200">200</option>
                    </select>
                    <span v-if="browse" class="row-count">{{ t('dbAdmin.totalRows', { n: browse.total }) }}</span>
                  </div>

                  <div v-if="browsing" class="tab-empty">{{ t('common.refresh') }}…</div>
                  <div v-else-if="!browse || browse.rows.length === 0" class="tab-empty">{{ t('dbAdmin.noRows') }}</div>
                  <div v-else class="grid-scroll">
                    <table class="data-table data-table--grid">
                      <thead>
                        <tr>
                          <th class="cell-op-col" />
                          <th
                            v-for="c in browse.columns"
                            :key="c"
                            class="th-sortable"
                            @click="toggleSort(c)"
                          >
                            {{ c }}
                            <span v-if="sortCol === c" class="sort-mark">{{ sortDir === 'asc' ? '▲' : '▼' }}</span>
                          </th>
                        </tr>
                      </thead>
                      <tbody>
                        <tr v-for="(row, ri) in browse.rows" :key="ri">
                          <td class="cell-op-col">
                            <button
                              v-if="structure && structure.primaryKeys.length > 0"
                              class="icon-btn"
                              :title="t('dbAdmin.editRow')"
                              @click.stop="guardWrite(() => openEdit(row), t('dbAdmin.editRow'))"
                            >
                              <NIcon :size="13"><Pencil /></NIcon>
                            </button>
                            <button
                              v-if="structure && structure.primaryKeys.length > 0"
                              class="icon-btn icon-btn--danger"
                              :title="t('dbAdmin.deleteRow')"
                              @click.stop="guardWrite(() => { deleteRowTarget = rowKeyFor(row) }, t('dbAdmin.deleteRow'))"
                            >
                              <NIcon :size="13"><Trash /></NIcon>
                            </button>
                          </td>
                          <td v-for="(c, i) in browse.columns" :key="c" class="mono" :class="{ 'cell-null': row[i] === null }">
                            {{ displayCell(row[i]) }}
                          </td>
                        </tr>
                      </tbody>
                    </table>
                  </div>

                  <div class="pager">
                    <button class="btn-secondary btn-sm" :disabled="page === 0 || browsing" @click="page--; loadBrowse()">
                      <NIcon :size="13"><ChevronLeft /></NIcon>
                    </button>
                    <span class="pager-info">{{ page + 1 }} / {{ totalPages }}</span>
                    <button class="btn-secondary btn-sm" :disabled="page + 1 >= totalPages || browsing" @click="page++; loadBrowse()">
                      <NIcon :size="13"><ChevronRight /></NIcon>
                    </button>
                  </div>
                </div>

                <!-- 表结构 -->
                <div v-else class="structure-wrap">
                  <template v-if="structure">
                    <h3 class="block-title">{{ t('dbAdmin.columns') }}</h3>
                    <table class="data-table">
                      <thead>
                        <tr>
                          <th>{{ t('dbAdmin.colColumn') }}</th>
                          <th>{{ t('dbAdmin.colType') }}</th>
                          <th>{{ t('dbAdmin.colNullable') }}</th>
                          <th>{{ t('dbAdmin.colDefault') }}</th>
                          <th v-if="engine === 'mysql'">{{ t('dbAdmin.colExtra') }}</th>
                          <th v-if="engine === 'mysql'">{{ t('dbAdmin.colKey') }}</th>
                        </tr>
                      </thead>
                      <tbody>
                        <tr v-for="c in structure.columns" :key="c.name">
                          <td class="mono cell-strong">{{ c.name }}</td>
                          <td class="mono">{{ c.dataType }}</td>
                          <td>{{ c.nullable ? 'YES' : 'NO' }}</td>
                          <td class="mono cell-dim">{{ c.default ?? '—' }}</td>
                          <td v-if="engine === 'mysql'" class="cell-dim">{{ c.extra || '—' }}</td>
                          <td v-if="engine === 'mysql'" class="cell-dim">{{ c.key || '—' }}</td>
                        </tr>
                      </tbody>
                    </table>
                    <h3 class="block-title">{{ t('dbAdmin.indexes') }}</h3>
                    <table class="data-table">
                      <thead>
                        <tr>
                          <th>{{ t('dbAdmin.colIndexName') }}</th>
                          <th>{{ t('dbAdmin.colUnique') }}</th>
                          <th>{{ t('dbAdmin.colIndexCols') }}</th>
                        </tr>
                      </thead>
                      <tbody>
                        <tr v-for="idx in structure.indexes" :key="idx.name">
                          <td class="mono">{{ idx.name }}</td>
                          <td>{{ idx.unique ? '✔' : '—' }}</td>
                          <td class="mono cell-dim">{{ idx.definition || idx.columns.join(', ') }}</td>
                        </tr>
                      </tbody>
                    </table>
                    <p v-if="structure.primaryKeys.length === 0" class="hint-line">{{ t('dbAdmin.noPkHint') }}</p>
                  </template>
                </div>
              </div>
            </template>
          </div>

          <!-- SQL 控制台 -->
          <div v-else-if="tab === 'sql'" class="tab-body">
            <div class="filter-bar">
              <select v-model="sqlDb" class="page-size">
                <option value="">{{ t('dbAdmin.noDatabase') }}</option>
                <option v-for="d in databases" :key="d.name" :value="d.name">{{ d.name }}</option>
              </select>
              <span class="hint-inline">{{ t('dbAdmin.sqlHint') }}</span>
            </div>
            <textarea
              v-model="sqlText"
              class="sql-input mono"
              :placeholder="t('dbAdmin.sqlPlaceholder')"
              rows="8"
              spellcheck="false"
            />
            <div class="sql-actions">
              <button class="btn-primary" :disabled="sqlRunning || !sqlText.trim()" @click="runSql">
                <span v-if="sqlRunning" class="spinner" aria-hidden="true" />
                {{ sqlRunning ? t('dbAdmin.running') : t('dbAdmin.run') }}
              </button>
              <span v-if="current.readonly" class="hint-inline">{{ t('dbAdmin.readonlySqlHint') }}</span>
            </div>
            <div v-if="sqlError" class="sql-error" role="alert">{{ sqlError }}</div>
            <template v-if="sqlResult">
              <div v-if="sqlResult.isSelect" class="grid-scroll">
                <table v-if="sqlResult.rows && sqlResult.rows.length" class="data-table data-table--grid">
                  <thead>
                    <tr><th v-for="c in sqlResult.columns" :key="c">{{ c }}</th></tr>
                  </thead>
                  <tbody>
                    <tr v-for="(row, ri) in sqlResult.rows" :key="ri">
                      <td v-for="(c, i) in sqlResult.columns" :key="c" class="mono" :class="{ 'cell-null': row[i] === null }">{{ displayCell(row[i]) }}</td>
                    </tr>
                  </tbody>
                </table>
                <div v-else class="tab-empty">{{ t('dbAdmin.noRows') }}</div>
              </div>
              <p v-else class="affected-line">
                {{ t('dbAdmin.rowsAffected', { n: sqlResult.rowsAffected ?? 0 }) }}
                <span v-if="sqlResult.truncated" class="hint-inline">{{ t('dbAdmin.resultTruncated') }}</span>
              </p>
              <p v-if="sqlResult.truncated && sqlResult.isSelect" class="hint-line">{{ t('dbAdmin.resultTruncated') }}</p>
            </template>
          </div>

          <!-- 用户管理 -->
          <div v-else-if="tab === 'users'" class="tab-body">
            <div class="tables-toolbar">
              <span class="hint-inline">{{ engine === 'mysql' ? t('dbAdmin.usersHintMysql') : t('dbAdmin.usersHintPg') }}</span>
              <button class="btn-primary btn-sm" @click="guardWrite(() => openUserModal('create'), t('dbAdmin.createUser'))">
                <NIcon :size="13"><Plus /></NIcon>{{ t('dbAdmin.createUser') }}
              </button>
            </div>
            <div v-if="loadingUsers" class="tab-empty">{{ t('common.refresh') }}…</div>
            <div v-else-if="users.length === 0" class="tab-empty">{{ t('dbAdmin.noUsers') }}</div>
            <div v-else class="users-layout">
              <table class="data-table users-table">
                <thead>
                  <tr>
                    <th>{{ t('dbAdmin.colUser') }}</th>
                    <th v-if="engine === 'mysql'">Host</th>
                    <th v-else>{{ t('dbAdmin.colSuper') }}</th>
                  </tr>
                </thead>
                <tbody>
                  <tr
                    v-for="u in users"
                    :key="u.name + (u.host ?? '')"
                    class="row-click"
                    :class="{ 'row-selected': selectedUser === u }"
                    @click="selectUser(u)"
                  >
                    <td class="mono cell-strong">{{ u.name }}</td>
                    <td v-if="engine === 'mysql'" class="mono">{{ u.host }}</td>
                    <td v-else>{{ u.super ? '✔' : '—' }}</td>
                  </tr>
                </tbody>
              </table>

              <div v-if="selectedUser" class="grants-panel">
                <div class="grants-head">
                  <strong class="mono">{{ selectedUser.name }}{{ selectedUser.host ? '@' + selectedUser.host : '' }}</strong>
                  <span class="detail-actions">
                    <button class="btn-secondary btn-sm" @click="guardWrite(() => openUserModal('password', selectedUser ?? undefined), t('dbAdmin.setPassword'))">{{ t('dbAdmin.setPassword') }}</button>
                    <button class="btn-secondary btn-sm btn-danger-ghost" @click="guardWrite(() => { dropUserTarget = selectedUser }, t('dbAdmin.dropUser'))">{{ t('dbAdmin.dropUser') }}</button>
                  </span>
                </div>
                <div v-if="!grantsView" class="tab-empty">{{ t('common.refresh') }}…</div>
                <template v-else-if="engine === 'mysql'">
                  <ul class="grant-lines mono">
                    <li v-for="(line, li) in grantsView.lines" :key="li">{{ line }};</li>
                  </ul>
                </template>
                <template v-else>
                  <table class="data-table">
                    <thead>
                      <tr><th>{{ t('dbAdmin.colDatabase') }}</th><th>CONNECT</th></tr>
                    </thead>
                    <tbody>
                      <tr v-for="g in grantsView.rows" :key="g.database" :class="{ 'row-selected': g.database === grantsDb }" class="row-click" @click="grantsDb = g.database">
                        <td class="mono">{{ g.database }}</td>
                        <td>{{ g.connect ? '✔' : '—' }}</td>
                      </tr>
                    </tbody>
                  </table>
                  <div class="grant-actions">
                    <select v-model="grantTemplate" class="page-size">
                      <option value="all">{{ t('dbAdmin.grantAll') }}</option>
                      <option value="read">{{ t('dbAdmin.grantRead') }}</option>
                      <option value="crud">{{ t('dbAdmin.grantCrud') }}</option>
                    </select>
                    <button class="btn-secondary btn-sm" :disabled="!grantsDb || grantBusy" @click="guardWrite(() => doGrant(grantTemplate), t('dbAdmin.grant'))">{{ t('dbAdmin.grant') }}</button>
                    <button class="btn-secondary btn-sm btn-danger-ghost" :disabled="!grantsDb || grantBusy" @click="guardWrite(() => doRevoke(), t('dbAdmin.revoke'))">{{ t('dbAdmin.revoke') }}</button>
                  </div>
                </template>
                <p class="hint-line">{{ t('dbAdmin.grantNote') }}</p>
              </div>
            </div>
          </div>

          <!-- 导入 -->
          <div v-else-if="tab === 'import'" class="tab-body">
            <div class="filter-bar">
              <label class="instance-label">{{ t('dbAdmin.targetDatabase') }}</label>
              <select v-model="importDb" class="page-size">
                <option v-for="d in databases" :key="d.name" :value="d.name">{{ d.name }}</option>
              </select>
              <input type="file" accept=".sql,text/plain" @change="onImportFile" />
            </div>
            <textarea
              v-model="importText"
              class="sql-input mono"
              :placeholder="t('dbAdmin.importPlaceholder')"
              rows="10"
              spellcheck="false"
            />
            <div class="sql-actions">
              <button class="btn-primary" :disabled="!importDb || !importText.trim() || importRunning" @click="guardWrite(runImport, t('dbAdmin.runImport'))">
                <span v-if="importRunning" class="spinner" aria-hidden="true" />
                {{ importRunning ? t('dbAdmin.running') : t('dbAdmin.runImport') }}
              </button>
              <span v-if="current.readonly" class="hint-inline warn">{{ t('dbAdmin.readonlyImportHint') }}</span>
            </div>
            <p v-if="importMessage" class="affected-line mono">{{ importMessage }}</p>
            <p class="hint-line">{{ t('dbAdmin.importNote') }}</p>
          </div>
        </section>
      </div>
    </template>
  </div>

  <!-- 解锁确认 -->
  <Teleport to="body">
    <div v-if="unlockOpen && current" class="modal-scrim" role="dialog" :aria-label="t('dbAdmin.unlock')" aria-modal="true" @keydown.esc="cancelUnlock">
      <div class="modal modal--sm">
        <div class="modal-head">
          <div class="modal-icon modal-icon--danger"><NIcon :size="18"><LockOpen /></NIcon></div>
          <div>
            <h3 class="modal-title">{{ t('dbAdmin.unlockTitle') }}</h3>
            <p class="modal-sub">{{ t('dbAdmin.unlockBody') }}</p>
            <p v-if="pendingWriteLabel" class="modal-sub modal-sub--warn">{{ t('dbAdmin.unlockPendingHint', { action: pendingWriteLabel }) }}</p>
          </div>
        </div>
        <div class="modal-body">
          <p class="delete-text mono">{{ current.name }} · {{ current.host }}:{{ current.port }}</p>
        </div>
        <div class="modal-footer modal-footer--standalone">
          <button class="btn-secondary" :disabled="readonlyBusy" @click="cancelUnlock">{{ t('dbAdmin.cancel') }}</button>
          <button class="btn-danger" :disabled="readonlyBusy" @click="confirmUnlock">{{ t('dbAdmin.unlockConfirm') }}</button>
        </div>
      </div>
    </div>
  </Teleport>

  <!-- 建库 -->
  <Teleport to="body">
    <div v-if="createDbOpen" class="modal-scrim" role="dialog" :aria-label="t('dbAdmin.createDb')" aria-modal="true" @keydown.esc="createDbOpen = false">
      <div class="modal modal--sm">
        <div class="modal-head">
          <div class="modal-icon"><NIcon :size="18"><Database /></NIcon></div>
          <div>
            <h3 class="modal-title">{{ t('dbAdmin.createDb') }}</h3>
            <p class="modal-sub">{{ t('dbAdmin.createDbHint') }}</p>
          </div>
        </div>
        <div class="modal-body">
          <input v-model="createDbName" class="field-input mono" type="text" :placeholder="t('dbAdmin.dbNamePlaceholder')" @keyup.enter="confirmCreateDb" />
        </div>
        <div class="modal-footer modal-footer--standalone">
          <button class="btn-secondary" :disabled="createDbBusy" @click="createDbOpen = false">{{ t('dbAdmin.cancel') }}</button>
          <button class="btn-primary" :disabled="!createDbName.trim() || createDbBusy" @click="confirmCreateDb">{{ t('dbAdmin.create') }}</button>
        </div>
      </div>
    </div>
  </Teleport>

  <!-- 删库(输入库名确认)-->
  <Teleport to="body">
    <div v-if="dropDbTarget" class="modal-scrim" role="dialog" :aria-label="t('dbAdmin.dropDb')" aria-modal="true" @keydown.esc="dropDbTarget = null">
      <div class="modal modal--sm">
        <div class="modal-head">
          <div class="modal-icon modal-icon--danger"><NIcon :size="18"><Trash /></NIcon></div>
          <div>
            <h3 class="modal-title">{{ t('dbAdmin.dropDbTitle') }}</h3>
            <p class="modal-sub">{{ t('dbAdmin.dropDbBody') }}</p>
          </div>
        </div>
        <div class="modal-body">
          <p class="delete-text mono">{{ dropDbTarget.name }}</p>
          <input v-model="dropDbConfirmName" class="field-input mono" type="text" :placeholder="t('dbAdmin.dropDbConfirmPlaceholder')" />
        </div>
        <div class="modal-footer modal-footer--standalone">
          <button class="btn-secondary" :disabled="dropDbBusy" @click="dropDbTarget = null">{{ t('dbAdmin.cancel') }}</button>
          <button class="btn-danger" :disabled="dropDbConfirmName.trim() !== dropDbTarget.name || dropDbBusy" @click="confirmDropDb">{{ t('dbAdmin.dropDbConfirm') }}</button>
        </div>
      </div>
    </div>
  </Teleport>

  <!-- 行编辑 -->
  <Teleport to="body">
    <div v-if="rowEditorOpen" class="modal-scrim" role="dialog" :aria-label="t('dbAdmin.insertRow')" aria-modal="true" @keydown.esc="rowEditorOpen = false">
      <div class="modal">
        <div class="modal-head">
          <div class="modal-icon"><NIcon :size="18"><ListDetails /></NIcon></div>
          <div>
            <h3 class="modal-title">{{ rowEditorMode === 'insert' ? t('dbAdmin.insertRow') : t('dbAdmin.editRow') }} · <span class="mono">{{ currentTable }}</span></h3>
            <p class="modal-sub">{{ t('dbAdmin.rowEditorHint') }}</p>
          </div>
        </div>
        <div class="modal-form">
          <div v-for="f in rowFields" :key="f.name" class="row-field">
            <div class="row-field-label">
              <span class="mono">{{ f.name }}</span>
              <span class="row-field-type">{{ f.dataType }}</span>
            </div>
            <div class="row-field-input">
              <input
                v-model="f.value"
                class="field-input mono"
                type="text"
                :disabled="f.isNull"
                spellcheck="false"
              />
              <label v-if="f.nullable" class="check-inline">
                <input v-model="f.isNull" type="checkbox" /> NULL
              </label>
            </div>
          </div>
        </div>
        <div class="modal-footer modal-footer--standalone">
          <button class="btn-secondary" :disabled="rowEditorBusy" @click="rowEditorOpen = false">{{ t('dbAdmin.cancel') }}</button>
          <button class="btn-primary" :disabled="rowEditorBusy" @click="submitRow">{{ rowEditorMode === 'insert' ? t('dbAdmin.create') : t('dbAdmin.save') }}</button>
        </div>
      </div>
    </div>
  </Teleport>

  <!-- 删行确认 -->
  <Teleport to="body">
    <div v-if="deleteRowTarget" class="modal-scrim" role="dialog" :aria-label="t('dbAdmin.deleteRow')" aria-modal="true" @keydown.esc="deleteRowTarget = null">
      <div class="modal modal--sm">
        <div class="modal-head">
          <div class="modal-icon modal-icon--danger"><NIcon :size="18"><Trash /></NIcon></div>
          <div>
            <h3 class="modal-title">{{ t('dbAdmin.deleteRowConfirm') }}</h3>
            <p class="modal-sub mono">{{ deleteRowTarget.columns.map((c, i) => `${c}=${deleteRowTarget?.values[i]}`).join(', ') }}</p>
          </div>
        </div>
        <div class="modal-footer modal-footer--standalone">
          <button class="btn-secondary" :disabled="deleteRowBusy" @click="deleteRowTarget = null">{{ t('dbAdmin.cancel') }}</button>
          <button class="btn-danger" :disabled="deleteRowBusy" @click="confirmDeleteRow">{{ t('dbAdmin.delete') }}</button>
        </div>
      </div>
    </div>
  </Teleport>

  <!-- 清空/删表确认 -->
  <Teleport to="body">
    <div v-if="tableAction" class="modal-scrim" role="dialog" :aria-label="tableAction === 'drop' ? t('dbAdmin.dropTable') : t('dbAdmin.truncate')" aria-modal="true" @keydown.esc="tableAction = null">
      <div class="modal modal--sm">
        <div class="modal-head">
          <div class="modal-icon modal-icon--danger"><NIcon :size="18"><Trash /></NIcon></div>
          <div>
            <h3 class="modal-title">{{ tableAction === 'drop' ? t('dbAdmin.dropTableConfirm') : t('dbAdmin.truncateConfirm') }}</h3>
            <p class="modal-sub mono">{{ currentDb }} · {{ currentTable }}</p>
          </div>
        </div>
        <div class="modal-footer modal-footer--standalone">
          <button class="btn-secondary" :disabled="tableActionBusy" @click="tableAction = null">{{ t('dbAdmin.cancel') }}</button>
          <button class="btn-danger" :disabled="tableActionBusy" @click="confirmTableAction">{{ t('dbAdmin.confirm') }}</button>
        </div>
      </div>
    </div>
  </Teleport>

  <!-- 建号/改密 -->
  <Teleport to="body">
    <div v-if="userModal" class="modal-scrim" role="dialog" :aria-label="t('dbAdmin.createUser')" aria-modal="true" @keydown.esc="userModal = null">
      <div class="modal modal--sm">
        <div class="modal-head">
          <div class="modal-icon"><NIcon :size="18"><Users /></NIcon></div>
          <div>
            <h3 class="modal-title">{{ userModal === 'create' ? t('dbAdmin.createUser') : t('dbAdmin.setPassword') }}</h3>
            <p class="modal-sub">{{ t('dbAdmin.userModalHint') }}</p>
          </div>
        </div>
        <div class="modal-form">
          <div class="field">
            <label class="field-label">{{ t('dbAdmin.colUser') }}</label>
            <input v-model="userForm.name" class="field-input mono" type="text" :disabled="userModal === 'password'" spellcheck="false" />
          </div>
          <div v-if="engine === 'mysql'" class="field">
            <label class="field-label">Host</label>
            <input v-model="userForm.host" class="field-input mono" type="text" spellcheck="false" />
          </div>
          <div class="field">
            <label class="field-label">{{ t('dbAdmin.colPassword') }}</label>
            <div class="row-field-input">
              <input v-model="userForm.password" class="field-input mono" type="text" spellcheck="false" />
              <button class="btn-secondary btn-sm" type="button" @click="userForm.password = genPassword()">{{ t('dbAdmin.genPassword') }}</button>
            </div>
          </div>
        </div>
        <div class="modal-footer modal-footer--standalone">
          <button class="btn-secondary" :disabled="userBusy" @click="userModal = null">{{ t('dbAdmin.cancel') }}</button>
          <button class="btn-primary" :disabled="!userForm.name.trim() || !userForm.password || userBusy" @click="submitUserModal">{{ t('dbAdmin.save') }}</button>
        </div>
      </div>
    </div>
  </Teleport>

  <!-- 删号确认 -->
  <Teleport to="body">
    <div v-if="dropUserTarget" class="modal-scrim" role="dialog" :aria-label="t('dbAdmin.dropUser')" aria-modal="true" @keydown.esc="dropUserTarget = null">
      <div class="modal modal--sm">
        <div class="modal-head">
          <div class="modal-icon modal-icon--danger"><NIcon :size="18"><Trash /></NIcon></div>
          <div>
            <h3 class="modal-title">{{ t('dbAdmin.dropUserConfirm') }}</h3>
            <p class="modal-sub mono">{{ dropUserTarget.name }}{{ dropUserTarget.host ? '@' + dropUserTarget.host : '' }}</p>
          </div>
        </div>
        <div class="modal-footer modal-footer--standalone">
          <button class="btn-secondary" :disabled="dropUserBusy" @click="dropUserTarget = null">{{ t('dbAdmin.cancel') }}</button>
          <button class="btn-danger" :disabled="dropUserBusy" @click="confirmDropUser">{{ t('dbAdmin.delete') }}</button>
        </div>
      </div>
    </div>
  </Teleport>
</template>

<style scoped>
.dba-view {
  display: flex;
  flex-direction: column;
  gap: 18px;
}

.view-header {
  display: flex;
  align-items: flex-start;
  gap: 16px;
  padding-bottom: 18px;
  border-bottom: 1px solid var(--color-border);
}
.header-main {
  flex: 1;
}
.view-title {
  font-size: var(--text-display);
  font-weight: 700;
  letter-spacing: -0.02em;
  color: var(--color-text);
}
.view-sub {
  font-size: var(--text-body);
  color: var(--color-faint);
  margin-top: 2px;
}
.header-tools {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
}
.info-chip {
  font-size: 0.74rem;
  color: var(--color-dim);
  background: var(--color-inset);
  border: 1px solid var(--color-border);
  padding: 4px 10px;
  border-radius: var(--rounded-full);
}
.mode-chip {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-size: 0.74rem;
  font-weight: 600;
  padding: 4px 10px;
  border-radius: var(--rounded-full);
  border: 1px solid var(--color-border-strong);
}
.mode-chip--ro {
  color: var(--color-faint);
}
.mode-chip--rw {
  color: var(--color-green);
  border-color: var(--color-green);
}

.mono {
  font-family: var(--font-mono);
}

.empty-panel {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 10px;
  padding: 72px 32px;
  text-align: center;
  border: 1.5px dashed var(--color-border-strong);
  border-radius: var(--rounded-card);
  color: var(--color-dim);
}
.empty-title {
  font-size: 1rem;
  font-weight: 600;
  color: var(--color-text);
}
.empty-desc {
  font-size: 0.82rem;
  color: var(--color-faint);
  max-width: 48ch;
}

.instance-bar {
  display: flex;
  align-items: center;
  gap: 10px;
}
.instance-label {
  font-size: 0.8rem;
  font-weight: 500;
  color: var(--color-dim);
}
.instance-select {
  height: 34px;
  padding: 0 10px;
  background: var(--color-inset);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  color: var(--color-text);
  font-family: var(--font-sans);
  font-size: 0.84rem;
  max-width: 420px;
}
.instance-meta {
  font-size: 0.76rem;
  color: var(--color-faint);
}

.dba-layout {
  display: grid;
  grid-template-columns: 240px 1fr;
  gap: 16px;
  align-items: start;
}

.db-sidebar {
  background: var(--color-card);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-card);
  overflow: hidden;
}
.sidebar-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 11px 14px;
  font-size: 0.82rem;
  font-weight: 600;
  color: var(--color-text);
  border-bottom: 1px solid var(--color-border);
}
.sidebar-empty {
  padding: 22px 14px;
  font-size: 0.78rem;
  color: var(--color-faint);
  text-align: center;
}
.db-list {
  list-style: none;
  margin: 0;
  padding: 6px;
  max-height: 60vh;
  overflow-y: auto;
}
.db-list li {
  display: flex;
  align-items: center;
  gap: 4px;
}
.db-item {
  flex: 1;
  display: flex;
  align-items: center;
  gap: 7px;
  min-width: 0;
  padding: 7px 9px;
  border: none;
  background: transparent;
  border-radius: var(--rounded-md);
  color: var(--color-dim);
  cursor: pointer;
  font-size: 0.8rem;
  text-align: left;
  transition: background-color var(--duration-fast), color var(--duration-fast);
}
.db-item:hover {
  background: var(--color-inset);
  color: var(--color-text);
}
.db-item--active {
  background: var(--color-primary-soft);
  color: var(--color-primary);
  font-weight: 600;
}
.db-item-name {
  flex: 1;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.db-item-meta {
  font-size: 0.68rem;
  color: var(--color-faint);
  white-space: nowrap;
}

.dba-main {
  background: var(--color-card);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-card);
  overflow: hidden;
  min-height: 420px;
}
.dba-tabs {
  display: flex;
  gap: 2px;
  border-bottom: 1px solid var(--color-border);
  padding: 0 10px;
}
.dba-tab {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 11px 14px;
  border: none;
  background: transparent;
  color: var(--color-faint);
  font-size: 0.82rem;
  font-weight: 500;
  cursor: pointer;
  border-bottom: 2px solid transparent;
  margin-bottom: -1px;
  transition: color var(--duration-fast), border-color var(--duration-fast);
}
.dba-tab:hover:not(:disabled) {
  color: var(--color-dim);
}
.dba-tab--active {
  color: var(--color-text);
  font-weight: 600;
  border-bottom-color: var(--color-primary);
}
.dba-tab:disabled {
  opacity: 0.45;
  cursor: not-allowed;
}

.tab-body {
  padding: 16px;
  display: flex;
  flex-direction: column;
  gap: 14px;
}
.tab-empty {
  padding: 40px 16px;
  text-align: center;
  font-size: 0.82rem;
  color: var(--color-faint);
}

.tables-toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
}
.tables-title {
  font-size: 0.84rem;
  font-weight: 600;
  color: var(--color-text);
}
.tables-actions {
  display: flex;
  align-items: center;
  gap: 8px;
}

.data-table {
  width: 100%;
  border-collapse: collapse;
  font-size: 0.78rem;
}
.data-table th {
  text-align: left;
  padding: 8px 10px;
  font-size: 0.7rem;
  font-weight: 600;
  color: var(--color-faint);
  text-transform: uppercase;
  letter-spacing: 0.04em;
  border-bottom: 1px solid var(--color-border);
  background: var(--color-card-2);
  white-space: nowrap;
}
.data-table td {
  padding: 8px 10px;
  border-bottom: 1px solid var(--color-border);
  color: var(--color-dim);
  vertical-align: top;
}
.data-table tbody tr:last-child td {
  border-bottom: none;
}
.row-click {
  cursor: pointer;
  transition: background-color var(--duration-fast);
}
.row-click:hover {
  background: var(--color-inset);
}
.row-selected {
  background: var(--color-primary-soft);
}
.cell-strong {
  color: var(--color-text);
  font-weight: 500;
}
.cell-dim {
  color: var(--color-faint);
}
.cell-null {
  color: var(--color-faint);
  font-style: italic;
}
.grid-scroll {
  overflow-x: auto;
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
}
.data-table--grid td,
.data-table--grid th {
  max-width: 360px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.th-sortable {
  cursor: pointer;
  user-select: none;
}
.th-sortable:hover {
  color: var(--color-dim);
}
.sort-mark {
  font-size: 0.62rem;
}
.cell-op-col {
  width: 56px;
  white-space: nowrap;
}

.table-detail {
  display: flex;
  flex-direction: column;
  gap: 14px;
}
.detail-head {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
}
.btn-link {
  border: none;
  background: none;
  color: var(--color-primary);
  font-size: 0.8rem;
  font-weight: 500;
  cursor: pointer;
  padding: 0;
}
.btn-link:hover {
  text-decoration: underline;
}
.detail-name {
  font-size: 0.9rem;
  color: var(--color-text);
}
.subtabs {
  display: inline-flex;
  gap: 2px;
  background: var(--color-inset);
  border-radius: var(--rounded);
  padding: 3px;
}
.subtab {
  border: none;
  background: transparent;
  padding: 5px 12px;
  font-size: 0.76rem;
  color: var(--color-dim);
  border-radius: var(--rounded-md);
  cursor: pointer;
}
.subtab--active {
  background: var(--color-card);
  color: var(--color-text);
  font-weight: 600;
  box-shadow: var(--shadow);
}
.detail-actions {
  display: flex;
  align-items: center;
  gap: 6px;
  margin-left: auto;
  flex-wrap: wrap;
}

.filter-bar {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.filter-input {
  flex: 1;
  min-width: 240px;
  height: 32px;
  padding: 0 10px;
  background: var(--color-inset);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  color: var(--color-text);
  font-size: 0.78rem;
}
.filter-input:focus {
  outline: none;
  border-color: var(--color-primary);
}
.page-size {
  height: 32px;
  padding: 0 8px;
  background: var(--color-inset);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  color: var(--color-text);
  font-size: 0.78rem;
}
.row-count {
  font-size: 0.74rem;
  color: var(--color-faint);
}

.pager {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 10px;
}
.pager-info {
  font-size: 0.78rem;
  color: var(--color-dim);
}

.sql-input {
  width: 100%;
  box-sizing: border-box;
  padding: 12px;
  background: var(--color-inset);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  color: var(--color-text);
  font-size: 0.8rem;
  line-height: 1.6;
  resize: vertical;
}
.sql-input:focus {
  outline: none;
  border-color: var(--color-primary);
}
.sql-actions {
  display: flex;
  align-items: center;
  gap: 10px;
}
.sql-error {
  padding: 10px 12px;
  background: var(--color-red-soft);
  border: 1px solid var(--color-red-line);
  color: var(--color-red);
  border-radius: var(--rounded);
  font-size: 0.78rem;
  white-space: pre-wrap;
  word-break: break-all;
}
.affected-line {
  font-size: 0.8rem;
  color: var(--color-dim);
}
.hint-line {
  font-size: 0.74rem;
  color: var(--color-faint);
  line-height: 1.5;
}
.hint-inline {
  font-size: 0.74rem;
  color: var(--color-faint);
}
.hint-inline.warn {
  color: var(--color-amber);
}

.users-layout {
  display: grid;
  grid-template-columns: minmax(280px, 1fr) minmax(300px, 1.2fr);
  gap: 16px;
  align-items: start;
}
.grants-panel {
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  padding: 14px;
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.grants-head {
  display: flex;
  align-items: center;
  gap: 10px;
}
.grant-lines {
  list-style: none;
  margin: 0;
  padding: 10px 12px;
  background: var(--color-inset);
  border-radius: var(--rounded);
  font-size: 0.74rem;
  color: var(--color-dim);
  display: flex;
  flex-direction: column;
  gap: 6px;
  word-break: break-all;
}
.grant-actions {
  display: flex;
  align-items: center;
  gap: 8px;
}

.btn-primary {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  height: 34px;
  padding: 0 15px;
  border: none;
  background: var(--color-primary);
  color: #fff;
  font-family: var(--font-sans);
  font-size: 0.83rem;
  font-weight: 600;
  border-radius: var(--rounded);
  cursor: pointer;
  box-shadow: 0 5px 16px var(--color-primary-soft);
  transition: background-color var(--duration-fast);
  white-space: nowrap;
  text-decoration: none;
}
.btn-primary:hover:not(:disabled) {
  background: var(--color-primary-press);
}
.btn-primary:disabled {
  opacity: 0.45;
  cursor: not-allowed;
  box-shadow: none;
}
.btn-secondary {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  height: 34px;
  padding: 0 15px;
  background: var(--color-card-2);
  color: var(--color-text);
  border: 1px solid var(--color-border-strong);
  font-family: var(--font-sans);
  font-size: 0.83rem;
  font-weight: 500;
  border-radius: var(--rounded);
  cursor: pointer;
  transition: border-color var(--duration-fast);
  text-decoration: none;
}
.btn-secondary:hover:not(:disabled) {
  border-color: var(--color-faint);
}
.btn-secondary:disabled {
  opacity: 0.45;
  cursor: not-allowed;
}
.btn-sm {
  height: 28px;
  padding: 0 10px;
  font-size: 0.76rem;
}
.btn-danger-ghost:hover:not(:disabled) {
  color: var(--color-red);
  border-color: var(--color-red-line);
  background: var(--color-red-soft);
}
.btn-danger {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  height: 34px;
  padding: 0 15px;
  background: var(--color-red-soft);
  color: var(--color-red);
  border: 1px solid var(--color-red-line);
  font-family: var(--font-sans);
  font-size: 0.83rem;
  font-weight: 600;
  border-radius: var(--rounded);
  cursor: pointer;
}
.btn-danger:hover:not(:disabled) {
  background: oklch(62% 0.18 22 / 0.25);
}
.btn-danger:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}

.icon-btn {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 26px;
  height: 26px;
  border: 1px solid var(--color-border);
  border-radius: var(--rounded-md);
  background: transparent;
  color: var(--color-dim);
  cursor: pointer;
  transition: color var(--duration-fast), border-color var(--duration-fast);
}
.icon-btn:hover:not(:disabled) {
  color: var(--color-primary);
  border-color: var(--color-primary);
}
.icon-btn--danger:hover:not(:disabled) {
  color: var(--color-red);
  border-color: var(--color-red-line);
  background: var(--color-red-soft);
}

.modal-scrim {
  position: fixed;
  inset: 0;
  background: oklch(0% 0 0 / 0.62);
  display: grid;
  place-items: center;
  z-index: 100;
  padding: 24px;
}
.modal {
  width: 100%;
  max-width: 640px;
  max-height: min(90vh, calc(100dvh - 48px));
  display: flex;
  flex-direction: column;
  background: var(--color-card);
  border: 1px solid var(--color-border-strong);
  border-radius: var(--rounded-xl);
  box-shadow: var(--shadow-modal);
  overflow: hidden;
}
.modal--sm {
  max-width: 520px;
}
.modal-head {
  display: flex;
  align-items: flex-start;
  gap: 12px;
  padding: 20px 20px 16px;
  border-bottom: 1px solid var(--color-border);
}
.modal-icon {
  width: 36px;
  height: 36px;
  border-radius: var(--rounded-lg);
  background: var(--color-primary-soft);
  color: var(--color-primary);
  display: grid;
  place-items: center;
  flex-shrink: 0;
}
.modal-icon--danger {
  background: var(--color-red-soft);
  color: var(--color-red);
}
.modal-title {
  font-size: 1rem;
  font-weight: 600;
  color: var(--color-text);
  margin-top: 2px;
}
.modal-sub {
  font-size: 0.78rem;
  color: var(--color-faint);
  margin-top: 3px;
  line-height: 1.45;
}
.modal-sub--warn {
  color: var(--color-amber);
}
.modal-body {
  padding: 18px 20px;
  display: flex;
  flex-direction: column;
  gap: 10px;
  min-height: 0;
  overflow-y: auto;
}
.modal-form {
  padding: 18px 20px;
  display: flex;
  flex-direction: column;
  gap: 12px;
  min-height: 0;
  max-height: min(60vh, 520px);
  overflow-y: auto;
}
.modal-footer {
  display: flex;
  justify-content: flex-end;
  gap: 8px;
}
.modal-footer--standalone {
  padding: 4px 20px 20px;
}
.field {
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.field-label {
  font-size: 0.78rem;
  font-weight: 500;
  color: var(--color-dim);
}
.field-input {
  width: 100%;
  height: 36px;
  background: var(--color-inset);
  border: 1px solid var(--color-border);
  border-radius: var(--rounded);
  padding: 0 12px;
  color: var(--color-text);
  font-size: 0.84rem;
  box-sizing: border-box;
}
.field-input:focus {
  outline: none;
  border-color: var(--color-primary);
}

.row-field {
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.row-field-label {
  display: flex;
  align-items: baseline;
  gap: 8px;
}
.row-field-type {
  font-size: 0.68rem;
  color: var(--color-faint);
}
.row-field-input {
  display: flex;
  align-items: center;
  gap: 8px;
}
.row-field-input .field-input {
  flex: 1;
}
.check-inline {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font-size: 0.74rem;
  color: var(--color-dim);
  white-space: nowrap;
  cursor: pointer;
}
.check-inline input {
  accent-color: var(--color-primary);
}

.delete-text {
  font-size: 0.86rem;
  color: var(--color-dim);
  line-height: 1.55;
}

.spinner {
  display: inline-block;
  width: 13px;
  height: 13px;
  border: 2px solid rgba(255, 255, 255, 0.35);
  border-top-color: #fff;
  border-radius: var(--rounded-full);
  animation: spin 0.7s linear infinite;
}
@keyframes spin {
  to { transform: rotate(360deg); }
}

.block-title {
  font-size: 0.84rem;
  font-weight: 600;
  color: var(--color-text);
  margin: 6px 0 0;
}

@media (max-width: 900px) {
  .dba-layout {
    grid-template-columns: 1fr;
  }
  .users-layout {
    grid-template-columns: 1fr;
  }
  .db-list {
    max-height: 200px;
  }
}
</style>
