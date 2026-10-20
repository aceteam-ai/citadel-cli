# Shield synthetic golden fixtures

These seven combined-access-log fixtures are byte-for-byte copies of the
existing `examples/*.log` files in
`/home/jason/workspace/aceteam-ai/shield-concept/`, inspected on 2026-10-19.
They contain synthetic documentation/benchmark IP addresses, example hostnames,
and generated requests, not customer logs. `gen.py` declares `random.seed(7)`;
it was read, not rerun. Each vendored log retains its original final newline.

Scope is [Citadel #1352](https://github.com/aceteam-ai/citadel-cli/issues/1352),
under [the ST1 design of record](https://github.com/aceteam-ai/aceteam/issues/11673#issuecomment-6274347426),
sections 3.3, 3.5, 3.8, and 7.

## Source durability prerequisite

At this inspection the prototype directory had no separate Git repository.
Its enclosing repository was `/home/jason/workspace/aceteam-ai`, whose
`git ls-files -- shield-concept` returned no tracked paths and whose scoped
status listed the prototype files as untracked. There is no source commit to
cite. The design explicitly requires committing the prototype before ST1;
vendoring these logs does not prove that separate prerequisite is satisfied.
This audit did not commit or change the original prototype.

## Golden expectations

`expected.json` records each scenario's `flagged` mapping from source IP to
sorted R1 through R5 rule IDs, plus the sorted `stat_only` IP list. Scenario
names match log basenames without `.log`. These expectations come from reading
the scenario assertions in `test_shield.py`, the default policy, and
`flow/sentinel.j2`, not from execution or newly generated detector output.
The prototype explicitly asserts the `stat_only` lists for a, c, and f; the
other empty lists also follow directly from inspecting their fewer-than-eight
404 responses per source IP. No parser, generator, Jinja template, or test was
executed in this fixture-vendoring step.

`sentinel.j2`, `policy.default.json`, `gen.py.txt`, and `test_shield.py.txt`
preserve the exact inspected prototype specification as inactive provenance
data. The `.py.txt` suffixes prevent automatic Python test discovery or module
import. They are not an executable implementation, evaluator dependency, or
test-results receipt. This does not preserve every file of the prototype or
claim that its original workspace directory is committed.

- R1 is default-policy known-bad membership; R2 is SQL-injection pattern match;
  R3 is a sensitive-path probe; R4 is at least five failed POST `/login`
  requests per IP; R5 is failures followed by a successful POST `/login`.
- Failure statuses are 401 and 403; success statuses are 200 and 302.
- e's `198.18.1.60` succeeds before its failures and must never acquire R5
  merely because R4 eventually matches. Its two-mistype legitimate user
  remains clean.
- e2 has three failures from each of three IPs, then success on
  `198.18.2.143`. No single IP reaches five failures. Its R5 correlation groups
  by the declared `source.ip_subnet24` enrichment, `198.18.2.0/24`, while the
  reported actor is the succeeding IP. Its result has R5 without R4.
- `stat_only` is an entity-level verdict: at least eight 404 responses and no
  R1 through R5 match for that IP. It is not an additional malicious rule ID.
  A report must not label an IP stat-only if a malicious rule also matches it.

## Window and ordering differences

The Jinja prototype has batch-wide counts and input-row ordering, no clock or
time window, and no state between batches. ST1 intentionally replaces this
execution model with bounded windowed state. R5 must be a chained Sigma
correlation over a failed-login count and success, with a ten-minute window;
it must not be reimplemented as a hidden special-case detector.

The fixture log timestamps are chronological in file order. Batch evaluation
can explicitly select event-time replay; ingest-time evaluation must use a
deterministic replay timeline rather than wall-clock execution speed. The
positive R5 sequences are each contained within ten minutes. Scenario f spans
10:00:00 through 10:10:38 in total, but its earlier normal events have different
actors from its later scanner and crawler. Its crawler's ten 404s occur from
10:10:20 through 10:10:38; c's ten crawler 404s occur from 10:05:00 through
10:05:18. Both fit one ten-minute window. These fixtures alone do not establish
boundary, lateness, timeout, cross-window, or restart correctness; separate
operator tests must cover those.

Rule arrays and stat-only IP lists are canonicalized for golden comparison;
the `flagged` object's property ordering is not an output-order contract.
The logs' original row ordering must not be changed. Determinism of full
alert IDs and output digests is a separate ST1 acceptance requirement.

## Deliberately excluded history matching

The prototype also reads `examples/history.json` and matches by IP,
`ua_path`, or subnet according to `policy.match_keys`. That enrichment stays
in the escalated flow's Investigator, outside ST1. In particular, no golden
claim is made here for c's benign precedent `INC-2026-0266`, f's scanner
`ua_path` match against `INC-2026-0301`, or f's crawler having no history
matches. Historical matches in b and d are likewise not evaluator acceptance.
`history.json` and its records are not runtime dependencies of this fixture
pack. Existing R1 through R5 and stat-only outcomes must not be suppressed or
promoted by past history verdicts.

## SHA-256 inventory

The log and specification hashes below were checked against both the original
and vendored bytes. The remaining history hash identifies excluded source data;
they do not imply the untracked prototype was committed or executed.

| Original path | SHA-256 |
| --- | --- |
| `examples/a_clean.log` | `c7ddc9b3d510b292f14eeffd0524737cb694be2c4f4afb9e6dbeceb6ee67c71b` |
| `examples/b_sqli_knownbad.log` | `13b565d8ad093aa12d6e5375b453e4557c8946ed55ee526293c5d2d8b9519533` |
| `examples/c_ambiguous_crawler.log` | `38aabe9b55a4efd0d1329d24350c2610857e17b8cfaaec1ec34b9e15238bde3b` |
| `examples/d_repeat_credstuffing.log` | `e3e07c4bcccbf5ff6573559c2690a268fab768c5a5e43976ad4b5dbca367440f` |
| `examples/e_fail_then_success.log` | `2d4b9099965c560e117e8115c5700eea50a7206ef84a053895b4ab85d30e9066` |
| `examples/e2_split_ip_fail_then_success.log` | `5e03df6c856113175305e4fef54bfcc82ad5f9ad29dc7f517e8dba8dcddbb4d5` |
| `examples/f_multikey_match.log` | `271a9ee1ca05c312f140e1aea887d6496c01e00ba4c80fb5e6a07f3b21afeb88` |
| `flow/sentinel.j2` | `d9b9568f1a15d77d35ebb6979441ca2d8cf48f18a882da7317033c77187b61d4` |
| `flow/policy.default.json` | `ca6cc2001023a62c1520ff95e46bf86e6800ec0fb51fc38822b0ab6aa4e7323a` |
| `test_shield.py` | `1571659b97e0c6340930cee62d89388f893433bb6d0726a2f8d195b05017c8e6` |
| `gen.py` | `afd45f1dbc57a03898f27bd6768ef23a31ff28333957a098a410030724b65602` |
| `examples/history.json` (not vendored) | `1803613037aa5e23e93d4f604aceaee54d71428beb7d42f790a56ad4044d5c34` |
