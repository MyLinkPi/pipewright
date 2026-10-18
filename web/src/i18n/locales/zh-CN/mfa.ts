/**
 * 两步验证(TOTP + Passkey)词条。命名空间 mfa:登录页验证码步骤在 login.*,
 * 此处覆盖设置页卡片、开启向导与管理员操作。
 */
export default {
  // —— 卡片总览 ——
  title: '两步验证',
  subtitle: '为账户增加一道独立于密码的登录防线。',
  totpCardTitle: '验证器 App(TOTP)',
  totpCardDesc: '使用验证器 App(如 Google Authenticator、1Password、Microsoft Authenticator)生成 6 位动态码。',
  totpOn: '已开启',
  totpOff: '未开启',
  backupRemaining: '剩余备份码:{n} 个',
  passkeyCardTitle: '通行密钥(Passkey)',
  passkeyCardDesc: '使用指纹 / 面容 / 硬件密钥一步登录,免密码免验证码。可绑定多台设备。',
  passkeyAdd: '添加通行密钥',
  passkeyNameLabel: '设备名称',
  passkeyNamePlaceholder: '如:MacBook 指纹',
  passkeyLastUsed: '最近使用',
  passkeyNever: '从未使用',
  passkeyEmpty: '尚未绑定通行密钥。',

  // —— 开启向导 ——
  wizardTitle: '开启两步验证',
  stepScan: '扫码添加验证器',
  stepScanDesc: '用验证器 App 扫描二维码,或手动输入以下密钥:',
  copySecret: '复制密钥',
  copied: '已复制',
  stepBackup: '保存备份码',
  stepBackupDesc: '每个备份码只能使用一次。丢失验证器时用它登录,请离线妥善保存。',
  stepBackupWarning: '备份码仅本次展示,关闭后将无法再次查看。',
  stepVerify: '验证并开启',
  stepVerifyDesc: '输入验证器当前显示的 6 位动态码完成开启。',
  next: '下一步',
  back: '上一步',
  confirmEnable: '确认开启',
  backupDownload: '下载备份码',
  prevStep: '上一步',

  // —— 关闭 / 重生成 ——
  disableTitle: '关闭两步验证',
  disableConfirm: '输入动态码或备份码以确认关闭:',
  disable: '关闭两步验证',
  regenerateBackup: '重新生成备份码',
  regenerateTitle: '重新生成备份码',
  regenerateDesc: '旧备份码将全部作废,新备份码仅本次展示。',
  codeLabel: '验证码 / 备份码',
  confirm: '确认',

  // —— 状态 / 错误 ——
  errCode: '验证码或备份码错误',
  errLocked: '尝试次数过多,请稍后再试',
  errPasskeyUnsupported: '当前浏览器不支持通行密钥',
  errPasskeyFailed: '通行密钥操作失败或已取消',
  errInsecure: '通行密钥需要 HTTPS 访问(localhost 除外)',
  enabled: '两步验证已开启',
  disabled: '两步验证已关闭',
  backupRegenerated: '备份码已重新生成',
  passkeyAdded: '通行密钥已添加',
  passkeyRemoved: '通行密钥已移除',
  cancel: '取消',
  remove: '移除',
  removeConfirm: '确定移除该通行密钥?移除后该设备将无法用于登录。',

  // —— 管理员(用户管理页)——
  adminColTotp: '两步验证',
  adminColPasskeys: '通行密钥',
  adminResetTotp: '重置两步验证',
  adminResetPasskeys: '清除通行密钥',
  adminResetTotpConfirm: '确定重置 {username} 的两步验证?其验证器与备份码将全部失效。',
  adminResetPasskeysConfirm: '确定清除 {username} 的全部通行密钥?',
  adminResetDone: '已重置',
}
