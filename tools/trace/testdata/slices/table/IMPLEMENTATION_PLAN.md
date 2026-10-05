# Fixture plan

## 3. Slices

### S0 — Fixture slice
- **Depends on:** —
- **Tests:**
  - (a) none.
- **Verify:** none.

## 4. Test → slice

Every row's slice is off S0's dependency path or unknown, so only the table rules report.

| Test | Slice | Where it lives |
|---|---|---|
| T-COR-1 | S1 | harness/spectest (Go, rltest) |
| T-COR-3 | S99 | harness/spectest (Go, rltest) |
| T-COR-1 | S1 | harness/spectest (Go, rltest) |
| T-COR-4 | S1 | harness/spectest (Go, rltest) |
| T-COR-99 | S1 | harness/spectest (Go, rltest) |
| T-RPT-1 | H5 | report/ (pytest, run_evidence) |
