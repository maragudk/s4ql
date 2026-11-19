//go:build s4ql_browser

package simulator

import (
	"sync"

	"maragu.dev/s4ql/internal/sqlite/faultvfs"
)

// disk of an instance: a VFS of faultvfs through which its processes open their caches, so that the
// action failDisk can make every file operation of them fail. Nothing fails until it does.
type disk struct {
	v *faultvfs.VFS
}

func (d disk) name() string {
	return d.v.Name()
}

// fail every operation on every file of the disk from now on, or none.
func (d disk) fail(on bool) {
	if on {
		d.v.Fail(faultvfs.Fault{Ops: faultvfs.AnyOp, Files: faultvfs.AnyFile})
		return
	}
	d.v.Fail(faultvfs.Fault{})
}

// disks that no run uses. A VFS is never closed, because a crashed process leaves its connections
// open, and a VFS cannot be closed while a file that was opened through it is open. So a run takes
// its disks from here, and puts them back when it has ended.
var disks struct {
	mu   sync.Mutex
	free []*faultvfs.VFS
}

// takeDisks for n instances, each of which fails nothing.
func takeDisks(n int) ([]disk, error) {
	disks.mu.Lock()
	defer disks.mu.Unlock()
	var out []disk
	for range n {
		var v *faultvfs.VFS
		if len(disks.free) > 0 {
			v, disks.free = disks.free[len(disks.free)-1], disks.free[:len(disks.free)-1]
		} else {
			var err error
			if v, err = faultvfs.New(); err != nil {
				disks.free = append(disks.free, vfsOf(out)...)
				return nil, err
			}
		}
		d := disk{v: v}
		d.fail(false)
		out = append(out, d)
	}
	return out, nil
}

// putDisks back, for later runs.
func putDisks(ds []disk) {
	disks.mu.Lock()
	defer disks.mu.Unlock()
	for _, d := range ds {
		d.fail(false)
	}
	disks.free = append(disks.free, vfsOf(ds)...)
}

func vfsOf(ds []disk) []*faultvfs.VFS {
	var out []*faultvfs.VFS
	for _, d := range ds {
		out = append(out, d.v)
	}
	return out
}
