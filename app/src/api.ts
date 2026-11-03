export type TerminalRow = { id: string; alive: boolean; cwd: string };
export class APIError extends Error {
  status: number;
  readonly code: string;
  readonly requestId: string;
  constructor(message: string, status: number, code = "request_failed", requestId = "") {
    super(message);
    this.status = status;
    this.code = code;
    this.requestId = requestId;
  }
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
    if (!response.ok) {
      const problem = data?.error;
      const message = typeof problem?.message === "string" ? problem.message : typeof problem === "string" ? problem : `Request failed (${response.status})`;
      throw new APIError(message.slice(0,1024), response.status,
        typeof problem?.code === "string" ? problem.code.slice(0,64) : "request_failed",
        response.headers?.get("X-Request-ID") || (typeof problem?.request_id === "string" ? problem.request_id.slice(0,128) : ""));
    }
    return data as T;
  } finally {
    clearTimeout(timer);
    signal?.removeEventListener("abort", abort);
  }
}
