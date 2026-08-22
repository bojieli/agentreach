<!--
Thank you for contributing. The checklist below is short because most of it is
covered by `make check`; the items that remain are the ones CI cannot decide.
-->

## What this changes

<!-- And why. If it fixes something subtle, say what the failure looked like:
     the comments in this codebase exist so the next reader knows which shell,
     harness or protocol quirk made the code look the way it does. -->

## Checklist

- [ ] `make check` and `make lint` pass
- [ ] `make integration` passes, if this touches transports or file operations
- [ ] Comments explain *why* where the code looks odd

If this touches a **harness adapter**:

- [ ] A conformance test covers the seam, and fails when the seam changes shape
- [ ] `docs/RESEARCH.md` records what was observed, and the harness version it
      was observed on
- [ ] Anything not verified is written down as unverified rather than implied to
      work

If this touches a **file-operation tier**:

- [ ] New cases went into the shared suite (`internal/fileops/fileopstest`), so
      every tier has to pass them, not just the one you changed
- [ ] `make bench` was re-run and `docs/TRANSPORTS.md` updated, if this changes
      what reach negotiates

If this changes what reach puts on a **target**:

- [ ] It is opt-in, visible in `reach doctor`, and removable
- [ ] Autonegotiation still cannot select it
