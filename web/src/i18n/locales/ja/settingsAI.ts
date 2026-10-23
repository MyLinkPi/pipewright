export default {
  protocolHelp:
    '選択するのは提供元ではなく API の互換形式です。DeepSeek、OpenRouter、自前のゲートウェイも対応する形式を選び、その接続先、API キー、モデル ID を入力できます。',
  compatibleProtocol: '互換プロトコル',
  selectProtocol: 'API プロトコルを選択',
  quickSetup: 'クイック設定',
  customGateway: 'カスタムゲートウェイ',
  modelId: 'モデル ID',
  availableModels: '利用可能なモデル',
  fetchModels: 'モデルを取得',
  modelsUnavailable: 'モデル一覧を取得できません。ID を手動入力できます。',
  title: 'AI プロバイダー',
  subtitle:
    'Pipewright は独自のモデルを学習しません —— 障害診断と設定生成には自前の LLM を接続します。キーはこのインスタンスの暗号化された保管庫にのみ保存され、外部には決して漏れません。',
  statusConfigured: '設定済み',
  statusUnconfigured: '未設定',
  retry: '再試行',

  providerClaudeTag: '診断におすすめ',

  guidanceAria: 'AI 設定ガイド',
  guidanceTitle: 'AI 診断を有効化するには LLM を設定してください',
  guidanceBody:
    'Claude、OpenAI、または任意の OpenAI 互換エンドポイント(例: Ollama /v1)を接続すると、パイプライン障害時に Pipewright が根本原因の仮説と修正案を自動生成し、手動でのログ調査が不要になります。',

  selectProvider: 'プロバイダーを選択',
  providerRadioAria: 'AI プロバイダーの選択',
  selectProviderAria: '{name} を選択',
  providerConfig: '{name} の設定',
  lastSaved: '前回の保存 {time}',

  apiKeyHint:
    '入力後はマスク表示のみ。空欄のままにすると既存のキーを保持します',
  apiKeyReplacing: '置き換え中…',
  apiKeyConfigured: '設定済み ••••（空欄で変更なし）',
  apiKeyPaste: 'API Key を貼り付け…',
  apiKeyMaskedAria: '設定済みマスク: {masked}',


  baseUrlLabel: '接続先 (Base URL)',
  baseUrlHint: 'デフォルト: {url}(アドレスはそのまま連結されます。/v1 などのバージョン接頭辞を保持してください)',

  modelLabel: 'モデル',
  modelHint:
    '診断に使用するメインモデル。例: claude-opus-4-7 / gpt-4o / llama3',

  testConnection: '接続テスト',
  testOk: '推論正常 · レイテンシ {ms}ms',
  testFail: '接続に失敗しました',

  budgetLabel: '月間 Token 上限',
  budgetHint:
    '超過すると AI 診断を一時停止します（空欄=無制限。今回は宣言のみ、次の Epic で強制適用）',
  budgetPlaceholder: '例: 500000、空欄で無制限',

  ctxTitle: 'コンテキスト管理',
  ctxWindowHint:
    'Ops エージェントのコンテキスト予算:ウィンドウ上限に近づくと古い会話を自動で要約に圧縮し、オーバーフロー時は圧縮して再試行します。単位はトークン、空欄で既定値。',
  ctxWindowLabel: 'コンテキストウィンドウ',
  ctxWindowPlaceholder: '既定 200000',
  ctxMaxOutputLabel: '1 回あたりの出力上限',
  ctxMaxOutputPlaceholder: '既定 8192',
  ctxKeepLabel: '圧縮時に保持する直近',
  ctxKeepPlaceholder: '既定 15000',
  ctxAutoCompact: '上限付近で履歴を自動圧縮',
  ctxAdvanced: '詳細設定',
  ctxBufferLabel: '発動リザーブ',
  ctxBufferPlaceholder: '自動(ウィンドウの 10%)',
  ctxHistoryMessagesLabel: '履歴メッセージ上限',
  ctxHistoryMessagesPlaceholder: '既定 100',
  ctxHistoryBytesLabel: '履歴サイズ上限 (KB)',
  ctxHistoryBytesPlaceholder: '既定 256',
  ctxNote:
    '圧縮はモデルへ送るコンテキストにのみ影響し、セッション画面は完全な履歴を保持します。要約は現在設定中のモデルが生成します。',
  errContextInvalid: 'コンテキスト値は空欄または 0 以上の整数にしてください',

  enableAi: 'AI 機能を有効化',
  enableAiDesc:
    'オフの場合 AI 診断は静かにスキップされ、コアの CI/CD パイプラインには影響しません',

  dirtyNote: '未保存の変更があります',
  cleanNote: '変更なし',
  discard: '破棄',
  saveChanges: '変更を保存',

  toastSaveSuccess: 'AI 設定を保存しました',
  toastSaveFailed: '保存に失敗しました',

  errServerUnreachable:
    'サーバーに接続できません。バックエンドが稼働しているか確認して再試行してください。',
  errServerUnreachableShort: 'サーバーに接続できません',
  errVaultUnconfigured:
    '保管庫に master key が設定されていません。PIPEWRIGHT_MASTER_KEY 環境変数を設定してください。',
  errLoadFailed: '読み込みに失敗しました（{status}）',
  errLoadGeneric:
    'AI 設定の読み込みに失敗しました。しばらくしてから再試行してください。',
  errBudgetInvalid: '月間 token 上限は正の整数か空欄である必要があります',
  errProviderInvalid: '有効なプロバイダーを選択してください',
  errBaseUrlRequired: '接続先を入力してください',
  errApiKeyRequired: 'API Key は空にできません(ローカルエンドポイントは任意の非空値で可)',
  errRequestFailed: 'リクエストに失敗しました（{status}）',
  errUnknown: '不明なエラー',
  webTitle: 'ウェブ検索・取得',
  webEnable: 'ウェブ検索/取得ツールを有効化',
  webEnableDesc: '運用エージェントに web_search / web_fetch を提供(呼び出しごとに課金、既定は無効)',
  webProviderLabel: 'プロバイダー',
  webProviderHint: 'プロバイダー種別が API 方言と公式既定アドレスを決定します。現在 DeepSeek / GLM / Tavily / 博查 / Ollama に対応;今後のプロバイダーはここに追加します',
  webBaseUrlLabel: '接続先 (Base URL)',
  webBaseUrlHint: '空欄で LLM の接続先を再用;入力すると上書き(公式既定: {url})',
  webBaseUrlDefaultHint: '空欄で公式既定アドレスを使用: {url}',
  webApiKeyHint: '空欄かつ接続先が一致すれば LLM のキーを再用;入力値はウェブツール専用で、以降はマスク表示',
  webTest: '検索をテスト',
  webAvailable: 'ウェブツール利用可能(宛先とキー解決済み)',
  webUnavailable: '現在の設定では利用できません',
  webNote: 'web_search は公開ウェブを検索、web_fetch はページを Markdown で取得(博查は非対応)。エージェントの呼び出しは選択したプロバイダー課金(失敗は通常無料)。',
}
