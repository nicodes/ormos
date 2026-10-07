export type PreviewTarget = { port: number; path: string };

const localAliases = new Set(["localhost", "127.0.0.1", "[::1]"]);

export function previewURL(target: PreviewTarget, workspace: URL): string {
  if (!Number.isInteger(target.port) || target.port < 1 || target.port > 65535) throw new Error("Enter a port between 1 and 65535.");
  const origin = new URL(workspace.origin);
  origin.port = String(target.port);
  if (origin.origin === workspace.origin) throw new Error("Choose an app port, not the Ormos port.");
  if (!target.path.startsWith("/") || target.path.startsWith("//") || target.path.includes("\\")) throw new Error("Use a local app path.");
  const url = new URL(target.path, origin);
  if (url.origin !== origin.origin) throw new Error("Use a local app path.");
  return url.href;
}

export function parsePreviewAddress(raw: string, workspace: URL, currentPort?: number): PreviewTarget {
  const value = raw.trim();
  let port: number;
  let path: string;
  const short = /^:?(\d{1,5})([/?#].*)?$/.exec(value);
  if (short) { port = Number(short[1]); path = short[2] ?? "/"; }
  else if (/^https?:\/\//i.test(value)) {
    if (value.includes("\\")) throw new Error("Use a local app address.");
    const url = new URL(value);
    if ((!localAliases.has(url.hostname) && url.hostname !== workspace.hostname) || url.username || url.password) throw new Error("Use this machine's app port or a localhost URL.");
    if (url.protocol !== "http:" && url.protocol !== workspace.protocol) throw new Error("Use a local HTTP app URL.");
    port = Number(url.port || (url.protocol === "https:" ? 443 : 80));
    path = url.pathname + url.search + url.hash;
  } else if (currentPort && value) { port = currentPort; path = value.startsWith("/") ? value : `/${value}`; }
  else throw new Error("Enter a local app port or localhost URL.");
  if (!path.startsWith("/")) path = `/${path}`;
  const url = new URL(previewURL({ port, path }, workspace));
  return { port, path: url.pathname + url.search + url.hash };
}
