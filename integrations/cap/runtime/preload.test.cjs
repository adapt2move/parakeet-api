const { test } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");
const source = fs.readFileSync(`${__dirname}/preload.cjs`, "utf8");
function load(overrides = {}) {
  const calls = [];
  const context = {
    URL, Request, Headers,
    process: { env: { ASSEMBLYAI_BASE_URL: "http://parakeet:8080", ASSEMBLY_API_KEY: "k".repeat(40), ...overrides } },
    console: { info() {} },
    fetch: async (...args) => { calls.push(args); return new Response("{}", {status: 200}); },
  };
  vm.runInNewContext(source, context);
  return { fetch: context.fetch, calls };
}
test("Cap options become a Parakeet request with its own credential", async () => {
  const { fetch, calls } = load();
  await fetch("https://api.assemblyai.com/v2/transcript", {
    method: "POST", headers: { authorization: "old-key", "content-length": "999", host: "api.assemblyai.com" },
    body: JSON.stringify({ audio_url: "http://parakeet:8080/uploads/test", language_code: "de",
      speech_models: ["universal-3-5-pro"], format_text: true, punctuate: true, disfluencies: true,
      language_detection: true, language_detection_options: { expected_languages: ["de"] } }),
  });
  const [url, init] = calls[0];
  assert.equal(url.href, "http://parakeet:8080/v2/transcript");
  assert.deepEqual(JSON.parse(init.body), {audio_url: "http://parakeet:8080/uploads/test", language_code: "de"});
  assert.equal(init.headers.get("authorization"), "k".repeat(40));
  assert.equal(init.headers.has("content-length"), false);
  assert.equal(init.headers.has("host"), false);
  assert.equal(init.redirect, "error");
});
test("Request uploads retain bytes without reading the audio into the adapter", async () => {
  const { fetch, calls } = load();
  const bytes = new Uint8Array([0, 255, 17, 128]);
  await fetch(new Request("https://api.eu.assemblyai.com/v2/upload", { method: "POST", body: bytes }));
  assert.deepEqual(new Uint8Array(await new Response(calls[0][1].body).arrayBuffer()), bytes);
  assert.equal(calls[0][0].href, "http://parakeet:8080/v2/upload");
});
test("poll, subtitle and deletion routes preserve method and query", async () => {
  for (const [method, path] of [["GET", "/v2/transcript/abc-123"], ["GET", "/v2/transcript/abc-123/vtt?chars_per_caption=40"], ["DELETE", "/v2/transcript/abc-123"]]) {
    const { fetch, calls } = load();
    await fetch(new URL(`https://api.assemblyai.com${path}`), { method });
    assert.equal(calls[0][0].href, `http://parakeet:8080${path}`);
    assert.equal(calls[0][1].method, method);
  }
});
test("other Cap requests are passed through unchanged", async () => {
  const { fetch, calls } = load();
  for (const url of ["https://storage.example/audio", "https://api.assemblyai.com.attacker.invalid/v2/upload", "data:text/plain,test"]) {
    const input = new Request(url);
    const init = { cache: "no-store" };
    await fetch(input, init);
    assert.equal(calls.at(-1)[0], input);
    assert.equal(calls.at(-1)[1], init);
  }
});
test("unknown features and routes fail before any network request", async () => {
  const { fetch, calls } = load();
  await assert.rejects(fetch("https://api.assemblyai.com/v2/transcript", {
    method: "POST", body: JSON.stringify({audio_url: "test", speaker_labels: true}),
  }), /Unsupported AssemblyAI option/);
  await assert.rejects(fetch("https://api.assemblyai.com/v2/realtime/token", { method: "POST" }), /Unsupported AssemblyAI route/);
  assert.equal(calls.length, 0);
});
test("missing keys and unsafe target configurations prevent startup", () => {
  for (const overrides of [
    {ASSEMBLY_API_KEY: ""}, {ASSEMBLYAI_BASE_URL: ""},
    {ASSEMBLYAI_BASE_URL: "https://api.assemblyai.com"},
    {ASSEMBLYAI_BASE_URL: "http://api.assemblyai.com"},
    {ASSEMBLYAI_BASE_URL: "ftp://parakeet"},
    {ASSEMBLYAI_BASE_URL: "http://user:password@parakeet"},
    {ASSEMBLYAI_BASE_URL: "http://parakeet/v2"},
  ]) assert.throws(() => load(overrides));
});
