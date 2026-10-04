// Package opencodeplugin holds the Flopwire plugin for opencode
// (flopwire.js), which `flopwire setup` installs into opencode's global
// plugin directory and `flopwire probe` into its scratch one.
package opencodeplugin

import _ "embed"

// Source is the plugin file.
//
//go:embed flopwire.js
var Source []byte

// FileName is the plugin's file name in an opencode plugin directory.
const FileName = "flopwire.js"
