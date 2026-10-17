/**
 * serverCapacity — 서버 선택 시 한 줄 용량 요약(앱스토어 배포, 게이트웨이 호스트
 * 선택 등). 사용 가능한 항목만 ' · ' 구분자로 연결한다.
 */
export default {
  containers: '컨테이너 {running} 실행 / {total}',
  noRuntime: '컨테이너 런타임 없음',
  unreachable: '접속 불가',
  load: '부하 {load} / {cores}코어',
  mem: '메모리 {used} / {total} {unit}',
  disk: '디스크 {pct}%',
}
