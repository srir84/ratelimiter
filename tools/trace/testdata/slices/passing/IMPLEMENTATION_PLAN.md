# Fixture plan

## 3. Slices

### S0 — Fixture slice
- **Depends on:** —
- **Tests:**
  - (a) `TestHelper` passes.
- **Verify:** none.

## 4. Test → slice

| Test | Slice | Where it lives |
|---|---|---|
| T-COR-1 | S0 | harness/spectest (Go, rltest) |
| T-RPT-1 | S0 | report/ (pytest, run_evidence) |
