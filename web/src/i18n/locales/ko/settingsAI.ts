export default {
  protocolHelp:
    '공급업체가 아닌 API 호환 형식을 선택합니다. DeepSeek, OpenRouter, 자체 게이트웨이도 호환 프로토콜을 선택하고 해당 주소, API 키, 모델 ID를 입력하면 사용할 수 있습니다.',
  compatibleProtocol: '호환 프로토콜',
  selectProtocol: 'API 프로토콜 선택',
  quickSetup: '빠른 설정',
  customGateway: '사용자 지정 게이트웨이',
  modelId: '모델 ID',
  availableModels: '사용 가능한 모델',
  fetchModels: '모델 가져오기',
  modelsUnavailable:
    '모델 목록을 사용할 수 없습니다. ID를 직접 입력할 수 있습니다.',
  title: 'AI 제공자',
  subtitle:
    'Pipewright는 자체 모델을 학습하지 않습니다 —— 실패 진단과 설정 생성을 위해 직접 보유한 LLM을 연결하세요. 키는 이 인스턴스의 암호화된 보관소에만 저장되며 절대 외부로 유출되지 않습니다.',
  statusConfigured: '구성됨',
  statusUnconfigured: '미구성',
  retry: '다시 시도',

  providerClaudeTag: '진단 추천',

  guidanceAria: 'AI 구성 가이드',
  guidanceTitle: 'AI 진단을 활성화하려면 LLM을 구성하세요',
  guidanceBody:
    'Claude, OpenAI 또는 임의의 OpenAI 호환 엔드포인트(예: Ollama /v1)를 연결하면 파이프라인 실패 시 Pipewright가 근본 원인 가설과 수정 제안을 자동으로 생성하여 수동 로그 분석이 필요 없습니다.',

  selectProvider: '제공자 선택',
  providerRadioAria: 'AI 제공자 선택',
  selectProviderAria: '{name} 선택',
  providerConfig: '{name} 구성',
  lastSaved: '마지막 저장 {time}',

  apiKeyHint:
    '입력 후에는 마스킹된 값만 표시됩니다. 비워 두면 기존 키를 유지합니다',
  apiKeyReplacing: '교체 중…',
  apiKeyConfigured: '구성됨 ••••(비워 두면 변경 없음)',
  apiKeyPaste: 'API Key 붙여넣기…',
  apiKeyMaskedAria: '구성된 마스크: {masked}',


  baseUrlLabel: '접속 주소 (Base URL)',
  baseUrlHint: '기본값: {url} (주소는 그대로 연결됩니다 — /v1 등 버전 접두사를 유지하세요)',

  modelLabel: '모델',
  modelHint: '진단에 사용하는 기본 모델, 예: claude-opus-4-7 / gpt-4o / llama3',

  testConnection: '연결 테스트',
  testOk: '추론 정상 · 지연 {ms}ms',
  testFail: '연결 실패',

  budgetLabel: '월 Token 한도',
  budgetHint:
    '초과 시 AI 진단을 일시 중지합니다(비워 두면 무제한. 이번 주기에는 선언만, 다음 Epic에서 강제 적용)',
  budgetPlaceholder: '예: 500000, 비워 두면 무제한',

  ctxTitle: '컨텍스트 관리',
  ctxWindowHint:
    'Ops 에이전트의 컨텍스트 예산: 창 한계에 가까워지면 오래된 대화를 자동으로 요약 압축하고, 초과 시 압축 후 재시도합니다. 단위는 토큰, 비워 두면 기본값.',
  ctxWindowLabel: '컨텍스트 창',
  ctxWindowPlaceholder: '기본 200000',
  ctxMaxOutputLabel: '호출당 최대 출력',
  ctxMaxOutputPlaceholder: '기본 8192',
  ctxKeepLabel: '압축 시 최근 보존',
  ctxKeepPlaceholder: '기본 15000',
  ctxAutoCompact: '한계 근처에서 기록 자동 압축',
  ctxAdvanced: '고급 설정',
  ctxBufferLabel: '트리거 여유분',
  ctxBufferPlaceholder: '자동(창의 10%)',
  ctxHistoryMessagesLabel: '기록 메시지 상한',
  ctxHistoryMessagesPlaceholder: '기본 100',
  ctxHistoryBytesLabel: '기록 크기 상한 (KB)',
  ctxHistoryBytesPlaceholder: '기본 256',
  ctxNote:
    '압축은 모델로 보내는 컨텍스트에만 영향을 주며 세션 화면은 전체 기록을 유지합니다. 요약은 현재 구성된 모델이 생성합니다.',
  errContextInvalid: '컨텍스트 값은 빈 값 또는 음이 아닌 정수여야 합니다',

  enableAi: 'AI 기능 활성화',
  enableAiDesc:
    '끄면 AI 진단이 조용히 건너뛰며 핵심 CI/CD 파이프라인에는 영향이 없습니다',

  dirtyNote: '저장하지 않은 변경 사항이 있습니다',
  cleanNote: '변경 사항 없음',
  discard: '취소',
  saveChanges: '변경 사항 저장',

  toastSaveSuccess: 'AI 설정이 저장되었습니다',
  toastSaveFailed: '저장 실패',

  errServerUnreachable:
    '서버에 연결할 수 없습니다. 백엔드가 실행 중인지 확인한 후 다시 시도하세요.',
  errServerUnreachableShort: '서버에 연결할 수 없습니다',
  errVaultUnconfigured:
    '보관소에 master key가 구성되지 않았습니다. PIPEWRIGHT_MASTER_KEY 환경 변수를 설정하세요.',
  errLoadFailed: '불러오기 실패({status})',
  errLoadGeneric: 'AI 설정을 불러오지 못했습니다. 잠시 후 다시 시도하세요.',
  errBudgetInvalid: '월 token 한도는 양의 정수이거나 비워 두어야 합니다',
  errProviderInvalid: '유효한 제공자를 선택하세요',
  errBaseUrlRequired: '접속 주소를 입력하세요',
  errApiKeyRequired: 'API Key는 비워 둘 수 없습니다(로컬 엔드포인트는 아무 값이나 가능))',
  errRequestFailed: '요청 실패({status})',
  errUnknown: '알 수 없는 오류',
}
