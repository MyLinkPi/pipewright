export default {
  eyebrow: '运维大盘',
  title: '证书管理',
  subtitle: '平台证书统一管理 —— acme.sh 自动签发与续期(DNS-01,支持泛域名),也可导入现成证书;签发引擎运行在服务注册网关主机上,证书自动下发网关。',
  loadingAria: '正在加载证书列表',
  refreshingAll: '刷新中…',

  // 摘要卡
  summaryAria: '证书摘要',
  cardTotal: '证书总数',
  cardIssued: '已签发',
  cardPendingFailed: '申请中 / 失败',
  cardPendingFailedBreak: '{pending} 申请中 · {failed} 失败',
  cardSoonest: '最近到期',
  cardSoonestNone: '暂无到期信息',
  dayUnit: '天',

  // 引擎卡
  engine: {
    title: '签发引擎(acme.sh)',
    running: '运行中',
    stopped: '已停止',
    notInstalled: '未部署',
    unconfigured: '网关未配置',
    host: '网关主机',
    deploy: '部署引擎',
    deployed: '签发引擎已部署',
    deployFail: '部署签发引擎失败',
    hint: '签发引擎与证书签发/续期运行在网关主机上;创建证书时会自动部署,这里也可显式部署或检查状态。',
    gotoGateway: '前往服务注册',
  },

  btn: {
    create: '签发证书',
    import: '导入证书',
  },

  // 表头
  colDomains: '域名',
  colSource: '来源',
  colProvider: 'DNS 提供商',
  colStatus: '状态',
  colExpiry: '有效期',
  colAutoRenew: '自动续期',
  colActions: '操作',

  // 状态 / 来源
  statusIssued: '已签发',
  statusPending: '处理中',
  statusFailed: '失败',
  sourceAcme: 'acme.sh',
  sourceManual: '导入',

  // 到期文案
  daysLeft: '{n} 天后',
  expiresToday: '今天到期',
  expiredAgo: '已过期 {n} 天',

  // 自动续期 / 行操作
  autoOn: '开',
  autoOff: '关',
  autoRenewTitle: '切换自动续期(到期前 30 天)',
  renewTitle: '立即续期',
  redeployTitle: '按库内证书重新下发网关',
  deleteTitle: '删除证书',
  renewing: '已触发续期',
  renewFail: '续期触发失败',
  redeployed: '已重新下发网关',
  autoRenewToggled: '自动续期已更新',
  autoRenewFail: '更新自动续期失败',
  deleted: '证书已删除',
  deleteFail: '删除失败',
  delTitle: '删除证书?',
  delBody: '将删除 {domain} 的证书并停止自动续期;若某网关基域正在使用它,该基域将回退为仅 HTTP。',
  delConfirm: '删除',

  pollHint: '有证书正在签发/续期,自动刷新中…',

  // 创建(ACME)对话框
  create: {
    title: '签发新证书',
    primary: '主域名',
    primaryPh: '*.efg.com 或 efg.com',
    sans: '附加域名(每行一个,可含通配符)',
    sansPh: 'efg.com\nwww.efg.com',
    provider: 'DNS 提供商',
    errNoProvider: '请选择 DNS 提供商(DNS-01 需要;未绑定请先到设置里添加)',
    ca: '证书颁发机构',
    keyType: '密钥类型',
    autoRenew: '到期前 30 天自动续期',
    submit: '创建并签发',
    creating: '创建中…',
    success: '证书已创建,签发进行中',
    fail: '创建证书失败',
    hint: 'DNS-01 挑战:所选提供商必须托管每个域名的根区;通配符证书建议把裸域一并加进附加域名。',
  },

  // 导入对话框
  import: {
    title: '导入证书',
    certFile: '证书(fullchain.pem)',
    keyFile: '私钥(privkey.pem)',
    submit: '导入',
    importing: '导入中…',
    success: '证书已导入',
    fail: '导入失败',
    hint: '证书与私钥须配对;域名(SAN)自动从证书解析,覆盖的网关基域会自动下发。也可选择文件后继续在文本框里粘贴。',
  },

  // 空 / 错误
  empty: {
    title: '还没有证书',
    desc: '签发第一张 ACME 证书(DNS-01,支持泛域名),或导入现成证书。',
  },
  errTitle: '加载证书失败',
  errNetwork: '网络错误,请稍后重试。',

  close: '关闭',
  cancel: '取消',
  footNote: '签发与续期由平台集成的 acme.sh 完成(到期前 30 天自动续期);证书密文存于平台保险库,并自动下发到网关。',
}
