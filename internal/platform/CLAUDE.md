# internal/platform

OS- and runtime-level controls for the engine's performance mode: **GC
suspension** and **core pinning**. Core pinning is build-tagged (Linux has real
affinity control; darwin is a no-op) so the engine builds and runs on the dev
machine while production tuning lives behind the Linux path.

## Files

- `gc.go` — `GCOff()` (disable GC for a measured session, returns prior percent)
  / `GCOn(prev)` (restore). The hot path targets zero allocation; GC-off removes
  residual collection jitter during benchmarks.
- `pin_linux.go` — real core affinity (build tag `linux`).
- `pin_darwin.go` — no-op (build tag `darwin`).
- `platform_test.go`.

## Constraints

- Keep the build-tagged files in sync: every exported symbol on the Linux path
  needs a matching no-op (or alternative) on darwin so the package builds on
  both. Don't reference Linux-only syscalls outside the `linux` file.
- `GCOff` is for measured sessions only; always pair it with a deferred
  `GCOn(prev)`. Do not disable GC in normal operation.

## Testing (positive / negative / edge)

`platform_test.go` exists. Cover:
- **Positive**: `GCOff`/`GCOn` round-trip restores the prior GC percent;
  pinning succeeds (Linux) / is a clean no-op (darwin).
- **Edge/negative**: nested or repeated GCOff/GCOn; pinning to an out-of-range
  core. Tests must pass on both Linux and darwin (build-tag aware).
