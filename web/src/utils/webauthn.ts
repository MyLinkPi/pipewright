/**
 * WebAuthn(Passkey)浏览器侧工具:base64url ↔ ArrayBuffer 转换、服务端选项的
 * 浏览器化预处理、navigator.credentials 结果的序列化,以及能力探测。
 *
 * 服务端(go-webauthn)JSON 里的 challenge / user.id / credentials[].id 是
 * base64url 字符串,浏览器 API 需要 ArrayBuffer;回传时再转回 base64url。
 * 不引第三方库,原生 atob/btoa 即可(纯浏览器环境)。
 */

/** base64url 字符串 → ArrayBuffer(空串/空值返回空 buffer)。 */
export function base64UrlToArrayBuffer(value?: string | null): ArrayBuffer {
  if (!value) return new ArrayBuffer(0)
  const padded = value + '='.repeat((4 - (value.length % 4)) % 4)
  const binary = atob(padded.replace(/-/g, '+').replace(/_/g, '/'))
  const buffer = new ArrayBuffer(binary.length)
  const bytes = new Uint8Array(buffer)
  for (let i = 0; i < binary.length; i += 1) bytes[i] = binary.charCodeAt(i)
  return buffer
}

/** ArrayBuffer → base64url 字符串(去 padding)。 */
export function arrayBufferToBase64Url(buffer?: ArrayBuffer | ArrayBufferLike | null): string {
  if (!buffer) return ''
  const bytes = new Uint8Array(buffer)
  let binary = ''
  for (let i = 0; i < bytes.byteLength; i += 1) binary += String.fromCharCode(bytes[i])
  return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/g, '')
}

// 服务端返回的选项是宽松 JSON(浏览器类型注解按结构收窄)。
type RawOptions = Record<string, any>

function unwrapOptions(payload: any, what: string): RawOptions {
  const options = payload?.publicKey ?? payload?.PublicKey ?? payload?.response ?? payload?.Response
  if (!options) throw new Error(`无法解析 Passkey ${what} 选项`)
  return options as RawOptions
}

/** 注册:服务端 PublicKeyCredentialCreationOptions → 浏览器可用的选项。 */
export function prepareCredentialCreationOptions(payload: any): PublicKeyCredentialCreationOptions {
  const options = unwrapOptions(payload, '注册')
  const publicKey = {
    ...options,
    challenge: base64UrlToArrayBuffer(options.challenge),
    user: { ...options.user, id: base64UrlToArrayBuffer(options.user?.id) },
  } as PublicKeyCredentialCreationOptions & Record<string, any>
  if (Array.isArray(options.excludeCredentials)) {
    publicKey.excludeCredentials = options.excludeCredentials.map(
      (item: RawOptions) =>
        ({ ...item, id: base64UrlToArrayBuffer(item.id) }) as PublicKeyCredentialDescriptor,
    )
  }
  // 空数组在某些浏览器实现上非法,删除交由默认值。
  if (Array.isArray(options.attestationFormats) && options.attestationFormats.length === 0) {
    delete publicKey.attestationFormats
  }
  return publicKey
}

/** 登录:服务端 PublicKeyCredentialRequestOptions → 浏览器可用的选项。 */
export function prepareCredentialRequestOptions(payload: any): PublicKeyCredentialRequestOptions {
  const options = unwrapOptions(payload, '登录')
  const publicKey = {
    ...options,
    challenge: base64UrlToArrayBuffer(options.challenge),
  } as PublicKeyCredentialRequestOptions & Record<string, any>
  if (Array.isArray(options.allowCredentials)) {
    publicKey.allowCredentials = options.allowCredentials.map(
      (item: RawOptions) =>
        ({ ...item, id: base64UrlToArrayBuffer(item.id) }) as PublicKeyCredentialDescriptor,
    )
  }
  return publicKey
}

/** 注册结果 → 提交 /finish 的 JSON(全部二进制字段 base64url 化)。 */
export function buildRegistrationResult(credential: PublicKeyCredential | null): Record<string, any> | null {
  if (!credential) return null
  const response = credential.response as AuthenticatorAttestationResponse & {
    getTransports?: () => string[]
  }
  return {
    id: credential.id,
    rawId: arrayBufferToBase64Url(credential.rawId),
    type: credential.type,
    authenticatorAttachment: credential.authenticatorAttachment,
    response: {
      attestationObject: arrayBufferToBase64Url(response.attestationObject),
      clientDataJSON: arrayBufferToBase64Url(response.clientDataJSON),
      transports: typeof response.getTransports === 'function' ? response.getTransports() : undefined,
    },
    clientExtensionResults: credential.getClientExtensionResults?.() ?? {},
  }
}

/** 登录断言结果 → 提交 /finish 的 JSON。 */
export function buildAssertionResult(credential: PublicKeyCredential | null): Record<string, any> | null {
  if (!credential) return null
  const response = credential.response as AuthenticatorAssertionResponse
  return {
    id: credential.id,
    rawId: arrayBufferToBase64Url(credential.rawId),
    type: credential.type,
    authenticatorAttachment: credential.authenticatorAttachment,
    response: {
      authenticatorData: arrayBufferToBase64Url(response.authenticatorData),
      clientDataJSON: arrayBufferToBase64Url(response.clientDataJSON),
      signature: arrayBufferToBase64Url(response.signature),
      userHandle: response.userHandle ? arrayBufferToBase64Url(response.userHandle) : null,
    },
    clientExtensionResults: credential.getClientExtensionResults?.() ?? {},
  }
}

/** 当前浏览器是否支持 Passkey(WebAuthn)。 */
export async function isPasskeySupported(): Promise<boolean> {
  if (typeof window === 'undefined' || !window.PublicKeyCredential) return false
  const { PublicKeyCredential: pk } = window
  try {
    if (typeof pk.isUserVerifyingPlatformAuthenticatorAvailable === 'function') {
      return await pk.isUserVerifyingPlatformAuthenticatorAvailable()
    }
  } catch {
    return false
  }
  return true
}

/** 发起注册(浏览器弹窗)。 */
export function createCredential(options: PublicKeyCredentialCreationOptions): Promise<Credential | null> {
  return navigator.credentials.create({ publicKey: options })
}

/** 发起登录断言(浏览器弹窗)。 */
export function getCredential(options: PublicKeyCredentialRequestOptions): Promise<Credential | null> {
  return navigator.credentials.get({ publicKey: options })
}
