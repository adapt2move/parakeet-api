// SPDX-License-Identifier: EUPL-1.2
const target = new URL(process.env.ASSEMBLYAI_BASE_URL);
const key = process.env.ASSEMBLY_API_KEY;
const cloudOrigins = new Set(["https://api.assemblyai.com", "https://api.eu.assemblyai.com"]);
if (
  !["http:", "https:"].includes(target.protocol) ||
  target.username || target.password || target.search || target.hash ||
  target.pathname !== "/" || ["api.assemblyai.com", "api.eu.assemblyai.com"].includes(target.hostname) || !key || key.length < 32
) throw new Error("Cap Parakeet preload requires an internal API origin and client key");

const ignoredOptions = new Set([
  "speech_models", "speech_model", "format_text", "punctuate", "disfluencies",
  "language_detection", "language_detection_options",
]);
const originalFetch = globalThis.fetch;
globalThis.fetch = async function(input, init) {
  const url = new URL(input?.url ?? input);
  if (!cloudOrigins.has(url.origin)) return originalFetch(input, init);
  const request = new Request(input, init);
  const method = request.method;
  const upload = url.pathname === "/v2/upload" && method === "POST";
  const submit = url.pathname === "/v2/transcript" && method === "POST";
  const result = /^\/v2\/transcript\/[a-zA-Z0-9-]+(?:\/(?:srt|vtt))?$/.test(url.pathname)
    && ["GET", "DELETE"].includes(method);
  if (!upload && !submit && !result) throw new Error("Unsupported AssemblyAI route in Parakeet profile");
  const headers = new Headers(request.headers);
  headers.set("authorization", key);
  headers.delete("host");
  let body = request.body;
  if (submit) {
    const options = JSON.parse(await request.text());
    for (const name of Object.keys(options)) {
      if (ignoredOptions.has(name)) delete options[name];
      else if (!["audio_url", "language_code"].includes(name)) {
        throw new Error(`Unsupported AssemblyAI option in Parakeet profile: ${name}`);
      }
    }
    body = JSON.stringify(options);
    headers.delete("content-length");
    headers.set("content-type", "application/json");
  }
  const response = await originalFetch(new URL(url.pathname + url.search, target), {
    method, headers, body, signal: request.signal, duplex: "half",
    redirect: "error", cache: "no-store",
  });
  if (upload || submit) console.info(JSON.stringify({
    event: "cap_parakeet_request", operation: upload ? "upload" : "submit", status: response.status,
  }));
  return response;
};
console.info(JSON.stringify({event: "cap_parakeet_preload_ready"}));
