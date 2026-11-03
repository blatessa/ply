package workspace

import "golang.org/x/sys/unix"

// CloneFile copies src to a new file dst, using APFS clonefile so the copy
// shares storage until either changes.
func CloneFile(src, dst string) error {
	if err := unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW); err == nil {
		return nil
	}
	return copyFile(src, dst)
}
