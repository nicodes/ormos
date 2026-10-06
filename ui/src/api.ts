export type TerminalRow = { id: string; alive: boolean; cwd: string };
export class APIError extends Error {
  status: number;
  constructor(message: string, status: number) { super(message); this.status = status; }
}
export function apiURL(path: string, workspace = new URL(location.href)): URL {
  const base = (workspace.pathname === "/ormos" || workspace.pathname.startsWith("/ormos/")) ? "/ormos/" : "/";
  return new URL(base + path.replace(/^\//, ""), workspace.origin);
}
export async function request<T>(path: string, body?: object, signal?: AbortSignal): Promise<T> {
  const response = await fetch(apiURL(path), body ? {
    signal, method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body),
  } : signal ? { signal } : undefined);
  const data = await response.json();
  if (!response.ok) throw new APIError(data.error ?? `Request failed (${response.status})`, response.status);
  return data as T;
}
