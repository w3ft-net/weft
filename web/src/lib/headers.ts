// A deployment adds headers to every request the page makes, whether through
// api.ts or through the wasm shell (hosted pins the active org with one). Core
// sends none of its own beyond the session token and the client id.
type HeaderSource = () => Record<string, string>

const sources: HeaderSource[] = []

export function addRequestHeaders(source: HeaderSource): void {
  sources.push(source)
}

export function extraRequestHeaders(): Record<string, string> {
  return Object.assign({}, ...sources.map(s => s()))
}
