# Fixture plan

## 3. Slices

### S0 — Named tests
- **Goal:** prose naming `TestOutsideGoal`, which is not in a Tests section.
- **Depends on:** —
- **Build:** also mentions `TestOutsideBuild` and the phrase - **Tests:** inside a sentence.
- **Tests:**
  - `TestPass` passes; `TestMissing` does not exist; `TestRenamed` was renamed.
  - `TestNoMatch_*` matches nothing; `TestStar_*` matches two passing tests.
  - `TestNoPass` ran without a pass; `TestBrace_{A,B}` is met by subtests.
  - `TestGap_{A,B}`: its parent and subtest A passed, but no subtest B ran.
  - `TestOtherPkg` is in a package no target runs; `TestSkipped` is excluded with -skip.
  - `TestOther_{sub,kept}`: the -skip alternative `TestOther/sub` excludes only the subtest sub.
  - `TestFlaky` failed in one run and passed in another; `TestFlakyStream` failed in one stream and passed in another.
  - `ExampleNoOutput` has no output comment; `TestT_COR_99` is left to the ID check.
  - `report/tests/test_a.py::test_py_pass` passed; `report/tests/test_a.py::test_py_nopass` has no pass.
  - `report/tests/test_b.py::test_py_nojunit` has no JUnit file; prose like `Example…` and `make test-unit` is not a name.
  - `TestSkipThenPass` passed in one stream and was skipped in another; `report/tests/test_a.py::test_py_skip` has a skipped case beside a passing one.
  - `TestUnrelatedSkip` has no result: only the command for ./pkg/unrelated skips it; `TestRanOnly` is skipped by the command for ./pkg/ran.
  - `TestCaseSkip` passed beside its skipped table case b; `TestGroup_{A,B}`: subtest A passed beside its skipped case y, and B passed.
  - `test_py_underscore` passed and is defined only in report/_impl/test_c.py: only go ./... leaves out a "_" directory.
  - `TestThenSkip` is skipped by a command after then, `TestDoSkip` by one after do, and `TestWrapperSkip` by a gotest wrapper with another name.
- **Verify:**
  - `TestAfterVerify` is outside the Tests section.

### S1 — Off the S2 path
- **Depends on:** S0.
- **Tests:**
  - `TestOnlyInS1` does not exist.
- **Verify:** none.

### S2 — Depends on S0 only
- **Depends on:** S0.
- **Tests:**
  - (a) nothing new.
- **Verify:** none.

## 4. Test → slice

| Test | Slice | Where it lives |
|---|---|---|
| T-COR-1 | S9 | harness/spectest (Go, rltest) |
