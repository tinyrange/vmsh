# OCI layer delta experiment

This experiment tests whether a large OCI layer can remain a normal gzip-compressed
tar blob while also being reconstructable from chunks already present in an older
layer.

The proposed wire shape is a deterministic tar stream split into independently
compressed, concatenated gzip members. Concatenated members are valid gzip, so the
result can use the ordinary OCI layer media type and be stored by an unmodified
registry. A small index maps target members to their hashes, offsets, and lengths.
An updater can copy matching members from locally cached old layers and fetch only
missing byte ranges from the target blob.

The standalone measurement remains an investigation prototype. Its Gear-hash content
defined chunker is deliberately small and should be replaced by a proven FastCDC
implementation. The file-aware measurement synthesizes canonical metadata records;
it measures reuse after deterministic repacking rather than reproducing the digest
of today's conventionally-gzipped input layer.

## NeurodeskAppX implementation

The first application-integrated experiment now uses standard eStargz layers. The
blob is still an ordinary OCI gzip layer and can be pushed, pulled, mirrored, and
garbage-collected by an unmodified registry. A `vmshDelta` extension in the eStargz
TOC records the SHA-256 digest, compressed offset, and compressed length of each
member. Other eStargz implementations ignore that additional JSON field.

NeurodeskAppX opts into three related behaviors; SquadVM does not yet opt in:

- eStargz blobs remain compressed in the local blob cache and file reads are served
  from the compressed chunks, instead of expanding the complete layer beside it;
- when an update is advertised, it is pulled into a separate staged image while the
  existing VM continues to run;
- for enhanced eStargz layers, the downloader copies matching compressed members
  from any older local blob and requests only coalesced missing byte ranges from the
  target registry blob. It verifies the completed blob against the OCI descriptor
  digest before making it available.

Every path has a compatibility fallback. A conventional gzip layer uses the old
expanded-layer cache, an eStargz layer without `vmshDelta` downloads in full, and a
registry that ignores Range requests also downloads in full.

To produce one enhanced layer for testing:

```sh
cd cc
go run ./cmd/estargz-repack \
  --chunk-size 262144 \
  --min-chunk-size 262144 \
  input-layer.tar.gz output-layer.tar.gz
```

The command prints the new blob digest, compressed size, uncompressed DiffID, TOC
digest, and member count. An image manifest using this layer must be updated with
the printed blob digest and size, while its config must use the printed DiffID.
That manifest rewrite is now handled by the publishing workflow through
`cc/cmd/estargz-repack-image`, which performs
that conversion over a single-platform OCI image layout while preserving the
conventional input layout.

The Neurodesktop and SquadVM publishing workflows use parallel tags so existing
consumers do not change format unexpectedly:

- `<tag>`, `<tag>-amd64`, and `<tag>-arm64` remain conventional images;
- `<tag>-estargz`, `<tag>-estargz-amd64`, and `<tag>-estargz-arm64` contain the
  enhanced layers;
- publishing moving tags updates both `latest` and `latest-estargz`.

The workflow pulls each just-built conventional architecture image as an OCI
layout, converts it locally, pushes the enhanced layout with Crane, validates the
remote image, and finally publishes both multi-platform manifests. SquadVM has the
same publishing logic but remains opt-out at runtime and should not be dispatched
until its rollout is explicitly requested.

## Run

Synthetic mutations:

```sh
go run ./experiments/oci-layer-delta
```

Two real gzip-compressed tar layers:

```sh
go run ./experiments/oci-layer-delta old-layer.tar.gz new-layer.tar.gz
```

The synthetic path verifies that concatenated gzip members decompress into a valid
tar stream and that reconstruction produces the exact target blob.

## Results

The synthetic input was a 128 MiB layer. Selected results:

| Mutation | Strategy | Target download |
|---|---:|---:|
| 2 MiB insertion near the start | fixed chunks | 100% |
| 2 MiB insertion near the start | CDC, nominal 256 KiB | 5.26% |
| 64 KiB in-place edit | fixed 256 KiB | 0.14% |
| 64 KiB in-place edit | CDC, nominal 256 KiB | 0.73% |
| package-like add/remove/edit | fixed chunks | 100% |
| package-like add/remove/edit | CDC, nominal 256 KiB | 14.62% |
| all tar mtimes changed | CDC, nominal 256 KiB | 87.30% |

The metadata-churn case is the warning: reproducible tar headers, stable ordering,
and deterministic gzip headers are requirements, not optional optimizations.

For a real changed 245.4 MiB compressed SquadVM layer, comparing the old and new
contents after deterministic repacking produced:

| Strategy | Repacked size | Missing bytes | Missing % | Index estimate |
|---|---:|---:|---:|---:|
| fixed 256 KiB over the whole tar stream | 247.13 MiB | 61.72 MiB | 24.97% | 149 KiB |
| CDC, nominal 256 KiB over the whole tar stream | 246.24 MiB | 63.77 MiB | 25.90% | 71 KiB |
| file-aware fixed 256 KiB | 269.22 MiB | 11.92 MiB | 4.43% | 4.7 MiB |
| file-aware CDC, nominal 256 KiB | 268.83 MiB | 12.65 MiB | 4.70% | 4.6 MiB |

The file-aware experiment created a member for every metadata record, which explains
both its roughly 9.5% compression overhead and large index. A practical writer
should group metadata and small files into minimum-sized members, while applying CDC
inside large file payloads. It should also coalesce adjacent missing ranges before
issuing registry requests.

## Implemented runtime shape

The integrated experiment builds on the eStargz layout rather than defining a
wholly new layer format:

- emit a canonical tar stream with stable order, ownership, modes, timestamps, and
  gzip headers;
- group metadata and small files into members of roughly 256 KiB;
- split large file payloads into fixed-size chunks, resetting at file boundaries;
- place the TOC and eStargz footer at the end of the same OCI blob;
- include enough member digest and length data to reproduce and verify the target
  compressed blob exactly;
- reconstruct a changed layer by copying local compressed member ranges and using
  HTTP Range requests for missing ranges in the target registry blob;
- verify the completed blob against the target OCI descriptor digest before use.

The registry stores only the normal target layer. It does not need pairwise patch
objects or a server-side chunk service. A registry without byte-range support falls
back to downloading the complete layer.

FastCDC within large files is intentionally deferred. eStargz's stable file
boundaries already retain unchanged files when a large single layer is rebuilt;
real Neurodesk releases should be measured before accepting the extra producer and
runtime complexity of content-defined boundaries.

## Real Neurodesktop validation

On 2026-08-05, the demo build
`ghcr.io/tinyrange/neurodesktop-glass:estargz-demo-20260805-v2` produced a
2.67 GB compressed arm64 image. Its three largest individual layers were 783.6 MB,
521.3 MB, and 408.2 MB.

The largest 783,635,895-byte layer was downloaded and verified against its OCI
descriptor, then repacked with 256 KiB chunks:

- enhanced size: 787,429,125 bytes (0.48% overhead);
- indexed compressed members: 2,530;
- repack time: 72.25 seconds on an arm64 development host;
- maximum resident set reported by `/usr/bin/time`: about 225 MB;
- a runtime read crossing a chunk boundary matched the upstream standard eStargz
  reader without creating expanded layer contents.

After adding one small file and repacking again, the exact delta reconstruction
downloaded 5,738,502 bytes to reproduce the 787,429,703-byte target: 0.7288% of the
target blob. GHCR independently returned `206 Partial Content` and the exact
`Content-Range` for a bounded blob request, confirming that the intended regular
registry transport works there.
