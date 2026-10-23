export default {
  protocolHelp:
    '選擇的是介面相容協議，不限於官網。DeepSeek、OpenRouter 或自建閘道只要相容其中一種協議，就能填寫對應的接入位址、API Key 和模型 ID。',
  compatibleProtocol: '相容協定',
  selectProtocol: '選擇 API 協定',
  quickSetup: '快速設定',
  customGateway: '自訂閘道',
  modelId: '模型 ID',
  availableModels: '可用模型',
  fetchModels: '取得模型',
  modelsUnavailable: '模型清單無法取得，可手動輸入模型 ID。',
  title: 'AI 服務商',
  subtitle:
    'Pipewright 不自訓模型 —— 接入你自己的 LLM 用於失敗診斷與設定生成。金鑰僅存於本實例的加密保險庫,絕不外洩。',
  statusConfigured: '已設定',
  statusUnconfigured: '未設定',
  retry: '重試',

  providerClaudeTag: '診斷推薦',

  guidanceAria: 'AI 設定引導',
  guidanceTitle: '設定 LLM 以解鎖 AI 診斷',
  guidanceBody:
    '接入 Claude、OpenAI 或任意 OpenAI 相容端點(如本機 Ollama 的 /v1)後,流水線失敗時 Pipewright 將自動產生根因假說與修復建議,無需手動排查日誌。',

  selectProvider: '選擇服務商',
  providerRadioAria: 'AI 服務商選擇',
  selectProviderAria: '選擇 {name}',
  providerConfig: '{name} 設定',
  lastSaved: '上次儲存 {time}',

  apiKeyHint: '寫入後僅顯示遮罩;留空則保留已存金鑰',
  apiKeyReplacing: '正在替換…',
  apiKeyConfigured: '已設定 ••••(留空不變)',
  apiKeyPaste: '貼上 API Key…',
  apiKeyMaskedAria: '已設定遮罩: {masked}',


  baseUrlLabel: '接入位址 (Base URL)',
  baseUrlHint: '預設: {url}(地址按原樣拼接介面路徑,請保留 /v1 等版本前綴)',

  modelLabel: '模型',
  modelHint: '用於診斷的主模型,如 claude-opus-4-7 / gpt-4o / llama3',

  testConnection: '測試連線',
  testOk: '推理正常 · 延遲 {ms}ms',
  testFail: '連線失敗',

  budgetLabel: '每月 Token 上限',
  budgetHint: '超出後暫停 AI 診斷(留空=不限制;本期僅宣告,下一 Epic 強制執行)',
  budgetPlaceholder: '如 500000,留空不限制',

  ctxTitle: '上下文管理',
  ctxWindowHint:
    '運維對話 agent 的上下文預算:接近視窗上限時自動把舊對話壓縮為摘要,溢出時自動壓縮重試。單位均為 tokens,留空用預設值。',
  ctxWindowLabel: '上下文視窗',
  ctxWindowPlaceholder: '預設 200000',
  ctxMaxOutputLabel: '單次輸出上限',
  ctxMaxOutputPlaceholder: '預設 8192',
  ctxKeepLabel: '壓縮保留最近',
  ctxKeepPlaceholder: '預設 15000',
  ctxAutoCompact: '接近上限時自動壓縮歷史',
  ctxAdvanced: '進階設定',
  ctxBufferLabel: '觸發預留',
  ctxBufferPlaceholder: '自動(視窗 10%)',
  ctxHistoryMessagesLabel: '歷史條數上限',
  ctxHistoryMessagesPlaceholder: '預設 100',
  ctxHistoryBytesLabel: '歷史位元組上限 (KB)',
  ctxHistoryBytesPlaceholder: '預設 256',
  ctxNote:
    '壓縮只影響發給模型的上下文,會話界面仍保留完整歷史;壓縮摘要由目前配置的模型生成。',
  errContextInvalid: '上下文配置須為非負整數或留空',

  enableAi: '啟用 AI 功能',
  enableAiDesc: '關閉時 AI 診斷靜默略過,核心 CI/CD 流水線不受影響',

  dirtyNote: '有未儲存的變更',
  cleanNote: '暫無變更',
  discard: '捨棄',
  saveChanges: '儲存變更',

  toastSaveSuccess: 'AI 設定已儲存',
  toastSaveFailed: '儲存失敗',

  errServerUnreachable: '無法連線到伺服器,請檢查後端是否執行後重試',
  errServerUnreachableShort: '無法連線到伺服器',
  errVaultUnconfigured:
    '保險庫未設定 master key,請設定 PIPEWRIGHT_MASTER_KEY 環境變數',
  errLoadFailed: '載入失敗({status})',
  errLoadGeneric: '載入 AI 設定失敗,請稍後重試',
  errBudgetInvalid: '每月 token 上限須為正整數或留空',
  errProviderInvalid: '請選擇有效的服務商',
  errBaseUrlRequired: '請填寫接入位址',
  errApiKeyRequired: 'API Key 不可為空(本地位點填任意非空值即可)',
  errRequestFailed: '請求失敗({status})',
  errUnknown: '未知錯誤',
  webTitle: '網頁搜尋與擷取',
  webEnable: '啟用網頁搜尋/擷取工具',
  webEnableDesc: '為運維智能體提供 web_search / web_fetch 工具(按次計費,預設關閉)',
  webFollowLLM: '跟隨 LLM 配置(預設)',
  webProviderLabel: '供應商',
  webProviderHint: '預設沿用 LLM 的接入地址與密鑰(僅當 LLM 為 DeepSeek 官方地址時可用);明確選擇供應商後可另行配置',
  webBaseUrlLabel: '接入地址 (Base URL)',
  webBaseUrlHint: '留空用官方預設: {url}',
  webApiKeyHint: '與 LLM 接入地址一致時留空即沿用其密鑰;寫入後僅顯示掩碼',
  webTest: '測試搜尋',
  webAvailable: '網頁工具可用(已解析到有效目的地與密鑰)',
  webUnavailable: '目前配置不可用',
  webNote: 'web_search 查公開網絡資料、web_fetch 讀網頁正文(Markdown);智能體調用按供應商計費(DeepSeek 約 ¥5/千次,失敗不計費)。',
}
