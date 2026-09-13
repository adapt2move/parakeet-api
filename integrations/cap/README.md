# Cap integration — prepared, not deployed

The patch adds a configurable AssemblyAI endpoint to Cap and an explicit
`parakeet` transcription profile. It covers full recordings, editable-transcript
backfills and live recording chunks. The default AssemblyAI behavior is
preserved when the profile is not set.

Apply it to [CapSoftware/Cap at the pinned revision](https://github.com/CapSoftware/Cap/tree/5786c3d6d64e1b63ee7012fe6e571480a8e73dd7)
in `UPSTREAM_REVISION`. It changes source files, not generated Next.js bundles.
The currently published upstream Cap image does not support these settings.

| Variable | Value for Parakeet |
| --- | --- |
| `ASSEMBLYAI_BASE_URL` | Internal API origin, without `/v2` |
| `ASSEMBLYAI_TRANSCRIPTION_PROFILE` | `parakeet` |
| `ASSEMBLY_API_KEY` | Parakeet client key |

The Parakeet profile sends only the audio and an optional language label. It
omits AssemblyAI-specific model selection, language detection options and
formatting/disfluency controls. Parakeet performs multilingual decoding without
forced-language decoding or returning a detected language. Cap's broader
language picker remains unchanged; explicit languages must be supported by the
Parakeet model. The profile refuses to create a client without an explicit base
URL, preventing accidental use of the cloud default.

Build preparation, from the root of this repository:

```sh
cap_source="$(mktemp -d)"
git clone --filter=blob:none https://github.com/CapSoftware/Cap.git "$cap_source"
git -C "$cap_source" checkout "$(cat integrations/cap/UPSTREAM_REVISION)"
git -C "$cap_source" apply --check "$PWD/integrations/cap/assemblyai-endpoint.patch"
git -C "$cap_source" apply "$PWD/integrations/cap/assemblyai-endpoint.patch"
docker build -f "$cap_source/apps/web/Dockerfile" -t cap-web:parakeet-candidate "$cap_source"
```

The patch and its reverse were checked against the pinned source. Nine scoped
tests cover the existing cloud options, Parakeet requests, endpoint selection
and rejection of a missing endpoint. The modified helper, environment schema
and tests passed a scoped TypeScript check. Biome passed with one pre-existing
unused-variable warning in the live workflow. The complete Cap container and
workflow/UI tests have not been run; complete those before deploying a build.

The real AssemblyAI JavaScript SDK 4.36.4, the patched client/options helper and
Cap's existing edit-transcript/VTT conversion functions were tested against a
running CPU Parakeet deployment on 2026-09-13:

| Input | Result | Total request time |
| --- | --- | --- |
| 286.891s recording | 502 words, timestamps preserved through Cap conversion | 80.85s |
| 10.006s middle fMP4 fragment with initialization segment | 22 words, relative timestamps and valid VTT | 3.82s |

The long recording used three decoder windows. The fragment test exercises the
finite HTTP-upload format used by Cap's live workflow; it does not add an
AssemblyAI streaming WebSocket endpoint. Test jobs were deleted after checking,
and private recordings/transcripts are not included here.

## License

Cap and `assemblyai-endpoint.patch` are covered by the accompanying AGPLv3
[LICENSE](LICENSE), including Cap Software, Inc.'s copyright notice. Patch
contributions: Copyright 2026 Adapt2Move GmbH. This directory does not change
the EUPL-1.2 license of the Parakeet API application.
