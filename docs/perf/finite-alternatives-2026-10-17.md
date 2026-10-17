# Folded literal alternatives, 2026-10-17

The folded query `spotlight|mdutil|mds_stores` expanded into eleven ILIKE
predicates: two variants for `spotlight`, one for `mdutil`, and eight for
`mds_stores`. Each variant could recheck the same large body. The existing
single-literal specialization excluded alternatives.

This change generates one PostgreSQL expression for finite concatenations and
alternatives of folded ASCII literal nodes. It retains the source-size,
indexability and 32-variant eligibility checks. Explicit Go Unicode fold orbits
preserve long-s and Kelvin matches. Scoped case mixtures and other syntax retain
their existing paths. Go regexp remains the final matcher.

## Paired adapter measurements

Baseline: `17c10b17a0ec6f8fe5d7d4c1ba972c271f1d5ddc`.
Candidate renderer: `retrieval.go` blob
`9d052999a739bace2f2dc484671a83bd25c947ef`.
Both executables used the same temporary adapter driver and one migrated,
synthetic PostgreSQL database. Runs alternated A/B/B/A/A/B.

Environment: Apple M1 Pro, macOS 26.2 arm64, Go 1.26.6, isolated Docker
PostgreSQL 17.11 on a random loopback port. Other validation and agent work
continued on the host. These are warm exploratory results; they do not measure
cold-cache latency, p95 or hosted capacity.

The corpus contains 30 sessions with 1,000 messages each and 96,970,200 stored
text bytes. Hash bodies contain scattered trigrams for all three terms. Every
hundred messages includes separate complete `spotlight`, `mdutil` and
`mdſ_ſtoreſ` matches. Twenty sessions are five days old; ten are one day old.
Seven days admits 30,000 metadata rows; two days admits 10,000. The fixture is
deliberately adversarial and does not model a natural transcript distribution.

First-page milliseconds, median of three calls (range):

| Window and mode | Baseline | Candidate |
| --- | ---: | ---: |
| 7d ordinary | 9825 (9720–10245) | 869 (660–1025) |
| 7d continuation | 9385 (9312–9676) | 539 (525–547) |
| 2d ordinary | 3121 (3088–3131) | 545 (538–612) |
| 2d continuation | 3062 (3054–3130) | 523 (519–534) |

Ordinary calls used limit 35 and max_per_session 7. Continuation supports limit
20 and no max_per_session. Each timed call includes candidate selection,
hydration, Go verification and result metadata. Continuation cleanup was outside
the timer. The measurement budget was 60 seconds so baseline calls could finish.
An earlier baseline run with the default ten-second budget timed out on the
three-term query in both windows and modes while other host checks were active.
That run establishes a synthetic reproduction, not a production latency claim.

Every ordered first-page ID signature, hit count and ordinary total matched
between builds. Small PostgreSQL adapter tests separately compare complete
ordinary and continuation results with a full-scan Go oracle. Fixtures include
all eight long-s variants of `mds_stores`, both forms of `spotlight`, ASCII case,
Kelvin folds, scoped case, punctuation and controls. Tests retain fallback above
32 expanded variants.

Controls used A/B/B/A, two calls per case and build. The unchanged `mdutil`
path remained approximately 1 second. Absent `missingneedle` remained 3–15 ms.
The supported two-term `spotlight|absentneedle` query improved from 3.5–4.0
seconds to 0.51–0.69 seconds. All control signatures and counts matched.
Differences on unchanged paths are run noise.

## Reproduce

Use a dedicated PostgreSQL role with database-creation rights:

```sh
FLOPWIRE_TEST_DATABASE_URL=postgres://... go test ./internal/retrieval \
  -run '^$' -bench BenchmarkFiniteLiteralAlternativesAdapter -benchtime=3x -count=1
```

Copy only the benchmark file to the baseline checkout. The benchmark creates
and drops an isolated database per run. Compare ordered-ID signatures as well as
timings. Setup is excluded. The baseline may fail its default ten-second budget
on the deliberately broad case.

The paired run used the same seeded database for both executables and a
60-second per-call measurement budget. Its driver and sanitized measurement
records are in the [sanitized evidence archive](finite-alternatives-2026-10-17.tar.gz).

This change removes repeated alternative rechecks for the supported shape.
It does not bound every broad query's startup. Metadata overflow still falls
back to the existing global text candidate path. No engine, index, candidate cap,
timeout or continuation contract changes are included.
