export type TerminalRow = { id: string; alive: boolean; cwd: string };
export class APIError extends Error {
  status: number;
  constructor(message: string, status: number) { super(message); this.status = status; }
}
export async function request<T>(path: string, body?: object, signal?: AbortSignal): Promise<T> {
  const controller = new AbortController();
  const abort = () => controller.abort(signal?.reason);
  if (signal?.aborted) abort();
  else signal?.addEventListener("abort", abort, { once: true });
  const timer = setTimeout(() => controller.abort(new Error("Request timed out. Check your connection and try again.")), 15000);
  try {
    const response = await fetch(path, body ? {
      signal: controller.signal, method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body),
    } : { signal: controller.signal });
    const data = await response.json();
    if (!response.ok) throw new APIError(data.error ?? `Request failed (${response.status})`, response.status);
    return data as T;
  } finally {
    clearTimeout(timer);
    signal?.removeEventListener("abort", abort);
  }
}
