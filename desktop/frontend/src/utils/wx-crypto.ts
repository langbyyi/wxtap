function base64ToBytes(base64: string): Uint8Array {
  const binary = atob(base64);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
  return bytes;
}

function bytesToBase64(bytes: Uint8Array): string {
  let binary = '';
  for (let i = 0; i < bytes.length; i++) binary += String.fromCharCode(bytes[i]);
  return btoa(binary);
}

export class WxCryptoError extends Error {}

function decodeInput(label: string, value: string, expectedLen?: number): Uint8Array {
  if (!value.trim()) throw new WxCryptoError(`${label}不能为空`);
  let bytes: Uint8Array;
  try {
    bytes = base64ToBytes(value.trim());
  } catch {
    throw new WxCryptoError(`${label}不是有效的 Base64 编码`);
  }
  if (bytes.length === 0) throw new WxCryptoError(`${label}解码后为空`);
  if (expectedLen && bytes.length !== expectedLen) {
    throw new WxCryptoError(`${label}解码后应为 ${expectedLen} 字节，实际为 ${bytes.length} 字节`);
  }
  return bytes;
}

export async function wxDecrypt(encryptedDataB64: string, ivB64: string, sessionKeyB64: string): Promise<string> {
  const cipher = decodeInput('encryptedData', encryptedDataB64);
  const iv = decodeInput('iv', ivB64, 16);
  const key = decodeInput('sessionKey', sessionKeyB64, 16);
  if (cipher.length % 16 !== 0) throw new WxCryptoError(`encryptedData 解码后长度 (${cipher.length}) 不是 16 的倍数`);
  const cryptoKey = await crypto.subtle.importKey('raw', key as BufferSource, { name: 'AES-CBC' }, false, ['decrypt']);
  let plain: ArrayBuffer;
  try {
    plain = await crypto.subtle.decrypt({ name: 'AES-CBC', iv: iv as BufferSource }, cryptoKey, cipher as BufferSource);
  } catch {
    throw new WxCryptoError('解密失败：sessionKey 或 iv 与 encryptedData 不匹配');
  }
  return new TextDecoder().decode(plain);
}

export async function wxEncrypt(plaintext: string, ivB64: string, sessionKeyB64: string): Promise<string> {
  if (!plaintext.trim()) throw new WxCryptoError('待加密内容不能为空');
  const iv = decodeInput('iv', ivB64, 16);
  const key = decodeInput('sessionKey', sessionKeyB64, 16);
  const plainBytes = new TextEncoder().encode(plaintext);
  const cryptoKey = await crypto.subtle.importKey('raw', key as BufferSource, { name: 'AES-CBC' }, false, ['encrypt']);
  const cipher = await crypto.subtle.encrypt({ name: 'AES-CBC', iv: iv as BufferSource }, cryptoKey, plainBytes as BufferSource);
  return bytesToBase64(new Uint8Array(cipher));
}
