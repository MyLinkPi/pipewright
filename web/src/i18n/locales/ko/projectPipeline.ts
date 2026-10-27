export default {
  // ─── 상단 바 / 브레드크럼 ──────────────────────────────────────
  breadcrumbAria: '브레드크럼 내비게이션',
  breadcrumbProjects: '프로젝트',
  title: '파이프라인 설정',

  // ─── 탭 ─────────────────────────────────────────────────────────
  tabCanvas: '파이프라인 캔버스',
  tabBuild: '빌드',
  tabVars: '변수 및 캐시',
  tabTriggers: '트리거 설정',
  tabStripAria: '파이프라인 설정 탭',

  // ─── 코드형 파이프라인 (GitOps · FR-8-12) ───────────────────────
  pacTitle: '코드형 파이프라인',
  pacOnHint: '실행 시 실행 브랜치의 저장소 .pipewright.yml을 읽어 구동합니다(파일이 없거나 유효하지 않으면 이 설정으로 폴백).',
  pacOffHint: '켜면 각 실행이 실행 브랜치의 저장소 루트 .pipewright.yml을 읽어 구동합니다(없거나 유효하지 않으면 이 설정으로 폴백).',
  pacToggleFailed: '전환에 실패했습니다. 다시 시도하세요.',

  // ─── PR 상태 검사(커밋 상태 회신 · Story 8-9 / FR-8-9)──────────────
  prStatusTitle: 'PR 상태 검사',
  prStatusOnHint: '실행이 끝나면 프로젝트 저장소에서 GitHub/Gitee를 식별하고, 프로젝트 자격 증명으로 해당 커밋의 성공/실패 상태를 PR 검사로 회신합니다(최선 노력. 실패해도 실행에는 영향 없음).',
  prStatusOffHint: '켜면 실행이 끝날 때 저장소에서 GitHub/Gitee를 식별하고, 프로젝트 자격 증명으로 커밋 상태를 PR 검사로 회신합니다(최선 노력).',
  prStatusToggleFailed: '전환에 실패했습니다. 다시 시도하세요.',

  // ─── 저장소 구성 미리보기 (GitOps · ref 지정해 .pipewright.yml 가져와 검증) ──
  pacPreviewBtn: '저장소 구성 미리보기',
  pacPreviewTitle: '저장소 구성 미리보기',
  pacPreviewSub: '브랜치/태그/커밋을 지정해 저장소의 파일을 가져와 검증합니다:',
  pacPreviewCloseAria: '미리보기 닫기',
  pacPreviewCloseBtn: '닫기',
  pacPreviewRefLabel: '브랜치 / 태그 / 커밋',
  pacPreviewFetch: '가져와 검증',
  pacPreviewNotFound: '{ref}에서 {file}을 찾을 수 없습니다. 실행 시 여기서 구성한 파이프라인으로 폴백합니다.',
  pacPreviewInvalid: '{ref}의 {file} 검증에 실패했습니다. 실행 시 여기서 구성한 파이프라인으로 조용히 폴백합니다:',
  pacPreviewValid: '{ref}의 {file} 검증에 성공했습니다(스테이지 {count}개). 실행 시 이를 사용합니다.',
  pacPreviewJobCount: '작업 {count}개',
  pacPreviewConnFailed: '연결에 실패했습니다. 네트워크를 확인하고 다시 시도하세요.',
  pacPreviewFailed: '미리보기에 실패했습니다({status}). 다시 시도하세요.',
  pacPreviewFailedRetry: '미리보기에 실패했습니다. 다시 시도하세요.',
  pacPreviewFileLabel: '파이프라인 파일',
  pacPreviewSetDefault: '프로젝트 기본 파일로 설정',
  pacPreviewSetDefaultDone: '프로젝트 기본 파일로 설정했습니다',
  pacPreviewSetDefaultFailed: '기본 파일 설정에 실패했습니다. 다시 시도하세요.',
  pacPreviewSetDefaultSame: '프로젝트 기본값과 동일하여 저장할 필요 없음',
  pacPreviewIdle: '브랜치와 파이프라인 파일을 확인한 뒤 «가져와서 검증»을 클릭하세요.',
  pacPreviewVarsTitle: '선언된 변수({count})',
  pacPreviewVarsHint: '값은 «변수 및 캐시» 탭에서 입력합니다. secret 변수는 Vault 자격 증명을 참조하며 YAML에 직접 쓰지 않습니다.',
  pacVarRequired: '필수',
  pacVarSecret: 'secret',

  // ─── 툴바 버튼 ──────────────────────────────────────────────────
  aiGenerate: 'AI 파이프라인 생성',
  importYaml: 'YAML에서 가져오기',
  templates: '템플릿',
  validate: '설정 검증',
  closeValidationPanel: '검증 패널 닫기',
  badgeReady: '준비됨',
  badgeErrors: '오류 {n}개',
  saving: '저장 중…',
  saveDraft: '저장',

  // ─── 배너 / 상태 ────────────────────────────────────────────────
  dismiss: '닫기',
  draftSaved: '파이프라인이 저장되었습니다',
  scopeSettings: '변수',
  scopePipeline: '파이프라인 정의',
  noChanges: '저장되지 않은 변경 없음',
  errSavePartial: '일부만 저장됨({saved});실패: ',
  replaceDirtyTitle: '저장되지 않은 캔버스 변경',
  replaceDirtyBody: '이 작업은 캔버스 전체를 교체합니다. 저장되지 않은 변경 사항은 사라집니다. 계속할까요?',
  replaceDirtyConfirm: '버리고 계속',
  tabDirtyAria: '저장되지 않은 변경',
  retry: '다시 시도',
  loading: '로딩 중',

  // ─── 로드 오류 ──────────────────────────────────────────────────
  errNoServer: '서버에 연결할 수 없습니다. 백엔드가 실행 중인지 확인한 후 다시 시도하세요.',
  errProjectNotFound: '프로젝트가 존재하지 않습니다. 프로젝트 ID가 올바른지 확인하세요.',
  errLoadFailedStatus: '파이프라인 로드 실패({status})',
  errLoadFailedRetry: '파이프라인 로드에 실패했습니다. 잠시 후 다시 시도하세요.',

  // ─── 저장 오류 ──────────────────────────────────────────────────
  errSaveFailedRetry: '저장에 실패했습니다. 잠시 후 다시 시도하세요.',
  errSettingsNotLoaded: '설정을 불러오지 못해 아무것도 저장되지 않았습니다. 페이지를 새로 고침한 후 다시 시도하세요.',
  errSaveFailedStatus: '저장 실패({status})',
  errInvalidStage: '스테이지 이름은 비워 둘 수 없으며 kind는 허용된 값이어야 합니다. 확인 후 다시 시도하세요.',
  errInvalidJob: '작업 이름 또는 유형은 비워 둘 수 없습니다. 입력 후 다시 시도하세요.',
  errDuplicateId: '스테이지 또는 작업 ID가 중복됩니다. 중복 항목을 삭제한 후 다시 시도하세요.',
  errInvalidBuild: '빌드 모델은 dockerfile/toolchain이어야 하고, 산출물 유형은 image/jar/dist여야 합니다.',
  errInvalidVar: '변수 키는 비워 둘 수 없고 동일 범위 내에서 중복될 수 없습니다. secret 변수에는 볼트 자격 증명을 선택해야 합니다.',
  errCredentialNotFound: '참조된 볼트 자격 증명이 존재하지 않습니다. 다시 선택한 후 시도하세요.',
  errVaultUnconfigured: '볼트에 master key가 설정되지 않아 secret 자격 증명을 참조할 수 없습니다.',
}
