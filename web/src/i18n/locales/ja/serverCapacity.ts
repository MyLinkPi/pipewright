/**
 * serverCapacity — サーバー選択時の 1 行キャパシティ概要(アプリストア配置、
 * ゲートウェイホスト選択など)。取得できた項目のみ「 · 」区切りで連結する。
 */
export default {
  containers: 'コンテナ {running} 実行 / {total}',
  noRuntime: 'コンテナランタイムなし',
  unreachable: '到達不可',
  load: '負荷 {load} / {cores} コア',
  mem: 'メモリ {used} / {total} {unit}',
  disk: 'ディスク {pct}%',
}
