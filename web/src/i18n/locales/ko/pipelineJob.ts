export default {
  // ─── jobConfigSchema · shared option sets ──────────────────────────────
  artifactImage: "컨테이너 이미지 (image)",
  artifactJar: "JAR 패키지 (jar)",
  artifactDist: "정적 자산 (dist)",
  buildModelDockerfile: "자체 Dockerfile",
  buildModelToolchain: "플랫폼 툴체인",
  toolchainCustom: "사용자 지정",
  deployStrategyRolling: "롤링 업데이트(무중단)",
  deployStrategyRecreate: "중지 후 시작 (recreate)",
  deployStrategyBlueGreen: "블루-그린 배포",
  probeModeHttp: "HTTP 프로브",
  probeModeCommand: "명령 프로브",
  deployArtifactAuto: "자동(파일 아티팩트 우선, 없으면 이미지)",
  deployArtifactArchive: "아카이브 패키지 (archive)",

  // ─── jobConfigSchema · exec option fields ──────────────────────────────
  fieldTimeoutLabel: "타임아웃(초)",
  fieldTimeoutHint:
    "이 단계의 실행 타임아웃. 초과하면 컨테이너를 종료하고 실패 처리. 비움 / 0 = 무제한",
  fieldRetriesLabel: "실패 시 재시도 횟수",
  fieldRetriesHint:
    "0 이 아닌 종료 시 재실행하고, 상한에 도달해야 실패 처리. 사용자 취소는 재시도하지 않음. 비움 / 0 = 재시도 없음",
  fieldCpuLabel: "CPU 한도",
  fieldCpuHint:
    "docker --cpus 로 전달(예: 1, 0.5). 비움 = 무제한. 원격 runner 의 docker 지원에 따라 달라짐",
  fieldMemoryLabel: "메모리 한도",
  fieldMemoryHint: "docker --memory 로 전달(예: 512m, 2g). 비움 = 무제한",

  // ─── jobConfigSchema · script fields ───────────────────────────────────
  fieldImageLabel: "실행 이미지",
  fieldImageHint:
    "이 격리 컨테이너 안에서 명령 실행(Jenkins agent / 윈샤오 빌드 이미지에 해당)",
  jobRunnerHint: '비어 있으면 스테이지/프로젝트 기본값을 따릅니다(스테이지는 노드 설정, 프로젝트는 빌드 탭). 같은 셀렉터의 노드는 머신과 작업 공간을 공유하고, 다른 셀렉터의 노드는 각각 다른 머신에 배치됩니다.',
  fieldCommandsLabel: "실행 명령",
  fieldCommandsHint: "한 줄에 한 명령, 순서대로 실행",
  fieldWorkDirLabel: "작업 디렉터리",
  fieldWorkDirHint:
    "복제한 작업 공간 루트 기준 상대 경로. 비움 = 작업 공간 루트",
  fieldArtifactPathLabel: "아티팩트 경로",
  fieldArtifactPathHint:
    "선택, 한 줄에 한 개. 작업 공간 루트 기준 상대: 디렉터리→dist, *.jar→jar, 그 외 파일→archive. 한 노드에서 여러 개 출력 가능. 설정해야만 아티팩트 저장소에 보관되고 실행 상세에서 다운로드/배포 가능. 이미지 아티팩트는 「빌드」 노드(build_image)를 사용, 여기가 아님",
  fieldCachePathsLabel: "의존성 캐시 디렉터리",
  fieldCachePathsHint:
    "선택, 한 줄에 한 개, 작업 공간 루트 기준 상대. 설정해야만 캐시 활성화: 빌드 전 복원, 빌드 후 저장, 여러 실행에 걸쳐 의존성 재사용(node_modules/.m2/.gradle 등 재다운로드 방지). 캐시 문제는 빌드 결과에 절대 영향을 주지 않음",
  fieldCacheKeyLabel: "캐시 Key(선택)",
  fieldCacheKeyPlaceholder: "비움 = 브랜치 + lockfile 로 자동",
  fieldCacheKeyHint:
    "선택적 캐시 키 템플릿. 비우면 「브랜치 + 작업 공간 내 lockfile(package-lock/pom.xml/go.sum 등) 내용」으로 자동 도출: 의존성이 바뀌면 자동 콜드 빌드. {'{{매개변수}}'} 지원",

  // ─── jobConfigSchema · deploy ssh fields ───────────────────────────────
  fieldDeploySelectorLabel: "대상 셀렉터",
  fieldDeploySelectorHint: 'Pick label terms to select target hosts (combine with AND/OR); takes precedence over the server picker, empty = skip deploy',
  fieldServerIdLabel: "대상 서버",
  fieldServerIdHint: "등록된 서버 선택(자격 증명은 참조로 바인딩)",
  fieldArtifactTypeLabel: "배포 아티팩트 유형",
  fieldArtifactTypeHint:
    "이 run 이 이미지와 파일 아티팩트를 동시에 출력할 때 어느 것을 선택할지. 이미지는 대상 머신에서 docker pull → 새 컨테이너 시작 → 헬스 체크 → 실패 시 이전 이미지로 롤백",
  fieldDeployPathLabel: "배포 경로",
  fieldDeployPathHint:
    "파일 아티팩트: <배포 경로>/releases/<runId>/ 에 게시, current 심볼릭 링크가 이번 릴리스로 원자적 전환(무중단, 이전 릴리스는 롤백용으로 보존)",
  fieldContainerNameLabel: "컨테이너 이름",
  fieldContainerNameHint:
    "이미지 배포의 대상 컨테이너 이름. 비움 = 아티팩트 이름 사용. 동일 이름의 이전 컨테이너는 교체되며 이전 이미지로 롤백 가능",
  fieldPortsLabel: "포트 매핑",
  fieldPortsHint:
    "이미지 배포: 쉼표 / 공백 구분, 각 항목이 docker run -p <map> 로 확장",
  fieldRunArgsLabel: "docker run 인자",
  fieldRunArgsHint:
    "이미지 배포: docker run 에 그대로 추가(인자 자유. 자격 증명은 registry 로그인을 사용하고 여기에 쓰지 말 것)",
  fieldStrategyLabel: "배포 전략",
  fieldRestartCommandLabel: "재시작 / 전환 명령",
  fieldRestartCommandHint:
    "선택, 여러 줄(set -e, 한 줄씩 실행). 배포 후 대상 머신의 current 디렉터리에서 실행하여 재시작/리로드에 사용. 실패 시 자동 롤백",

  // ─── jobConfigSchema · git_source ──────────────────────────────────────
  typeGitSourceLabel: "소스 가져오기",
  typeGitSourceDesc: "Git 저장소를 빌드 작업 공간에 복제",
  fieldRepoUrlLabel: "저장소 URL",
  fieldRepoUrlHint: "비우면 프로젝트에 바인딩된 기본 저장소 사용",
  fieldBranchLabel: "브랜치 / Ref",
  fieldBranchHint: "비우면 트리거 시 브랜치 또는 프로젝트 기본 브랜치 사용",
  fieldCredentialIdLabel: "접근 자격 증명",
  fieldCredentialIdHint: "비공개 저장소는 Git 토큰 자격 증명 참조가 필요",
  fieldDepthLabel: "복제 깊이",
  fieldDepthHint: "얕은 복제 깊이. 비움 = 전체 이력",

  // ─── jobConfigSchema · build_image ─────────────────────────────────────
  typeBuildImageLabel: "빌드",
  typeBuildImageDesc: "아티팩트 빌드(이미지 / JAR / 정적 자산)",
  fieldArtifactTypeBuildLabel: "아티팩트 유형",
  fieldBuildModelLabel: "빌드 방식",
  fieldBuildModelHint: "자체 Dockerfile, 또는 플랫폼이 툴체인으로 빌드",
  fieldDockerfilePathLabel: "Dockerfile 경로",
  fieldContextLabel: "빌드 컨텍스트",
  fieldContextHint: "저장소 루트 기준 상대 빌드 컨텍스트 디렉터리",
  fieldToolchainLanguageLabel: "툴체인 언어",
  fieldToolchainVersionLabel: "툴체인 버전",
  fieldBuildCommandLabel: "빌드 명령",
  fieldBuildCommandHint: "툴체인 방식에서의 빌드 진입 명령",

  // ─── jobConfigSchema · push_image ──────────────────────────────────────
  typePushImageLabel: "이미지 푸시",
  typePushImageDesc: "이미지를 컨테이너 레지스트리로 푸시",
  fieldRegistryLabel: "이미지 레지스트리 URL",
  fieldImageNameLabel: "이미지 이름",
  fieldTagLabel: "태그",
  fieldTagHint:
    "사용 가능한 변수: {'${COMMIT_SHA}'} {'${BRANCH}'} {'${BUILD_NUMBER}'}",
  fieldRegistryCredentialLabel: "레지스트리 자격 증명",
  fieldRegistryCredentialHint: "이미지 레지스트리 로그인 자격 증명 참조",
  fieldRegistryUrlLabel: "이미지 레지스트리 URL",
  fieldRegistryUrlHint:
    "비워 두면 Docker Hub에 로그인; 프라이빗 레지스트리는 host[:port](https:// 없이) 입력, 이미지 참조와 일치해야 함",

  // ─── jobConfigSchema · deploy_ssh ──────────────────────────────────────
  typeDeploySshLabel: 'Host deploy',
  typeDeploySshDesc: 'Roll out file artifacts to target hosts over SSH: releases/<runId> dir + atomic current symlink + restart command + health gate + auto-rollback; supports batched rolling.',

  // ─── jobConfigSchema · health_check ────────────────────────────────────
  typeHealthCheckLabel: "헬스 게이트",
  typeHealthCheckDesc: "배포 후 헬스 프로브, 실패 시 롤백",
  fieldProbeModeLabel: "프로브 방식",
  fieldUrlLabel: "프로브 URL",
  fieldExpectStatusLabel: "예상 상태 코드",
  fieldCommandLabel: "프로브 명령",
  fieldHealthRetriesLabel: "재시도 횟수",
  fieldIntervalSecondsLabel: "재시도 간격(초)",

  // ─── jobConfigSchema · notify ──────────────────────────────────────────
  typeNotifyLabel: "알림",
  typeNotifyDesc: "실행이 이 노드에 도달하면 알림 전송",
  fieldChannelLabel: "알림 채널",
  fieldChannelHint:
    "「알림」 설정에서 이미 구성한 채널 중 선택. 없으면 먼저 설정에서 추가",
  fieldTitleTemplateLabel: "제목 템플릿(선택)",
  fieldTitleTemplatePlaceholder: "배포 성공: {'{{project}}'}",
  fieldTitleTemplateHint:
    "비움 = 기본 문구. 자리표시자 {'{{project}}'} {'{{branch}}'} {'{{commit}}'} {'{{status}}'} {'{{runId}}'} 지원",
  fieldBodyTemplateLabel: "본문 템플릿(선택)",
  fieldBodyTemplatePlaceholder:
    "브랜치 {'{{branch}}'} {'@'} {'{{commit}}'} 가 알림 노드까지 실행되었습니다.",
  fieldBodyTemplateHint:
    "비움 = 기본 문구. 마찬가지로 {'{{자리표시자}}'} 지원, 알 수 없는 것은 빈 값으로 렌더링",

  // ─── jobConfigSchema · template nodes ──────────────────────────────────
  typeBuildFrontendLabel: "프런트엔드 빌드",
  typeBuildFrontendDesc: "Node 컨테이너에서 의존성 설치 + 빌드, dist 출력",
  typeBuildBackendLabel: "백엔드 빌드",
  typeBuildBackendDesc: "Maven/Gradle 컨테이너에서 패키징, jar 출력",
  typeDeployFrontendLabel: "프런트엔드 푸시 배포",
  typeDeployFrontendDesc:
    "프런트엔드 dist 를 SSH 를 통해 무중단으로 서버에 배포",
  typeTemplatedLabel: "사용자 지정 노드",
  typeTemplatedDesc:
    "직접 매개변수 + 명령 템플릿({'{{매개변수}}'})을 정의, 매개변수만 설정하면 사용",
  fieldParamsLabel: "매개변수 표",
  fieldParamsHint:
    "한 줄에 하나의 key=value, 완전 자유. 명령 템플릿/아티팩트 경로에서 {'{{key}}'} 로 참조",
  fieldCommandTemplateLabel: "명령 템플릿",
  fieldCommandTemplateHint:
    "여러 줄. {'{{매개변수}}'} 는 매개변수 표의 값으로 치환, $ENV 는 컨테이너 내 shell 에 위임",
  fieldTemplatedArtifactPathHint:
    "선택, 한 줄에 하나, {'{{매개변수}}'} 와 와일드카드 지원. 디렉터리→dist, *.jar→jar, 그 외→archive",
  fieldTemplatedWorkDirHint: "선택, 복제한 작업 공간 루트 기준 상대",
  fieldTemplatedCachePathsHint:
    "선택, 한 줄에 하나, 작업 공간 루트 기준 상대, {'{{매개변수}}'} 지원. 설정해야만 캐시 활성화: 빌드 전 복원, 빌드 후 저장, 실행에 걸쳐 의존성 재사용. 캐시 문제는 빌드 결과에 절대 영향을 주지 않음",
  fieldTemplatedCacheKeyHint:
    "선택적 캐시 키 템플릿, {'{{매개변수}}'} 지원. 비우면 브랜치 + lockfile 내용으로 자동 도출(의존성이 바뀌면 자동 콜드 빌드)",
  typeScriptLabel: "사용자 지정 스크립트",
  typeScriptDesc: "격리 컨테이너 안에서 임의의 명령 실행",

  // ─── jobConfigSchema · categories ──────────────────────────────────────
  categorySource: "소스",
  categoryBuild: "빌드 및 아티팩트",
  categoryDeploy: "배포",
  categoryQuality: "품질 게이트",
  categoryNotify: "알림",
  categoryCustom: "사용자 지정",

  // ─── stepCompile · step kind meta ──────────────────────────────────────
  stepCommandLabel: "명령 실행",
  stepEnvLabel: "환경 변수 설정",
  stepWorkDirLabel: "디렉터리 전환",
  stepArtifactLabel: "아티팩트 업로드",
  stepConditionLabel: "조건 가드",

  // ─── studioCompile · catalog groups + items ────────────────────────────
  studioGroupBasic: "기본",
  studioGroupEnvDir: "환경 및 디렉터리",
  studioGroupQuality: "품질 게이트",
  studioGroupArtifact: "아티팩트",
  studioGroupControl: "제어 흐름",
  studioGroupDoc: "문서",
  studioStepCommand: "명령 실행",
  studioStepInstall: "의존성 설치",
  studioStepEcho: "로그 출력",
  studioStepEnv: "환경 변수 설정",
  studioStepWorkDir: "디렉터리 전환",
  studioStepPath: "PATH 추가",
  studioStepTest: "테스트 실행 + 보고서",
  studioStepHealthcheck: "헬스 체크",
  studioStepArtifact: "아티팩트 업로드",
  studioStepDownload: "파일 다운로드",
  studioStepExtract: "아카이브 압축 해제",
  studioStepCondition: "조건 가드",
  studioStepRetry: "재시도 래퍼",
  studioStepTimeout: "타임아웃 제한",
  studioStepSleep: "대기 / 지연",
  studioStepNote: "메모",
  studioDefaultCategory: "사용자 지정",

  // ─── stageSettings · when events ───────────────────────────────────────
  whenManual: "수동",
  whenSchedule: "예약",

  // ─── stageSettings · post conditions ───────────────────────────────────
  postAlways: "항상",
  postOnSuccess: "성공 시",
  postOnFailure: "실패 시",

  // ─── stageSettings · matrix errors ─────────────────────────────────────
  matrixAxisInvalid:
    "축 이름 「{name}」 이(가) 유효하지 않습니다(식별자여야 함: 문자/밑줄로 시작)",
  matrixAxisNeedsValue: "축 「{name}」 에는 최소 하나의 값이 필요합니다",
  matrixTooManyAxes:
    "매트릭스 차원 {count} 이(가) 상한 {max} 을(를) 초과했습니다",
  matrixTooManyCells:
    "매트릭스가 {cells} 개 셀로 확장되어 상한 {max} 을(를) 초과했습니다",

  // ─── JobDrawer.vue ─────────────────────────────────────────────────────
  drawerAria: "작업 설정",
  unnamedJob: "(이름 없는 작업)",
  thisPipeline: "이 파이프라인",
  closeDrawer: "서랍 닫기",
  basicInfo: "기본 정보",
  jobNameLabel: "작업 이름",
  jobNamePlaceholder: "예: 격리 빌드",
  jobTypeLabel: "작업 유형",
  changeType: "작업 유형 변경",
  change: "변경",
  summaryLabel: "요약 설명",
  summaryPlaceholder: "카드 부제(선택)",
  instanceParams: "인스턴스 매개변수",
  instanceParamsHint:
    "이 사용자 지정 노드는 다음 매개변수를 공개합니다. 필요에 따라 조정하세요. 나머지 명령 템플릿은 노드 작성자가 고정했습니다.",
  advancedRawToggle: "고급 · 원본 매개변수 보기/편집",
  configSuffix: " 설정",
  configView: "설정 보기",
  visualSteps: "비주얼 단계",
  rawParams: "원본 매개변수",
  execAdvancedToggle: "고급 · 타임아웃 / 재시도 / 리소스 / 캐시",
  toggleEnable: "활성화",
  stepBuilderHint:
    "단계는 여러 줄 명령으로 컴파일되어 격리 컨테이너 안에서 실행됩니다. 디렉터리 전환 / 환경 변수 설정은 후속 명령에 적용됩니다. 언제든지 「원본 매개변수」로 전환하여 확인할 수 있습니다.",
  advancedRawParams: "고급 · 원본 매개변수",
  kvEmptyWithSpec:
    "이 유형의 매개변수는 위 양식에 있습니다. 여기에 사용자 지정 키-값을 추가할 수 있습니다",
  kvEmptyNoSpec: "이 유형에 사용자 지정 키-값 매개변수 추가",
  kvKeyPlaceholder: "key",
  kvValuePlaceholder: "value",
  kvKeyAria: "설정 키 {n}",
  kvValueAria: "설정 값 {n}",
  kvDelAria: "설정 항목 {label} 삭제",
  addParam: "매개변수 추가",
  credUnselected: "— 미선택 —",
  channelUnselected: "— 채널 선택 —",
  channelDisabled: "(비활성화됨)",
  saveAsCustomNode: "사용자 지정 노드로 저장",
  savedToLibrary: "✓ 재사용 라이브러리에 저장됨",
  savePanelHint:
    "현재 노드의 유형과 매개변수 스냅샷을 재사용 라이브러리에 저장하면, 이후 어떤 파이프라인에도 선택하여 재사용할 수 있습니다.",
  saveNamePlaceholder: "노드 이름(라이브러리 내 고유)",
  saveNameAria: "사용자 지정 노드 이름",
  saveDescPlaceholder: "설명(선택)",
  saveDescAria: "사용자 지정 노드 설명",
  cancel: "취소",
  saving: "저장 중…",
  save: "저장",
  saveErrEmptyName: "노드 이름을 입력하세요",
  saveErrFailed: "저장 실패, 다시 시도하세요(이름 중복 가능성)",
  channelTypeEmail: "이메일",
  channelTypeFeishu: "Feishu",
  channelTypeWecom: "WeCom",
  channelTypeDingtalk: "DingTalk",

  // ─── StepBuilder.vue ───────────────────────────────────────────────────
  sbVisualSteps: "비주얼 단계",
  sbStepCount: "단계 수",
  sbDragSort: "드래그하여 정렬",
  sbMoveUp: "단계 {n} 위로",
  sbMoveDown: "단계 {n} 아래로",
  sbDelStep: "단계 {n} 삭제",
  sbCommandAria: "명령 {n}",
  sbEnvKeyAria: "환경 변수 이름 {n}",
  sbEnvValueAria: "환경 변수 값 {n}",
  sbDirAria: "디렉터리 {n}",
  sbCondAria: "조건 {n}",
  sbCondHintPre: "shell 조건, 충족되지 않으면 ",
  sbCondHintStrong: "후속 모든 단계를 건너뜀",
  sbCondHintPost: "(성공으로 종료). 예",
  sbArtifactAria: "아티팩트 경로 {n}",
  sbEmpty: "아직 단계가 없습니다. 아래의 「단계 추가」를 눌러 시작하세요",
  sbAddStep: "단계 추가",
  sbAddCommandDesc: "shell 명령(여러 줄 가능)",
  sbAddEnvDesc: "KEY=VALUE 를 환경에 주입",
  sbAddWorkDirDesc: "후속 명령의 작업 디렉터리 전환",
  sbAddConditionDesc: "충족되지 않으면 후속 단계 건너뜀",
  sbAddArtifactDesc: "glob 로 아티팩트 아카이브",
  typeBuildNodejsLabel: 'Node.js build',
  typeBuildNodejsDesc: 'Install deps and build inside a node container, producing dist. Great for package.json projects.',
  typeBuildJavaLabel: 'Java build',
  typeBuildJavaDesc: 'Package inside a Maven container, producing a jar (Gradle: just swap image and command).',
  typeBuildGolangLabel: 'Golang build',
  typeBuildGolangDesc: 'Compile inside a golang container, producing a binary.',
  typeBuildPythonLabel: 'Python build',
  typeBuildPythonDesc: 'Install deps and build inside a python container, producing wheels/sdists.',
  typeDeployContainerLabel: 'Container deploy',
  typeDeployContainerDesc: 'Deploy an image on target hosts: pull → remove old container → run new one → health gate → rollback to previous image on failure.',
  deployModeArtifact: 'Deploy artifact',
  deployModeCommand: 'Restart command only',
  fieldDeployModeLabel: 'Deploy content',
  fieldDeployModeHint: 'Deploy the selected artifact; or touch no artifact and only run the restart command on targets.',
  fieldArtifactJobLabel: 'Artifact source',
  fieldArtifactJobHint: 'Pick exactly which node produced the artifact to deploy (explicit binding, no auto guessing).',
  fieldArtifactJobImageHint: 'Pick a build_image node that produces the image.',
  fieldArtifactNameLabel: 'Artifact name filter',
  fieldArtifactNameHint: 'Glob to disambiguate when the source node emits multiple artifacts (e.g. app-*.jar).',
  fieldFirstBatchSizeLabel: 'First batch size',
  fieldFirstBatchSizeHint: 'Hosts in the first rolling batch (default 1 — verify small first).',
  fieldBatchSizeLabel: 'Batch size',
  fieldBatchSizeHint: 'Hosts upgraded simultaneously per batch after the first (empty/0 = all remaining at once; any batch failure stops the rollout).',
  fieldHealthUrlLabel: 'Health check URL',
  fieldHealthUrlHint: 'curl-probed on the target after deploy; failure triggers rollback. Empty = no gate.',
  fieldHealthCommandLabel: 'Health check command',
  fieldHealthCommandHint: 'Command probe (ignored when URL is set).',
  fieldHealthRetriesHint: 'Probe retries (default 3).',
  fieldHealthIntervalLabel: 'Probe interval (s)',
  fieldHealthIntervalHint: 'Seconds between probes (default 3).',
  fieldSelectorModeLabel: 'Label matching',
  fieldSelectorModeHint: 'How multiple label terms combine: match all (AND) or match any (OR).',
  selectorModeAll: 'Match all terms (AND)',
  selectorModeAny: 'Match any term (OR)',
  fieldArtifactPackModeLabel: 'Artifact packing',
  fieldArtifactPackModeHint: 'Pack directories into a tar.gz (default); or store as a file manifest without packing.',
  artifactPackTar: 'Pack as tar.gz',
  artifactPackNone: 'No packing (file manifest)',
  fieldTarLayoutLabel: 'Archive layout',
  fieldTarLayoutHint: 'Contents at archive root (unpacks into the release dir) = legacy; or keep the top-level directory.',
  tarLayoutContents: 'Contents at root (no dir prefix)',
  tarLayoutTop: 'Keep top-level directory',
  fieldArtifactRenameLabel: 'Artifact file name',
  fieldArtifactRenameHint: 'Optional: file name on the target host for single-file artifacts (default: original).',
  fieldHealthSelectorHint: 'Pick label terms to select hosts to probe; empty + http probe falls back to the platform host.',
  selectorNoServers: 'No servers yet — add one in Server management first',
  selectorNoLabels: 'No labels yet — label your servers in Server management first',
  selectorKeyAria: 'Label key',
  selectorValueAria: 'Label value',
  selectorDelAria: 'Remove label term {k}',
  selectorAddTerm: 'Add label term',
  producerUnselected: 'Select the artifact source node',
};
