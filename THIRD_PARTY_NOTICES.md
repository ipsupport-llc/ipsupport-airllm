# Third-party notices

AirLLM (this repo) is Apache-2.0 licensed — see `LICENSE` and `NOTICE`. This
file enumerates the third-party components that ship inside the two
published container images (`ghcr.io/ipsupport-llc/ipsupport-airllm` and
`ghcr.io/ipsupport-llc/ipsupport-airllm-dlp-bert`), so "what did we actually
distribute, under what terms" can be answered without re-deriving it from
`go.mod`/`pip install` lines every time.

**Bottom line:** everything below is permissive (MIT / BSD-2 / BSD-3 /
Apache-2.0), except `tqdm`, which carries an MPL-2.0 file-level exception
(see below) — not a problem for how we use it, but the one non-pure-permissive
entry in the whole dependency tree. No GPL/LGPL/AGPL-licensed *application*
code is linked into or shipped as part of either image's compiled artifact.

## Gateway image (`ipsupport-airllm`)

Go module closure actually linked into the shipped binary (`go list -deps
./cmd/ipsupport-airllm/...`, cross-checked against each module's `LICENSE`
file in the module cache) — 23 modules, all permissive:

| Module | License |
|---|---|
| cloud.google.com/go/compute/metadata | Apache-2.0 |
| github.com/beorn7/perks | MIT |
| github.com/cespare/xxhash/v2 | MIT |
| github.com/coreos/go-oidc/v3 | Apache-2.0 |
| github.com/go-jose/go-jose/v4 | Apache-2.0 |
| github.com/jackc/pgpassfile | MIT |
| github.com/jackc/pgservicefile | MIT |
| github.com/jackc/pgx/v5 | MIT |
| github.com/jackc/puddle/v2 | MIT |
| github.com/munnerz/goautoneg | BSD-3-Clause |
| github.com/prometheus/client_golang | Apache-2.0 |
| github.com/prometheus/client_model | Apache-2.0 |
| github.com/prometheus/common | Apache-2.0 |
| github.com/prometheus/procfs | Apache-2.0 |
| github.com/redis/go-redis/v9 | BSD-2-Clause |
| golang.org/x/crypto | BSD-3-Clause |
| golang.org/x/oauth2 | BSD-3-Clause |
| golang.org/x/sync | BSD-3-Clause |
| golang.org/x/sys | BSD-3-Clause |
| golang.org/x/text | BSD-3-Clause |
| google.golang.org/protobuf | BSD-3-Clause |
| go.uber.org/atomic | MIT |
| go.yaml.in/yaml/v2 | Apache-2.0 |

Base image: `golang:1.26-alpine` (build stage, discarded) →
`alpine:3.20` (runtime). The Go binary is `CGO_ENABLED=0`, statically linked,
not linked against musl/BusyBox — Alpine's BusyBox (GPL-2.0) and musl libc
(MIT) just sit alongside it unmodified in the filesystem layer, which is
standard practice and creates no derivative-work obligation on our binary.

`e2e/@playwright/test` (Apache-2.0) is a dev-only test dependency — never
built into either released image.

## DLP-BERT sidecar image (`ipsupport-airllm-dlp-bert`)

**Pinned model weights baked into the image:** `dslim/bert-base-NER` —
**MIT** (per the model's Hugging Face card). Its CoNLL-2003 training data is
tagged license "other" upstream, which only matters if redistributing that
*dataset*; we only redistribute the already-trained model weights, which are
MIT.

**Python dependencies** (`deploy/dlp-bert/Dockerfile` installs these via
unpinned `pip install`, so exact resolved versions drift between builds —
see caveat below):

| Package | License |
|---|---|
| torch | BSD-3-Clause (custom PyTorch/Caffe2 BSD-style text; a few vendored C libs inside the wheel are BSD-2/MIT) |
| transformers | Apache-2.0 |
| tokenizers | Apache-2.0 |
| huggingface-hub | Apache-2.0 |
| safetensors | Apache-2.0 |
| fastapi | MIT |
| pydantic | MIT |
| starlette | BSD-3-Clause |
| uvicorn[standard] | BSD-3-Clause |
| uvloop, httptools, websockets, watchfiles, python-dotenv | MIT / BSD-3-Clause |
| numpy | BSD-3-Clause |
| regex | Apache-2.0 |
| requests | Apache-2.0 |
| pyyaml | MIT |
| filelock | Unlicense (public domain) |
| packaging | Apache-2.0 OR BSD-2-Clause (dual) |
| **tqdm** | **MPL-2.0 AND MIT** — MPL-2.0 applies file-by-file to tqdm's own source; it only requires sharing *modified MPL files*, not anything that merely imports the package. We don't modify tqdm, so no obligation triggers; flagged because it's the one dependency in this whole tree that isn't purely permissive. |
| typing-extensions | PSF-2.0 (permissive) |

Base image: `python:3.11-slim` (Debian) — not modified beyond adding our
own `app.py`; standard base-image usage, not separately audited here.

## Caveat: unpinned Python versions

Unlike the Go side (`go.sum` pins every version and license was checked
against the exact pinned module), `deploy/dlp-bert/Dockerfile` does
`pip install torch …` / `pip install fastapi uvicorn[standard] transformers`
with no version pins. The table above reflects each package's current,
long-standing license and is unlikely to be wrong, but it is not a hash-pinned
guarantee the way the Go table is — a full rebuild can pull newer transitive
dependencies this table doesn't enumerate. If an exact SBOM is ever needed
(e.g. for an audit), generate one from the actual built image
(`pip-licenses` inside the running container, or `syft`/`grype` against the
image) rather than trusting this file as a live source of truth.
