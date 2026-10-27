export default {
  // ─── 顶栏 / 面包屑 ───────────────────────────────────────────────
  breadcrumbAria: '面包屑导航',
  breadcrumbProjects: '项目',
  title: '流水线配置',

  // ─── 标签页 ─────────────────────────────────────────────────────
  tabCanvas: '流水线编排',
  tabBuild: '构建配置',
  tabVars: '变量与缓存',
  tabTriggers: '触发设置',
  tabStripAria: '流水线配置标签页',

  // ─── 流水线即代码(GitOps · FR-8-12)──────────────────────────────
  pacTitle: '流水线即代码',
  pacOnHint: '运行将按运行分支读取仓库 .pipewright.yml 驱动(文件缺失或非法时回退到此处配置)。',
  pacOffHint: '开启后,每次运行按运行分支读取仓库根 .pipewright.yml 驱动(缺失或非法则回退此处配置)。',
  pacToggleFailed: '切换失败,请重试。',

  // ─── PR 状态检查(提交状态回写 · Story 8-9 / FR-8-9)────────────────
  prStatusTitle: 'PR 状态检查',
  prStatusOnHint: '运行结束时据项目仓库识别 GitHub/Gitee,经项目凭据把该 commit 的成功/失败状态回写为 PR 检查(尽力而为,失败不影响运行)。',
  prStatusOffHint: '开启后,运行结束时据项目仓库识别 GitHub/Gitee,经项目凭据将该 commit 的状态回写为 PR 检查(尽力而为)。',
  prStatusToggleFailed: '切换失败,请重试。',

  // ─── 预览仓库配置(GitOps · 按 ref 拉取并校验 .pipewright.yml)────────
  pacPreviewBtn: '预览仓库配置',
  pacPreviewTitle: '预览仓库配置',
  pacPreviewSub: '按分支/标签/提交拉取并校验仓库中的',
  pacPreviewCloseAria: '关闭预览',
  pacPreviewCloseBtn: '关闭',
  pacPreviewRefLabel: '分支 / 标签 / 提交',
  pacPreviewFetch: '拉取并校验',
  pacPreviewNotFound: '在 {ref} 未找到 {file},运行时将回退到此处库内配置。',
  pacPreviewInvalid: '{ref} 的 {file} 校验失败,运行时将静默回退到此处库内配置:',
  pacPreviewValid: '{ref} 的 {file} 校验通过,共 {count} 个阶段;运行时将用它驱动。',
  pacPreviewJobCount: '{count} 个任务',
  pacPreviewConnFailed: '连接失败,请检查网络后重试。',
  pacPreviewFailed: '预览失败({status}),请重试。',
  pacPreviewFailedRetry: '预览失败,请重试。',
  pacPreviewFileLabel: '流水线文件',
  pacPreviewSetDefault: '设为项目默认文件',
  pacPreviewSetDefaultDone: '已设为项目默认文件',
  pacPreviewSetDefaultFailed: '设置默认文件失败,请重试。',
  pacPreviewSetDefaultSame: '与项目当前默认一致,无需保存',
  pacPreviewIdle: '确认分支与流水线文件后,点击「拉取并校验」。',
  pacPreviewVarsTitle: '声明的变量({count})',
  pacPreviewVarsHint: '在「变量与缓存」中填写这些变量的值;secret 变量从保险库凭据引用,绝不在 YAML 中出现。',
  pacVarRequired: '必填',
  pacVarSecret: 'secret',

  // ─── 工具栏按钮 ─────────────────────────────────────────────────
  aiGenerate: 'AI 生成流水线',
  importYaml: '从 YAML 导入',
  templates: '模板',
  validate: '校验配置',
  closeValidationPanel: '关闭校验面板',
  badgeReady: '就绪',
  badgeErrors: '{n} 错误',
  saving: '保存中…',
  saveDraft: '保存',

  // ─── 横幅 / 状态 ────────────────────────────────────────────────
  dismiss: '关闭提示',
  draftSaved: '流水线已保存',
  scopeSettings: '变量',
  scopePipeline: '流水线编排',
  noChanges: '没有未保存的修改',
  errSavePartial: '部分已保存({saved});失败:',
  replaceDirtyTitle: '画布有未保存修改',
  replaceDirtyBody: '该操作会整体替换画布,未保存的修改将丢失。确定继续?',
  replaceDirtyConfirm: '丢弃并继续',
  tabDirtyAria: '有未保存修改',
  retry: '重试',
  loading: '加载中',

  // ─── 加载错误 ───────────────────────────────────────────────────
  errNoServer: '无法连接到服务器,请检查后端是否运行后重试。',
  errProjectNotFound: '项目不存在,请确认项目 ID 正确。',
  errLoadFailedStatus: '加载流水线失败({status})',
  errLoadFailedRetry: '加载流水线失败,请稍后重试。',

  // ─── 保存错误 ───────────────────────────────────────────────────
  errSaveFailedRetry: '保存失败,请稍后重试。',
  errSaveFailedStatus: '保存失败({status})',
  errSettingsNotLoaded: '设置尚未加载成功,本次未保存任何内容。请刷新页面后重试。',
  errInvalidStage: '阶段名不能为空或 kind 不在允许值内,请检查后重试。',
  errInvalidJob: '任务名称或类型不能为空,请补充后重试。',
  errDuplicateId: '阶段或任务 ID 重复,请删除重复项后重试。',
  errInvalidBuild: '构建模型须为 dockerfile/toolchain,产物类型须为 image/jar/dist。',
  errInvalidVar: '变量键不能为空且同作用域内不可重复;secret 变量须选择保险库凭据。',
  errCredentialNotFound: '引用的保险库凭据不存在,请重新选择后重试。',
  errVaultUnconfigured: '保险库未配置 master key,无法引用 secret 凭据。',
}
