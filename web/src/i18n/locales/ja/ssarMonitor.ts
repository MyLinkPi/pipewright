export default {
  title: 'マシン監視',
  subtitle:
    'ssar(自家保守版)によるサーバー監視:対象機に配布してローカルで継続採取し、プラットフォーム側は周期サンプルを保存して概観表示。各機は 7 日間の高粒度履歴をドリルダウン可能',
  errLoadTitle: '読み込みに失敗しました',

  binHeading: 'ssar パッケージ',
  binEmpty: 'パッケージ未キャッシュ(GitHub Release:MyLinkPi/ssar)',
  binDownloading: 'パッケージをダウンロード中…',
  binVersion: 'キャッシュ済み {version}(パッケージ {n} 件)',
  binErr: '前回のダウンロード失敗:{err}',
  binDownload: 'パッケージをダウンロード',
  binRedownload: '再ダウンロード',
  installAll: '全台にインストール / アップグレード',

  installAllOk: '{n} 台インストール成功',
  installPartial: '{ok} 台成功、{fail} 台失敗',
  installFailed: 'インストールリクエストに失敗しました',

  serversHeading: 'サーバー',
  colServer: 'サーバー',
  colStatus: 'ssar 状態',
  colLastSample: '最終サンプル',
  colActions: '操作',
  notInstalled: '未インストール',
  collectorActive: '採取中',
  collectorStopped: 'コレクター停止',
  install: 'インストール',
  upgrade: 'アップグレード',

  detailHeading: '{name} · 監視詳細',
  minutesLabel: '{n} 分',
  hoursLabel: '{n} 時間',
  daysLabel: '{n} 日',

  chartCpu: 'CPU(jiffies/s、全 CPU 合計)',
  chartMem: 'メモリ使用 %(ページキャッシュ込み)',
  chartLoad: '負荷と実行キュー',
  chartTcp: 'TCP 接続と再送',
  cpuUser: 'user',
  cpuSystem: 'system',
  cpuIowait: 'iowait',
  cpuSteal: 'steal',
  memUsed: '使用',
  load1: 'load1',
  runq: '実行キュー',
  tcpEstab: '確立接続',
  tcpRetrans: '再送/s',

  drillView: 'ドリルダウンビュー',
  drillMachineLocal:
    'ドリルダウンはマシンローカルの ssar 履歴(既定 7 日)から読み出し。時刻列はマシンのローカル時刻です',
  drillTime: '時刻(マシンローカル)',

  chartEmpty: 'データなし',
  chartAria: '指標の推移折れ線グラフ',
}
