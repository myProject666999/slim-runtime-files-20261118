package fileset

import (
	"os"
	"syscall"
)

// kindOf maps an Lstat mode onto a Kind. Special-file bits are terminal kinds:
// nothing is ever read, so fifo/socket/device semantics are irrelevant here.
func kindOf(mode os.FileMode) Kind {
	switch {
	case mode&os.ModeSymlink != 0:
		return KindSymlink
	case mode&os.ModeDevice != 0: // covers both ModeDevice and ModeCharDevice
		return KindDevice
	case mode&os.ModeNamedPipe != 0:
		return KindNamedPipe
	case mode&os.ModeSocket != 0:
		return KindSocket
	case mode.IsDir():
		return KindDirectory
	case mode.IsRegular():
		return KindRegular
	default:
		return KindOther
	}
}

// lstatMeta builds a Meta from an os.FileInfo returned by os.Lstat. Every value is
// read from the name itself, so for a symlink it describes the link rather than its
// target. Uid/Gid/Inode come from a guarded *syscall.Stat_t assertion (present on
// all Unix builds); when Sys() is unavailable the identity fields stay at -1/0.
// atime/ctime are populated only where the platform exposes them (see fillSysTimes).
func lstatMeta(fi os.FileInfo) Meta {
	meta := Meta{
		Mode:  fi.Mode(),
		Size:  fi.Size(),
		Uid:   -1,
		Gid:   -1,
		Mtime: fi.ModTime(),
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st != nil {
		meta.Uid = int32(st.Uid)
		meta.Gid = int32(st.Gid)
		meta.Inode = st.Ino
		fillSysTimes(&meta, st)
	}
	return meta
}
