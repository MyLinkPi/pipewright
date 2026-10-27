export default {
  // ─── 頂欄 / 麵包屑 ───────────────────────────────────────────────
  breadcrumbAria: '麵包屑導航',
  breadcrumbProjects: '專案',
  title: '流水線設定',

  // ─── 標籤頁 ─────────────────────────────────────────────────────
  tabCanvas: '流水線編排',
  tabBuild: '構建配置',
  tabVars: '變數與快取',
  tabTriggers: '觸發設定',
  tabStripAria: '流水線設定標籤頁',

  // ─── 流水線即程式碼(GitOps · FR-8-12)─────────────────────────────
  pacTitle: '流水線即程式碼',
  pacOnHint: '執行時將依執行分支讀取倉庫 .pipewright.yml 驅動(檔案缺失或非法時回退至此處設定)。',
  pacOffHint: '開啟後,每次執行依執行分支讀取倉庫根 .pipewright.yml 驅動(缺失或非法則回退此處設定)。',
  pacToggleFailed: '切換失敗,請重試。',

  // ─── PR 狀態檢查(提交狀態回寫 · Story 8-9 / FR-8-9)────────────────
  prStatusTitle: 'PR 狀態檢查',
  prStatusOnHint: '執行結束時依專案倉庫識別 GitHub/Gitee,經專案憑證將該 commit 的成功/失敗狀態回寫為 PR 檢查(盡力而為,失敗不影響執行)。',
  prStatusOffHint: '開啟後,執行結束時依專案倉庫識別 GitHub/Gitee,經專案憑證將該 commit 的狀態回寫為 PR 檢查(盡力而為)。',
  prStatusToggleFailed: '切換失敗,請重試。',

  // ─── 預覽倉庫設定(GitOps · 按 ref 拉取並校驗 .pipewright.yml)────────
  pacPreviewBtn: '預覽倉庫設定',
  pacPreviewTitle: '預覽倉庫設定',
  pacPreviewSub: '按分支/標籤/提交拉取並校驗倉庫中的',
  pacPreviewCloseAria: '關閉預覽',
  pacPreviewCloseBtn: '關閉',
  pacPreviewRefLabel: '分支 / 標籤 / 提交',
  pacPreviewFetch: '拉取並校驗',
  pacPreviewNotFound: '在 {ref} 未找到 {file},執行時將回退到此處庫內設定。',
  pacPreviewInvalid: '{ref} 的 {file} 校驗失敗,執行時將靜默回退到此處庫內設定:',
  pacPreviewValid: '{ref} 的 {file} 校驗通過,共 {count} 個階段;執行時將用它驅動。',
  pacPreviewJobCount: '{count} 個任務',
  pacPreviewConnFailed: '連線失敗,請檢查網絡後重試。',
  pacPreviewFailed: '預覽失敗({status}),請重試。',
  pacPreviewFailedRetry: '預覽失敗,請重試。',
  pacPreviewFileLabel: '流水線文件',
  pacPreviewSetDefault: '設為項目默認文件',
  pacPreviewSetDefaultDone: '已設為項目默認文件',
  pacPreviewSetDefaultFailed: '設置默認文件失敗,請重試。',
  pacPreviewSetDefaultSame: '與項目當前默認一致,無需保存',
  pacPreviewIdle: '確認分支與流水線文件後,點擊「拉取並校驗」。',
  pacPreviewVarsTitle: '聲明的變量({count})',
  pacPreviewVarsHint: '在「變量與緩存」中填寫這些變量的值;secret 變量從保險庫憑據引用,絕不在 YAML 中出現。',
  pacVarRequired: '必填',
  pacVarSecret: 'secret',

  // ─── 工具列按鈕 ─────────────────────────────────────────────────
  aiGenerate: 'AI 生成流水線',
  importYaml: '從 YAML 匯入',
  templates: '範本',
  validate: '驗證設定',
  closeValidationPanel: '關閉驗證面板',
  badgeReady: '就緒',
  badgeErrors: '{n} 個錯誤',
  saving: '儲存中…',
  saveDraft: '儲存',

  // ─── 橫幅 / 狀態 ────────────────────────────────────────────────
  dismiss: '關閉提示',
  draftSaved: '流水線已儲存',
  scopeSettings: '變數',
  scopePipeline: '流水線編排',
  noChanges: '沒有未儲存的修改',
  errSavePartial: '部分已儲存({saved});失敗:',
  replaceDirtyTitle: '畫布有未儲存修改',
  replaceDirtyBody: '該操作會整體替換畫布,未儲存的修改將遺失。確定繼續?',
  replaceDirtyConfirm: '捨棄並繼續',
  tabDirtyAria: '有未儲存修改',
  retry: '重試',
  loading: '載入中',

  // ─── 載入錯誤 ───────────────────────────────────────────────────
  errNoServer: '無法連線到伺服器,請檢查後端是否執行後重試。',
  errProjectNotFound: '專案不存在,請確認專案 ID 正確。',
  errLoadFailedStatus: '載入流水線失敗({status})',
  errLoadFailedRetry: '載入流水線失敗,請稍後重試。',

  // ─── 儲存錯誤 ───────────────────────────────────────────────────
  errSaveFailedRetry: '儲存失敗,請稍後重試。',
  errSettingsNotLoaded: '設定尚未載入成功,本次未儲存任何內容。請重新整理頁面後重試。',
  errSaveFailedStatus: '儲存失敗({status})',
  errInvalidStage: '階段名稱不能為空或 kind 不在允許值內,請檢查後重試。',
  errInvalidJob: '任務名稱或類型不能為空,請補充後重試。',
  errDuplicateId: '階段或任務 ID 重複,請刪除重複項後重試。',
  errInvalidBuild: '構建模型須為 dockerfile/toolchain,產物類型須為 image/jar/dist。',
  errInvalidVar: '變數鍵不能為空且同作用域內不可重複;secret 變數須選擇保險庫憑證。',
  errCredentialNotFound: '引用的保險庫憑證不存在,請重新選擇後重試。',
  errVaultUnconfigured: '保險庫未設定 master key,無法引用 secret 憑證。',
}
