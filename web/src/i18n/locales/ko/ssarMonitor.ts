export default {
  title: '머신 모니터링',
  subtitle:
    'ssar(자체 유지 관리 버전) 기반 서버 모니터링: 대상 머신에 배포해 로컬에서 지속 수집하고, 플랫폼은 주기 샘플을 저장해 개요를 제공하며, 각 머신은 7일간의 세밀한 이력을 드릴다운할 수 있습니다',
  errLoadTitle: '로드 실패',

  binHeading: 'ssar 패키지',
  binEmpty: '캐시된 패키지 없음(GitHub Release: MyLinkPi/ssar)',
  binDownloading: '패키지 다운로드 중…',
  binVersion: '캐시됨 {version}(패키지 {n}개)',
  binErr: '마지막 다운로드 실패: {err}',
  binDownload: '패키지 다운로드',
  binRedownload: '다시 다운로드',
  installAll: '전체 설치 / 업그레이드',

  installAllOk: '{n}대 설치 성공',
  installPartial: '{ok}대 성공, {fail}대 실패',
  installFailed: '설치 요청 실패',

  serversHeading: '서버',
  colServer: '서버',
  colStatus: 'ssar 상태',
  colLastSample: '최근 샘플',
  colActions: '작업',
  notInstalled: '미설치',
  collectorActive: '수집 중',
  collectorStopped: '수집기 중지',
  install: '설치',
  upgrade: '업그레이드',

  detailHeading: '{name} · 모니터링 상세',
  minutesLabel: '{n}분',
  hoursLabel: '{n}시간',
  daysLabel: '{n}일',

  chartCpu: 'CPU(jiffies/s, 전체 CPU 합계)',
  chartMem: '메모리 사용 %(페이지 캐시 포함)',
  chartLoad: '부하 및 실행 큐',
  chartTcp: 'TCP 연결 및 재전송',
  cpuUser: 'user',
  cpuSystem: 'system',
  cpuIowait: 'iowait',
  cpuSteal: 'steal',
  memUsed: '사용',
  load1: 'load1',
  runq: '실행 큐',
  tcpEstab: '현재 연결',
  tcpRetrans: '재전송/s',

  drillView: '상세 보기',
  drillMachineLocal:
    '상세 데이터는 머신 로컬 ssar 이력(기본 7일)에서 읽어옵니다. 시간 열은 머신 로컬 시간입니다',
  drillTime: '시간(머신 로컬)',

  chartEmpty: '데이터 없음',
  chartAria: '지표 추이 선 그래프',
}
