// Package settings declares the limiter's settings (DESIGN §A5.2, §A5.10; SPEC CFG-8)
// and validates them (validate.go: CFG-4, §A3.1, RCP-8, §A5.6). The host builds them,
// usually from DefaultPolicy and DefaultInstance, and passes them to New; the library
// never reads settings from the environment, files or flags.
package settings
