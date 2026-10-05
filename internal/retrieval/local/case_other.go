//go:build !darwin

package local

// diskCase is p: elsewhere the file systems Flopwire runs on compare
// path case exactly, so two spellings are two directories.
func diskCase(p string) string { return p }
