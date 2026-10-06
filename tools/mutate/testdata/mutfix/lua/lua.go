// Package lua is the Lua fixture of tools/mutate's tests: embedded scripts assembled with
// their prelude, as internal/scripts assembles the store's scripts.
package lua

import (
	"embed"
	"strings"
)

//go:embed *.lua
var files embed.FS

// Script returns name.lua's first line, then prelude.lua, then the rest of name.lua.
func Script(name string) (string, error) {
	prelude, err := files.ReadFile("prelude.lua")
	if err != nil {
		return "", err
	}
	src, err := files.ReadFile(name + ".lua")
	if err != nil {
		return "", err
	}
	header, rest, _ := strings.Cut(string(src), "\n")
	return header + "\n" + string(prelude) + rest, nil
}
