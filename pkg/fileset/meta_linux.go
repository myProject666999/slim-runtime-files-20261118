//go:build linux

package fileset

import (
	"syscall"
	"time"
)

// fillSysTimes populates mtime/ctime from the Linux stat structure. Linux is the
// only supported runtime target, so full timestamp fidelity is provided here. Atime
// is excluded by design (see Meta): it is mutated by reads and thus nondeterministic.
func fillSysTimes(meta *Meta, st *syscall.Stat_t) {
	meta.Mtime = time.Unix(st.Mtim.Sec, st.Mtim.Nsec)
	meta.Ctime = time.Unix(st.Ctim.Sec, st.Ctim.Nsec)
}
