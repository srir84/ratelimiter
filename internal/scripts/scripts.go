// Package scripts holds the store's Lua scripts (DESIGN §0, §A4.1). A Script carries its
// name, the lowercase hex SHA-1 of its text, which is the name EVALSHA calls it by, and the
// text SCRIPT LOAD sends when the store does not know it yet. Scripts are content-addressed,
// so two versions never overwrite each other (STA-16).
//
// The embedded *.lua files and the exported script values arrive in S4; until then New is
// the only way to build a Script (IMPLEMENTATION_PLAN S3 step 1).
package scripts

import (
	"crypto/sha1"
	"encoding/hex"
)

// Script is one store script: Name is its §A4.1 name without ".lua" (e.g. "acquire"),
// which also names its reply layout and its metric label; SHA is the lowercase hex SHA-1
// of Text; Text is exactly what the store receives.
type Script struct{ Name, SHA, Text string }

// New returns the script name with the given text and its SHA-1.
func New(name, text string) Script {
	sum := sha1.Sum([]byte(text))
	return Script{Name: name, SHA: hex.EncodeToString(sum[:]), Text: text}
}
