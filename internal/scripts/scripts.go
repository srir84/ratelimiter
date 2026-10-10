package scripts

import (
	"crypto/sha1"
	"embed"
	"encoding/hex"
	"strings"
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

//go:embed *.lua
var files embed.FS

// The 12 scripts of DESIGN §A4.1 (IMPLEMENTATION_PLAN S4, S6, S7, S8), assembled once at
// package initialization and never changed afterwards.
var (
	ReadEpoch       = assemble("read_epoch")
	Hold            = assemble("hold")
	AdminStartEpoch = assemble("admin_start_epoch")
	AdminStatus     = assemble("admin_status")
	AdminInspect    = assemble("admin_inspect")
	AdminRestore    = assemble("admin_restore")
	AdminClear      = assemble("admin_clear")
	Acquire         = assemble("acquire")
	Refund          = assemble("refund")
	DueEntries      = assemble("due_entries")
	CleanupEntry    = assemble("cleanup_entry")
	Reclaim         = assemble("reclaim")
)

// ByName returns the script with the §A4.1 name, and false for any other name.
func ByName(name string) (Script, bool) {
	switch name {
	case "read_epoch":
		return ReadEpoch, true
	case "hold":
		return Hold, true
	case "admin_start_epoch":
		return AdminStartEpoch, true
	case "admin_status":
		return AdminStatus, true
	case "admin_inspect":
		return AdminInspect, true
	case "admin_restore":
		return AdminRestore, true
	case "admin_clear":
		return AdminClear, true
	case "acquire":
		return Acquire, true
	case "refund":
		return Refund, true
	case "due_entries":
		return DueEntries, true
	case "cleanup_entry":
		return CleanupEntry, true
	case "reclaim":
		return Reclaim, true
	}
	return Script{}, false
}

// assemble builds the script name from <name>.lua and prelude.lua (§A4.2): the file's first
// line, "\n", the prelude, then the rest of the file. A missing embedded file or a file with
// no line after its header is a build defect, so it panics during package initialization.
func assemble(name string) Script {
	prelude, err := files.ReadFile("prelude.lua")
	if err != nil {
		panic("scripts: " + err.Error())
	}
	src, err := files.ReadFile(name + ".lua")
	if err != nil {
		panic("scripts: " + err.Error())
	}
	header, rest, ok := strings.Cut(string(src), "\n")
	if !ok || !strings.HasPrefix(header, "#!lua") {
		panic("scripts: " + name + ".lua does not start with a #!lua header line")
	}
	return New(name, header+"\n"+string(prelude)+rest)
}
