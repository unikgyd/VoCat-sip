# Linux software verification — 2026-10-26

Implementation was completed before the combined test/build phase. The tested
source is commit `2da4a5bc2435e020a14766fe79b640b4d46bc93f`, including the newly
built embedded frontend. The frozen input contains 1,821 files; its archive
SHA-256 is `f0c56d52325046989f1177d9d3b73abf4d9c8ee9cdb719dc42c213fdaef1999a`.
Source hashes matched before and after the run. Subsequent documentation commits
describe these results; they do not change the tested runtime code.

| Check | Result |
| --- | --- |
| Frontend tests | 79 passed, 0 failed, 0 skipped |
| TypeScript and Vite | Passed; generated frontend embedded in the binaries |
| Full native Go suite | 2,025 top-level tests passed, 46 skipped, 0 failed |
| Full `vocat_static` Go suite | 2,020 top-level tests passed, 43 skipped, 0 failed |
| Selected race regressions | 44 distinct top-level tests, repeated three times: 132 passes, 0 skips/failures |
| `go vet ./...` | Passed |
| Opt-in kernel regressions | 41 passed, each in a fresh network and mount namespace; 0 skips/failures |
| Embedded-Web Linux builds | amd64 and arm64, each with native and `vocat_static` media configurations: four passed |

Counts exclude subtests. Native Go tests/builds used Go 1.27.3 on Linux x86-64
(kernel 7.0.0-38-generic); frontend checks used Node 24.13.0 and npm 11.6.2.
Builds used `CGO_ENABLED=0`; race checks used `CGO_ENABLED=1`. Dependencies were
pinned by the lockfiles, with Go module downloads disabled during Linux checks.
The [machine-readable record](verification-20261026.json) contains test names,
remaining skips, binary sizes and SHA-256 values.

The full suites used `go test -count=1 -timeout=10m -json ./...`, with
`-tags vocat_static` for the second suite. Frontend checks used `npm test` and
`npm run build`. Each build used `go build -trimpath ./cmd/vocat` with the
appropriate `GOARCH` and media tag.

The new regressions exercise exact-dialog hold/resume and ACKs, rejected offers,
active-media cleanup, separate RTCP socket ownership and binding failure,
RTCP sequence/loss/report/RTT processing, original-journal recovery and stale
capabilities, stereo recording and retention, private per-SIM message revisions,
answer arbitration, task restoration journals and data-transfer cancellation.
They also preserve migration, SMS receipt and original profile restoration
assertions.

The first kernel runner shared one namespace between cases requiring fresh XFRM
state. Three cases failed because synthetic state from an earlier case remained.
Those failures are retained in the record. Running all 41 opt-in cases in separate
namespaces satisfied their prerequisites and passed without source changes.
There was no broad flush or modification of the host network namespace.

The full native suite's 46 skips comprise 41 opt-in kernel cases subsequently
passed above, two opt-in system ModemManager/QMI probes, and three native AMR
dependency/environment cases. `libasound.so.2` was available; native
`libopencore-amrnb.so.0` was absent. AMR library availability remains to be checked
on deployment targets.

Real SIM/RF changes, USB audio, automatic playback/takeover on live calls,
softphone interoperability, carrier voice/SMS delivery, physical hotplug and
installation/rollback remain hardware or deployment acceptance. No current
service was replaced and no real call/SMS was sent by this run. See the
[acceptance ledger](PARITY_ACCEPTANCE.md) for those boundaries. Local raw logs,
source manifests and candidate binaries are retained under
`publish/vocat-final-parity-20261026/run-6` in the containing workspace; these
private build archives are excluded from Git.
