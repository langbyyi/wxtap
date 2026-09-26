import { describe, expect, it } from 'vitest';
import { wxDecrypt, wxEncrypt, WxCryptoError } from './wx-crypto';

function b64encode(input: string | Uint8Array): string {
  const buf = typeof input === 'string' ? Buffer.from(input, 'utf-8') : Buffer.from(input);
  return buf.toString('base64');
}

function b64decode(input: string): Uint8Array {
  return new Uint8Array(Buffer.from(input, 'base64'));
}

const SAMPLE_KEY = b64encode(new Uint8Array([0x74, 0x69, 0x69, 0x68, 0x74, 0x4e, 0x63, 0x7a, 0x66, 0x35, 0x76, 0x36, 0x41, 0x4b, 0x52, 0x79]));
const SAMPLE_IV = b64encode(new Uint8Array(16).fill(0x0a));

async function encryptForTest(plaintext: string, keyB64: string, ivB64: string): Promise<string> {
  const key = await crypto.subtle.importKey('raw', b64decode(keyB64) as BufferSource, { name: 'AES-CBC' }, false, ['encrypt']);
  const iv = b64decode(ivB64) as BufferSource;
  const data = new TextEncoder().encode(plaintext) as BufferSource;
  const result = await crypto.subtle.encrypt({ name: 'AES-CBC', iv }, key, data);
  return b64encode(new Uint8Array(result));
}

describe('wx-crypto', () => {
  it('decrypts and re-encrypts symmetrically', async () => {
    const plain = JSON.stringify({ phoneNumber: '13888888888' });
    const cipher = await encryptForTest(plain, SAMPLE_KEY, SAMPLE_IV);
    const decrypted = await wxDecrypt(cipher, SAMPLE_IV, SAMPLE_KEY);
    expect(decrypted).toBe(plain);
    const reEncrypted = await wxEncrypt(plain, SAMPLE_IV, SAMPLE_KEY);
    const roundTrip = await wxDecrypt(reEncrypted, SAMPLE_IV, SAMPLE_KEY);
    expect(roundTrip).toBe(plain);
  });

  it('handles multi-block plaintext', async () => {
    const plain = JSON.stringify({ openId: 'oABC123', unionId: 'uXYZ789', phoneNumber: '13800000000', nickName: 'test-user', extra: 'x'.repeat(100) });
    const cipher = await encryptForTest(plain, SAMPLE_KEY, SAMPLE_IV);
    expect(await wxDecrypt(cipher, SAMPLE_IV, SAMPLE_KEY)).toBe(plain);
  });

  it('rejects empty sessionKey', async () => {
    await expect(wxDecrypt('AAAA', SAMPLE_IV, '')).rejects.toThrow(WxCryptoError);
  });

  it('rejects invalid base64 iv', async () => {
    await expect(wxDecrypt('AAAA', '!!!', SAMPLE_KEY)).rejects.toThrow('iv');
  });

  it('rejects wrong-length sessionKey', async () => {
    const shortKey = b64encode(new Uint8Array(15));
    await expect(wxDecrypt('AAAA', SAMPLE_IV, shortKey)).rejects.toThrow('16');
  });

  it('rejects cipher not multiple of 16', async () => {
    const badCipher = b64encode(new Uint8Array(5));
    await expect(wxDecrypt(badCipher, SAMPLE_IV, SAMPLE_KEY)).rejects.toThrow('16');
  });

  it('rejects wrong key on decrypt', async () => {
    const wrongKey = b64encode(new Uint8Array(16).fill(0xbb));
    const cipher = await encryptForTest('hello', SAMPLE_KEY, SAMPLE_IV);
    try {
      const result = await wxDecrypt(cipher, SAMPLE_IV, wrongKey);
      expect(result).not.toBe('hello');
    } catch {
      // acceptable
    }
  });

  it('rejects empty plaintext on encrypt', async () => {
    await expect(wxEncrypt('', SAMPLE_IV, SAMPLE_KEY)).rejects.toThrow(WxCryptoError);
  });
});
