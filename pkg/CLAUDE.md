# pkg

The module's intended-public surface: startup-only support packages. Neither is
touched on the hot path.

## config

`config.go` — `Config` (markets, ring size, WAL path, fixed-point scales, fee
rates, journaling mode, and snapshot cadence/retention/path) loaded from
environment variables, read **once at startup**. Journaling keys:
`OB_JOURNAL_MODE` (`sync` default, or `async` for off-thread fsync — the 1M
durable path) and `OB_JOURNAL_RING` (async hand-off ring size; power of two or 0
for the engine default). Snapshot keys: `OB_SNAPSHOT_PATH` (must differ from
`OB_WAL_PATH`), `OB_SNAPSHOT_EVERY` (count) / `OB_SNAPSHOT_INTERVAL` (seconds;
at least one > 0), `OB_SNAPSHOT_RETAIN` (>= 1). No
third-party dependencies. `Default()` supplies built-in defaults; `Load(getenv)`
takes an injected `getenv` (tests pass a deterministic map; production uses
`LoadFromOS`). It **validates** before returning. See `.env.example`.

Constraints: keep it dependency-free and side-effect-free apart from reading env
and validating. All fields are plain values so `Config` can be copied and logged.

Testing (`config_test.go`): **positive** — valid env produces the expected
config; **negative** — invalid/malformed values (bad scale, negative fee,
non-power-of-two ring) are rejected with an error; **edge** — unset keys fall
back to `Default()`, boundary values, empty market list.

## logger

`logger.go` — a thin wrapper over stdlib `slog`. `New(level)` writes text to
stderr; `Default()` is Info level. The engine logs **only at startup and
shutdown — never on the hot path**. Keep it trivial; no tests required beyond
construction.
