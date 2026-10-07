// Browser storage is optional for workspace state. Saved items must check writes
// before claiming persistence: private browsing and quota limits can reject them.
export function readStorage(key: string): string | null {
  try { return localStorage.getItem(key); } catch { return null; }
}
export function writeStorage(key: string, value: string): boolean {
  try { localStorage.setItem(key, value); return true; } catch { return false; }
}
export function removeStorage(key: string): boolean {
  try { localStorage.removeItem(key); return true; } catch { return false; }
}
