// Package golden checks written run-log lines against hand-written golden lines (DESIGN §C5,
// IMPLEMENTATION_PLAN H1 step 8): a written line pairs with the golden line that has the same
// values for the test's exact keys (by position when it names none); paired lines must have
// the same keys at every level and the same JSON type for each value (arrays element by
// element, of equal length); other values may differ. It is a test helper: production code
// never imports it.
package golden
