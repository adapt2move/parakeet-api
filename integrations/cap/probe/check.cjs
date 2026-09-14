// SPDX-License-Identifier: EUPL-1.2
const fs = require("node:fs");
const assert = require("node:assert/strict");
const jobs = new Set();
let raw;
const transport = globalThis.fetch;
globalThis.fetch = async (input, init) => {
  const response = await transport(input, init);
  const url = new URL(input instanceof Request ? input.url : input);
  if (url.pathname.startsWith("/v2/transcript") && response.ok) {
    const result = await response.clone().json();
    if (result.id) jobs.add(result.id);
    if (result.status === "completed" && result.words) raw = result;
  }
  return response;
};

const root = "/app/apps/web/.next/server";
require(`${root}/app/.well-known/workflow/v1/step/route.js`);
// This module ID belongs to the pinned Cap image. Updating Cap requires retesting.
require(`${root}/chunks/[turbopack]_runtime.js`)("server/app/.well-known/workflow/v1/step/route.js").m(864328);
const steps = globalThis[Symbol.for("@workflow/core//registeredSteps")];
assert(steps?.size > 0, "The pinned Cap workflow module did not register its steps");

async function main() {
  try {
    const data = fs.readFileSync(process.argv[2] || `${__dirname}/speech.m4a`);
    const url = `data:audio/mp4;base64,${data.toString("base64")}`;
    for (const name of ["transcribeWithAssemblyAI", "transcribeEditTranscriptWithAssemblyAI"]) {
      const start = Date.now();
      const step = steps.get(`step//./workflows/transcribe//${name}`);
      assert.equal(typeof step, "function");
      const result = name === "transcribeWithAssemblyAI" ? await step(url, "auto", 0) : await step(url, 0);
      const edited = JSON.parse(typeof result === "string" ? result : result.editTranscript);
      assert(edited.words.length > 5);
      assert.equal(edited.words.length, raw.words.length);
      for (let i = 0; i < edited.words.length; i++) {
        assert.equal(edited.words[i].startMs, raw.words[i].start);
        assert.equal(edited.words[i].endMs, raw.words[i].end);
        assert(edited.words[i].startMs >= 0 && edited.words[i].endMs <= raw.audio_duration * 1000);
      }
      if (typeof result !== "string") assert(result.vtt.startsWith("WEBVTT"));
      console.log(JSON.stringify({check: name, words: edited.words.length,
        elapsed_seconds: (Date.now() - start) / 1000, timestamps_preserved: true}));
    }
  } finally {
    for (const id of jobs) {
      const url = `${process.env.ASSEMBLYAI_BASE_URL}/v2/transcript/${id}`;
      const headers = {authorization: process.env.ASSEMBLY_API_KEY};
      assert.equal((await fetch(url, {method: "DELETE", headers})).status, 200);
      assert.equal((await fetch(url, {headers})).status, 404);
    }
  }
}
main().then(() => process.exit(0), () => {
  console.error("Cap image transcription check failed");
  process.exit(1);
});
