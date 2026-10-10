// Package metrics is the limiter's fixed metric registry (DESIGN §A5.11; SPEC API-1g,
// STA-14): one family per §A5.11 row, 17 families and 55 series, every label value
// registered at construction. Callers name a label value by its index in the family's
// table, never by a string, so no key, request, error text or reason can become a label
// and the series count never grows. Values are atomics: one Registry per Limiter, safe for
// concurrent use.
package metrics
