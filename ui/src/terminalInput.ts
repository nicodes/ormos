// Keep each JSON input message bounded without cutting a UTF-16 surrogate pair
// in half (which would turn an emoji into two replacement characters in Go).
export function* terminalInputChunks(data: string): Generator<string> {
  for (let start = 0; start < data.length;) {
    let end = Math.min(start + 2048, data.length);
    const last = data.charCodeAt(end - 1), next = data.charCodeAt(end);
    if (last >= 0xd800 && last <= 0xdbff && next >= 0xdc00 && next <= 0xdfff) end--;
    yield data.slice(start, end);
    start = end;
  }
}
