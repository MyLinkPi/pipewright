export default {
  protocolHelp:
    '选择的是接口兼容协议，不限于官网。DeepSeek、OpenRouter 或自建网关只要兼容其中一种协议，就能填写对应的接入地址、API Key 和模型 ID。',
  compatibleProtocol: '兼容协议',
  selectProtocol: '选择 API 协议',
  quickSetup: '快捷配置',
  customGateway: '自定义网关',
  modelId: '模型 ID',
  availableModels: '可用模型',
  fetchModels: '获取模型',
  modelsUnavailable: '模型列表不可用，可手动输入模型 ID。',
  title: 'AI 提供商',
  subtitle:
    'Pipewright 不自训模型 —— 接入你自己的 LLM 用于失败诊断与配置生成。密钥仅存于本实例的加密保险库,绝不外泄。',
  statusConfigured: '已配置',
  statusUnconfigured: '未配置',
  retry: '重试',

  providerClaudeTag: '诊断推荐',

  guidanceAria: 'AI 配置引导',
  guidanceTitle: '配置 LLM 以解锁 AI 诊断',
  guidanceBody:
    '接入 Claude、OpenAI 或任意 OpenAI 兼容端点(如本地 Ollama 的 /v1)后,流水线失败时 Pipewright 将自动生成根因假说与修复建议,无需手动排查日志。',

  selectProvider: '选择提供商',
  providerRadioAria: 'AI 提供商选择',
  selectProviderAria: '选择 {name}',
  providerConfig: '{name} 配置',
  lastSaved: '上次保存 {time}',

  apiKeyHint: '写入后仅显示掩码;留空则保留已存密钥',
  apiKeyReplacing: '正在替换…',
  apiKeyConfigured: '已配置 ••••(留空不变)',
  apiKeyPaste: '粘贴 API Key…',
  apiKeyMaskedAria: '已配置掩码: {masked}',


  baseUrlLabel: '接入地址 (Base URL)',
  baseUrlHint: '默认: {url}(地址按原样拼接接口路径,请保留 /v1 等版本前缀)',

  modelLabel: '模型',
  modelHint: '用于诊断的主模型,如 claude-opus-4-7 / gpt-4o / llama3',

  testConnection: '测试连接',
  testOk: '推理正常 · 延迟 {ms}ms',
  testFail: '连接失败',

  budgetLabel: '月 Token 上限',
  budgetHint: '超出后暂停 AI 诊断(留空=不限制;本期仅声明,下一 Epic 强制执行)',
  budgetPlaceholder: '如 500000,留空不限制',

  ctxTitle: '上下文管理',
  ctxWindowHint:
    '运维对话 agent 的上下文预算:接近窗口上限时自动把旧对话压缩为摘要,溢出时自动压缩重试。单位均为 tokens,留空用默认值。',
  ctxWindowLabel: '上下文窗口',
  ctxWindowPlaceholder: '默认 200000',
  ctxMaxOutputLabel: '单次输出上限',
  ctxMaxOutputPlaceholder: '默认 8192',
  ctxKeepLabel: '压缩保留最近',
  ctxKeepPlaceholder: '默认 15000',
  ctxAutoCompact: '接近上限时自动压缩历史',
  ctxAdvanced: '高级设置',
  ctxBufferLabel: '触发预留',
  ctxBufferPlaceholder: '自动(窗口 10%)',
  ctxHistoryMessagesLabel: '历史条数上限',
  ctxHistoryMessagesPlaceholder: '默认 100',
  ctxHistoryBytesLabel: '历史字节上限 (KB)',
  ctxHistoryBytesPlaceholder: '默认 256',
  ctxNote:
    '压缩只影响发给模型的上下文,会话界面仍保留完整历史;压缩摘要由当前配置的模型生成。',
  errContextInvalid: '上下文配置须为非负整数或留空',

  enableAi: '启用 AI 功能',
  enableAiDesc: '关闭时 AI 诊断静默跳过,核心 CI/CD 流水线不受影响',

  dirtyNote: '有未保存的改动',
  cleanNote: '暂无改动',
  discard: '放弃',
  saveChanges: '保存更改',

  toastSaveSuccess: 'AI 配置已保存',
  toastSaveFailed: '保存失败',

  errServerUnreachable: '无法连接到服务器,请检查后端是否运行后重试',
  errServerUnreachableShort: '无法连接到服务器',
  errVaultUnconfigured:
    '保险库未配置 master key,请设置 PIPEWRIGHT_MASTER_KEY 环境变量',
  errLoadFailed: '加载失败({status})',
  errLoadGeneric: '加载 AI 配置失败,请稍后重试',
  errBudgetInvalid: '月 token 上限须为正整数或留空',
  errProviderInvalid: '请选择有效的提供商',
  errBaseUrlRequired: '请填写接入地址',
  errApiKeyRequired: 'API Key 不可为空(本地端点填任意非空值即可)',
  errRequestFailed: '请求失败({status})',
  errUnknown: '未知错误',
}
