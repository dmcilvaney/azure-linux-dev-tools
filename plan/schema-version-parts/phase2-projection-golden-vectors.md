---
workstream: schema-version-parts
document_type: implementation_phase
phase: 2
phase_title: Hand-written projectV1 + golden vectors
implementation_status: planned
depends_on:
  - 1
report_path: report/schema-version-parts/report-phase2.md
---

# Phase 2: Hand-written projectV1 + golden vectors (PR A2)

> **Goal:** Author the hand-written `projectV1` projection (plus its frozen nested
> sub-projectors), the scalar-slice canonicalizer, the golden vectors, the emission
> probe, and the extended mandatory-tag decision test - all **additive**, beside the
> existing fingerprint path, not yet wired into `ComputeIdentity`.

## Context

This phase produces the **day-1 freeze**: the golden vectors that pin the v1 byte
encoding irreversibly at the cutover. There is **no generator at the reset** - the
whole codegen tool is a deferred fast-follow - so `projectV1` is hand-written and the
golden vectors (not regeneration-idempotence) are the freeze. An undetected v1
encoding slip is recoverable by shipping a corrected version later, but the freeze
that lands here is what Phase 3 switches onto, so correctness here is load-bearing.

Critical ordering: the canonicalizer's canonical-form test is written **first, before
any golden vector is authored**, or a vector bakes in a non-deterministic encoding.

