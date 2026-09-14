# Cap with Parakeet

Cap can use its existing upstream image with a Node preload module. Mount
`runtime/preload.cjs` read-only and set:

```text
NODE_OPTIONS=--require=/opt/parakeet/preload.cjs
ASSEMBLYAI_BASE_URL=http://parakeet-api.parakeet.svc.cluster.local:8080
ASSEMBLY_API_KEY=<Parakeet client key from a Secret>
```

The module intercepts only the US/EU AssemblyAI HTTPS origins. Uploads, job
submission, polling, deletion and subtitle requests go to the configured
internal origin. Audio uploads remain streams. Other HTTP requests, including
Cap storage, authentication and AI summaries, use the original fetch unchanged.
Responses and word timestamps are returned unchanged.

Job submission omits AssemblyAI-specific model selection, formatting and
language-detection controls. Parakeet supplies its model's transcription and
punctuation; it does not implement AssemblyAI's disfluency or forced-language
controls. Only an optional supported language label is forwarded. Unknown
features fail before a network request. The module refuses a missing client key
or a cloud AssemblyAI target, and redirects from the internal API are disabled.

`runtime/kustomization.yaml` generates the ConfigMap and a content hash. An
infrastructure overlay can reference this directory at an immutable Git commit,
mount `cap-parakeet-preload` and pin the Cap image digest. No application source
or build step belongs in the infrastructure repository. Cap Pods do not fetch
code at startup; Flux prepares the ConfigMap before starting them.

The tested upstream image is:

```text
ghcr.io/capsoftware/cap-web@sha256:8ee4cbd3fd87f88f538831aed06c954c525db9c2426a62abeaf0ca307c5e1ce9
```

Do not automatically advance the Cap image. Retest the module when changing
Cap or its SDK, because this is a runtime adaptation rather than a supported
Cap configuration flag. Six Node tests cover request conversion, upload bytes,
result routes, unrelated requests and rejected configurations/features:

```sh
node --test integrations/cap/runtime/preload.test.cjs
```

The original compiled transcription step in the pinned image was exercised
against a real CPU worker with a 71.723-second private recording. It returned
127 words in 19.702 seconds, preserving all decoder word timestamps through
Cap's editable transcript and VTT output. No image files were patched. Test
jobs were deleted and private recordings/transcripts are not committed.

`probe/` contains a synthetic speech fixture and a check that executes the
original compiled Cap transcription and backfill steps, compares their word
timestamps with the API response, and deletes its jobs. The fixture says:
"This is an internal transcription test. Our own server returns words and
accurate timestamps." It was generated with espeak-ng and encoded as AAC.
The check needs the pinned Cap image, the preload module, `ASSEMBLYAI_BASE_URL`
and `ASSEMBLY_API_KEY`; it must not be exposed as an HTTP endpoint.

A [source patch](source-patch.md) is available as an alternative for a future
Cap build or upstream contribution. It is not needed for this deployment.

## License

The independently written runtime module is EUPL-1.2, like the Parakeet API.
Copyright 2026 Adapt2Move GmbH. Cap and the optional source patch retain their
AGPLv3 license and Cap Software, Inc. attribution in [LICENSE](LICENSE).
