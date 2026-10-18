/**
 * Two-factor authentication (TOTP + Passkey) strings. Namespace `mfa`:
 * the login-page code step lives in login.*; this file covers the settings
 * cards, the enable wizard, and admin actions.
 */
export default {
  // —— overview cards ——
  title: 'Two-Factor Authentication',
  subtitle: 'Add a second login factor independent of your password.',
  totpCardTitle: 'Authenticator App (TOTP)',
  totpCardDesc: 'Generate 6-digit codes with an authenticator app such as Google Authenticator, 1Password, or Microsoft Authenticator.',
  totpOn: 'Enabled',
  totpOff: 'Disabled',
  backupRemaining: 'Backup codes left: {n}',
  passkeyCardTitle: 'Passkeys',
  passkeyCardDesc: 'Sign in with a fingerprint, face, or hardware key — no password, no code. Multiple devices supported.',
  passkeyAdd: 'Add passkey',
  passkeyNameLabel: 'Device name',
  passkeyNamePlaceholder: 'e.g. MacBook Touch ID',
  passkeyLastUsed: 'Last used',
  passkeyNever: 'Never used',
  passkeyEmpty: 'No passkeys registered yet.',

  // —— enable wizard ——
  wizardTitle: 'Enable two-factor authentication',
  stepScan: 'Scan with your authenticator',
  stepScanDesc: 'Scan the QR code with your authenticator app, or enter this secret manually:',
  copySecret: 'Copy secret',
  copied: 'Copied',
  stepBackup: 'Save your backup codes',
  stepBackupDesc: 'Each backup code can be used once. Use one to sign in if you lose your authenticator — store them offline.',
  stepBackupWarning: 'Backup codes are shown only once and cannot be viewed again.',
  stepVerify: 'Verify and enable',
  stepVerifyDesc: 'Enter the current 6-digit code from your authenticator to finish enabling.',
  next: 'Next',
  back: 'Back',
  confirmEnable: 'Enable now',
  backupDownload: 'Download codes',
  prevStep: 'Back',

  // —— disable / regenerate ——
  disableTitle: 'Disable two-factor authentication',
  disableConfirm: 'Enter a code or backup code to confirm:',
  disable: 'Disable 2FA',
  regenerateBackup: 'Regenerate backup codes',
  regenerateTitle: 'Regenerate backup codes',
  regenerateDesc: 'All existing backup codes will be invalidated. The new ones are shown only once.',
  codeLabel: 'Code / backup code',
  confirm: 'Confirm',

  // —— status / errors ——
  errCode: 'Incorrect code or backup code',
  errLocked: 'Too many attempts, please try again later',
  errPasskeyUnsupported: 'This browser does not support passkeys',
  errPasskeyFailed: 'Passkey operation failed or was cancelled',
  errInsecure: 'Passkeys require HTTPS (except localhost)',
  enabled: 'Two-factor authentication enabled',
  disabled: 'Two-factor authentication disabled',
  backupRegenerated: 'Backup codes regenerated',
  passkeyAdded: 'Passkey added',
  passkeyRemoved: 'Passkey removed',
  cancel: 'Cancel',
  remove: 'Remove',
  removeConfirm: 'Remove this passkey? The device will no longer be able to sign in.',

  // —— admin (users page) ——
  adminColTotp: '2FA',
  adminColPasskeys: 'Passkeys',
  adminResetTotp: 'Reset 2FA',
  adminResetPasskeys: 'Remove passkeys',
  adminResetTotpConfirm: "Reset this member's two-factor authentication? Their authenticator and backup codes will stop working.",
  adminResetPasskeysConfirm: "Remove all of this member's passkeys?",
  adminResetDone: 'Reset complete',
}
