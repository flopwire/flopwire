//go:build !darwin && !linux

package cowork

import (
	"io/fs"
	"os"
)

// OpenFile fails closed on platforms without the verified descriptor-relative
// opener. There is no default Cowork collection root on these platforms.
func OpenFile(root, path string) (*os.File, error) {
	return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrPermission}
}
