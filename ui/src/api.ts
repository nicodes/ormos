export type TerminalRow = { id: string; alive: boolean; cwd: string };
export class APIError extends Error {
  constructor(message: string, public status: number) { super(message); }
}
export async function request<T>(path: string, body?: object): Promise<T> {
  const response = await fetch(path, body ? {
    method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body),
  } : undefined);
  const data = await response.json();
  if (!response.ok) throw new APIError(data.error ?? `Request failed (${response.status})`, response.status);
  return data as T;
}
