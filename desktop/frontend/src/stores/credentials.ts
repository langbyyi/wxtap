import { defineStore } from 'pinia';
import { computed, ref } from 'vue';

export type CredentialMode = 'oa' | 'mini' | 'work';

// Credentials shared by the 「利用」 pages: verifying an AK and calling an
// official endpoint need the same AppID/AppSecret, so the user types it once
// per session instead of once per page.
//
// Nothing here is persisted — no config entry, no localStorage, no database
// write. The secret lives in this store (and therefore in the webview's memory)
// only, which is why the AK page can promise that credentials are never
// written to disk.
export const useCredentialStore = defineStore('credentials', () => {
  const mode = ref<CredentialMode>('mini');
  const accessKey = ref('');
  const secretKey = ref('');

  const complete = computed(() => accessKey.value.trim() !== '' && secretKey.value.trim() !== '');

  function set(next: { mode: CredentialMode; accessKey: string; secretKey: string }) {
    mode.value = next.mode;
    accessKey.value = next.accessKey;
    secretKey.value = next.secretKey;
  }

  function clear() {
    accessKey.value = '';
    secretKey.value = '';
  }

  return { mode, accessKey, secretKey, complete, set, clear };
});
