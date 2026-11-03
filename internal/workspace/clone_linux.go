package workspace

import (
	"os"

	"golang.org/x/sys/unix"
)

// CloneFile copies src to a new file dst, sharing storage (reflink) when the
// filesystem supports it, as btrfs and XFS do.
func CloneFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if err := unix.IoctlFileClone(int(out.Fd()), int(in.Fd())); err == nil {
		return out.Close()
	}
	out.Close()
	os.Remove(dst)
	return copyFile(src, dst)
}
