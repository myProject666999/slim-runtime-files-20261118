//go:build !linux

package fileset

import "syscall"

// fillSysTimes is a no-op off Linux: the portable mtime was already set from
// os.FileInfo.ModTime in lstatMeta, and ctime remains zero. Splitting this out
// keeps the package compiling on every platform without referencing field names
// (Ctimespec vs Ctim) that differ across operating systems.
func fillSysTimes(_ *Meta, _ *syscall.Stat_t) {}
