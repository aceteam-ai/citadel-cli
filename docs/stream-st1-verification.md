# ST1 offline verification

Measured 2026-10-20 UTC on ubuntu-gpu, Linux amd64, Go 1.26.6,
13th Gen Intel Core i5-13600K. This is source acceptance for Citadel #1352,
not a live runtime, fleet capacity, release or deployment result. All inputs
are the committed synthetic Shield corpus. No service was started.

## Checks

The following passed in the isolated ST1 worktree after formatting:

```sh
go test ./internal/stream/... -count=1
go test -race ./internal/stream/...
go vet ./internal/stream/...
go test ./cmd -run 'Test.*Stream|TestRoot.*|Test.*PersistentPreRun' -count=1
go build -buildvcs=false -o /tmp/citadel-st1 ./cmd/citadel
```

A writable temporary `GOCACHE` was used. The build disables only VCS metadata
stamping because the sandboxed Go VCS subprocess failed; it does not disable
any runtime guard. Normal GitHub CI remains the ordinary build/test gate.

`TestShieldOriginalGoldenFixturesBothTimeModes` verifies the original
R1–R5 and `stat_only` flags for all seven original scenarios, including
split-IP /24 grouping, in both time modes. Each mode also repeats the full
evaluation and checks byte-identical canonical JSON and output digests.
History matching is excluded. Other tests cover named unsupported-feature
refusal, bounded state, ordering, privacy and exact input-file custody.

The shipped CLI was also run against the complete 147-record, 21,792-byte
corpus. The following are the CLI report digests, not the distinct internal
golden test snapshot format:

| Mode | `output_digest` |
| --- | --- |
| ingest | `sha256:e29825013b93ae61127b3d86e0dcb8a66c06596e6cf523011eb5179238187d03` |
| event | `sha256:c3d6de5f387d1fe062e5ea1390f7eea07dcf252ecec191eea5aa925365286b27` |

Both use pack digest
`sha256:1a6d94bd71ec5f4b3bc091e939a722e02638923346a42dab8553f9bf75ac90cf`.
Two separate ingest invocations produced the same complete stdout SHA-256:
`d20bd88cbde29e5db871df309fa7953f67e19cec1f81e3af538b919b1b2e7663`.

```sh
/tmp/citadel-st1 stream eval internal/stream/testdata/pack.yml internal/stream/testdata/shield
/tmp/citadel-st1 stream eval internal/stream/testdata/pack.yml internal/stream/testdata/shield --time event
```

## Throughput

```sh
GOMAXPROCS=4 go test -p 4 ./internal/stream/rules -run '^$' \
  -bench '^BenchmarkShieldEvaluatorCorpus$' -benchtime=2s -count=3
```

Each operation parses and evaluates all seven scenarios with fresh engines,
including EOF flush, sorting and statistics. Pack loading and fixture
reading/hash validation are outside the timer. This measures the combined
parser and complete Shield pack, not a filter-only microbenchmark.

| Mode | Three samples, events/s | MB/s range | Allocation range per 147-event operation |
| --- | --- | --- | --- |
| ingest | 37,207; 36,320; 34,894 | 5.17–5.52 | 9,490,375–9,495,693 bytes; 21,546–21,549 allocations |
| event | 31,200; 34,734; 36,032 | 4.63–5.34 | 9,617,069–9,621,133 bytes; 21,920–21,922 allocations |

The machine is shared and CPU isolation was not requested. An earlier run
overlapping cold Go compilation ranged from 4,739 to 34,557 events/s; do not
treat the later samples as a guaranteed rate. Small fixture windows do not
prove performance at the maximum key/event caps or under long-lived load.
No native arm64 throughput result is claimed here. Coordination confirmed
there is currently no approved isolated arm64 runner for this test; the Mac
is allocated to another lane and was not used.
