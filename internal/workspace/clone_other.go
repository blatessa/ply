//go:build !linux && !darwin

package workspace

// CloneFile copies src to a new file dst.
func CloneFile(src, dst string) error { return copyFile(src, dst) }