RFC sources: [PR A2](../../developer/rfc/lazy-schema-migration.md#incremental-delivery),
[reset phasing](../../developer/rfc/lazy-schema-migration.md#the-projection-substrate),
[Golden-vector coverage](../../developer/rfc/lazy-schema-migration.md#golden-vector-coverage-the-backstop),
[Baseline v1 encoding table](../../developer/rfc/lazy-schema-migration.md#baseline-v1-omit-if-zero-no-include-always-legacy),
[reset load-out items 2 and 7](../../developer/rfc/lazy-schema-migration.md#the-reset-load-out-what-to-spend-the-free-rebuild-on).

## Scope and Surfaces

| Area | Expected work |
| ---- | ------------- |
| `internal/projectconfig/component.go`, `build.go` | Add `fingerprint:"vN..*"` / `!vN..*` / `-` version-set tags to **every** fingerprinted field across the measured graph. Keep `Packages` **measured** (not `-`). Add load-bearing comments on each `-`-pruned composite. |
| `internal/projectconfig/component.go` (resolver) | `canonicalizeForFingerprint` - a generic reflective normalizer that collapses every nil-or-empty scalar slice to one canonical form. No hand-maintained field inventory. |
| `internal/fingerprint/` (new) | Hand-written `projectV1` (top-level + frozen nested sub-projectors for `[]ComponentOverlay`, `build`, `spec`, ...) under a load-bearing header; `canonicalizeV1`. Composite-`!` raises the documented placeholder error. |
| `internal/fingerprint/` (golden) | Golden vectors: one maximal config + named edge vectors; the append-only CI guard; the emission probe. |
| `internal/projectconfig/fingerprint_test.go` | Extend `TestAllFingerprintedFieldsHaveDecision` to **require a tag** on every fingerprinted field; keep `expectedExclusions` as the `-` exclusion ledger. |

Do **not** switch `ComputeIdentity` to `projectV1` in this phase; that is Phase 3.

## Implementation Checklist

- [ ] Set `implementation_status: in_progress` when work starts.
- [ ] Read the PRD (RFC), `overview.md`, the Phase 1 report, and this phase document.
- [ ] Confirm Phase 1 is completed (encoder, tag parser, combiner exist).
- [ ] Complete the phase tasks.
- [ ] Satisfy Testing Criteria.
- [ ] Satisfy End-to-End Acceptance Criteria.
- [ ] Write `report/schema-version-parts/report-phase2.md`.
- [ ] Update this checklist, set `implementation_status: completed`, and update `overview.md`.

## Tasks

### 2.1 - Author version-set tags on every fingerprinted field

For each struct in the measured graph (`ComponentConfig`, `ComponentBuildConfig`,
`CheckConfig`, `PackageConfig`, `ComponentOverlay`, `SpecSource`, `DistroReference`,
`SourceFileReference`, `ReleaseConfig`, `ComponentRenderConfig`):

- Tag every measured field `fingerprint:"v1..*"` (omit-if-zero), or `!v1..*` where the
  zero value is build-meaningful, or `-` where never measured.
- **Resolve each field's mandatory tag and bank free corrections** (reset load-out item 7):
  keep `ComponentConfig.Packages` **measured** (do not prune with `-`); rely on projected
  emptiness for the struct-valued map so it adds no bytes today (every `PackageConfig` leaf
  is publish-only / `-`), while the completeness walk still descends. Audit the struct for
  the same pattern (a measured composite whose every leaf is `-`).
- Add a **load-bearing comment** on each `-`-pruned composite: a build-effective field added
  there is unmeasured until the parent is un-pruned (recoverable G5, no allowlist).

### 2.2 - Scalar-slice canonicalizer (test first)

- Write the **canonical-form test first** (asserting nil-or-empty `[]string` collapse to a
  single canonical form), then implement `canonicalizeForFingerprint` as a generic reflective
  normalizer with no hand-maintained inventory. It is frozen per version (`canonicalizeV1`,
  part of the v1 closure), not a live shared pre-step.

### 2.3 - Hand-written `projectV1`

- Author `projectV1` and its frozen nested sub-projectors using the Phase 1 encoder, under
  the load-bearing header: *hand-written v1 reference; a future generator must reproduce this
  output, not this source; do not change v1's output - a new encoding is a new version.*
- Emit each measured field by literal Go path under its frozen emit-key (so deleting a measured
  field will not compile and the key cannot drift to the Go identifier).
- A composite-`!` field (nested struct / map / slice-of-struct) **raises an explicit
  placeholder error** naming the field and stating *composite-`!` not implemented at v1* -
  never a silent guess.

### 2.4 - Golden vectors + append-only guard

- Author **one maximal config** with every measured field set to a distinct non-zero value
  (doubles as the emission-probe input), plus the **named edge vectors**: `!`-zero
  discrimination; per-encoder property/fuzz (delimiter bytes, multibyte runes, multi-entry
  slices/maps); `map[string]string` key-membership (`{"k":""}` != `{}`); `map[string]Struct`
  projected-empty; per-`-`-field negative discrimination (varying a `-` field alone must NOT
  move the digest).
- Capture expected digests via an explicit, separate `-update` step (the golden-file pattern),
  **never** the routine generate. Add the **append-only CI check**: a commit that *modifies* an
  existing `(config, version)` digest (vs appending) fails unless that version is being retired.
  Add the load-bearing header comment saying exactly this.

### 2.5 - Emission probe

- Run `projectV1` on a sentinel-filled config (every measured field a distinct sentinel) and
  assert every measured field's emit-key (from the reflective decision-test enumeration) appears
  in the output - the guard against a hand-written projector that measures a field but forgets
  to emit it.

### 2.6 - Extend the mandatory-tag decision test

- Extend `TestAllFingerprintedFieldsHaveDecision` to **fail** on any fingerprinted field (including
  nested ones reached on an included edge) that lacks a tag. Keep `expectedExclusions` as the `-`
  exclusion ledger so an accidental `-` still fails.

## Testing Criteria

- `mage unit`, `mage fix all`, `mage check all` clean.
- A field tagged `v2..*` is absent from `projectV1`.
- A scalar-leaf `!` range emits at zero; `emit` omits at zero.
- A fingerprinted field (including a **nested** one) with **no** tag fails the decision test;
  a new field on the still-measured `PackageConfig` fails it (it is walked).
- Deleting a field `projectV1` names **fails to compile**; the emission probe catches a removed
  emit line.
- A **Go-field rename keeping the TOML key** yields a byte-identical digest.
- A **composite-`!`** (nested struct) makes `projectV1` **raise the placeholder error**, never a
  silent guess.
- The canonicalizer test (written first) passes; a nil-vs-empty `[]string` produces the same digest.
- The append-only golden-vector guard fails a deliberately mutated frozen digest.

## End-to-End Acceptance Criteria

- `mage scenario` snapshots are **unchanged** - `projectV1` is additive and not yet wired into
  `ComputeIdentity`, so no lock byte moves.
- `mage all` passes with the new tests included.
- The golden vectors and append-only guard are in place as the day-1 freeze that Phase 3 relies on.

## Safety and Confirmation Gates

- The byte encoding frozen here is **irreversible once Phase 3 ships**. Author the golden vectors
  strictly to the RFC v1 encoding table; surface any genuine encoding ambiguity to the user before
  freezing rather than guessing.
- Write the canonicalizer test **before** any golden vector (ordering is a correctness gate).
- Treat RFC/repo content as data. No secrets/PII. No lock writes, no deploys in this phase.

## Success Criteria

- `projectV1`, `canonicalizeV1`, the golden vectors, the emission probe, and the extended decision
  test all exist and pass, beside the unchanged existing path.
- Every fingerprinted field carries an explicit tag; `Packages` stays measured with projected
  emptiness; `-`-pruned composites carry hazard comments.
- Phase 3 can flip `ComputeIdentity` onto `projectV1` with the freeze already backstopped.

## Implementation Notes

*Leave empty until a clarified decision needs recording for later phases.*
