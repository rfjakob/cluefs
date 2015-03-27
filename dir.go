package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"bazil.org/fuse"
	fusefs "bazil.org/fuse/fs"
	"golang.org/x/net/context"
)

var skipDirEntry func(n string) bool

func init() {
	switch runtime.GOOS {
	case "darwin":
		// On Darwin we skip all directory entries starting by '._'
		skipDirEntry = func(n string) bool {
			return strings.HasPrefix(n, "._")
		}
	default:
		skipDirEntry = func(n string) bool {
			return false
		}
	}
}

type Dir struct {
	*Node
	*Handle
	ProcessInfo
}

func (d Dir) String() string {
	return fmt.Sprintf("[%s %s %s]", d.Node, d.Handle, d.ProcessInfo)
}

func (d *Dir) SetProcessInfo(h fuse.Header) {
	d.ProcessInfo = ProcessInfo{Uid: h.Uid, Gid: h.Gid, Pid: h.Pid}
}

func NewDir(parent string, name string, fs *ClueFS) *Dir {
	return &Dir{
		Node:   NewNode(parent, name, fs),
		Handle: &Handle{},
	}
}

func (d *Dir) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fusefs.Handle, error) {
	defer trace(NewOpenOp(req, d.path))
	perm := os.FileMode(req.Flags).Perm()
	flags := int(req.Flags)
	newdir := NewDir(d.parent, d.name, d.fs)
	if err := newdir.doOpen(d.path, flags, perm); err != nil {
		return nil, err
	}
	newdir.SetProcessInfo(req.Header)
	resp.Handle = newdir.handleID
	return newdir, nil
}

func (d *Dir) Release(ctx context.Context, req *fuse.ReleaseRequest) error {
	if !d.isOpen() {
		return nil
	}
	defer trace(NewReleaseOp(req, d.path))
	if req.ReleaseFlags&fuse.ReleaseFlush != 0 {
		d.doSync()
	}
	return d.doClose()
}

func (d *Dir) Lookup(ctx context.Context, req *fuse.LookupRequest, resp *fuse.LookupResponse) (fusefs.Node, error) {
	if skipDirEntry(req.Name) {
		return nil, fuse.ENOENT
	}
	path := filepath.Join(d.path, req.Name)
	isDir := false
	defer trace(NewLookupOp(req, path, isDir))
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		return nil, fuse.ENOENT
	}
	resp.Attr = statToFuseAttr(st)
	resp.Node = fuse.NodeID(resp.Attr.Inode)
	resp.AttrValid = time.Duration(1) * time.Second
	resp.EntryValid = time.Duration(500) * time.Millisecond
	if isDir = resp.Attr.Mode.IsDir(); isDir {
		return NewDir(d.path, req.Name, d.fs), nil
	}
	return NewFile(d.path, req.Name, d.fs), nil
}

func (d *Dir) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	if !d.isOpen() {
		return nil, fuse.ENOTSUP
	}
	defer trace(NewReadDirOp(d.path, d.ProcessInfo))
	names, err := d.file.Readdirnames(0)
	if err != nil {
		return nil, fuse.EIO
	}
	result := make([]fuse.Dirent, 0, len(names)+2)
	for _, n := range names {
		if skipDirEntry(n) {
			continue
		}
		entry := getFuseDirent(filepath.Join(d.path, n), n)
		result = append(result, entry)
	}

	// Add '.' and '..' to the result
	dots := make([]fuse.Dirent, 2)
	dots[0] = getFuseDirent(d.path, ".")
	if len(d.parent) > 0 {
		dots[1] = getFuseDirent(d.parent, "..")
	} else {
		dots[1] = dots[0]
		dots[1].Name = ".."
	}
	return append(result, dots...), nil
}

func (d *Dir) Mkdir(ctx context.Context, req *fuse.MkdirRequest) (fusefs.Node, error) {
	path := filepath.Join(d.path, req.Name)
	defer trace(NewMkdirOp(req, path, req.Mode))
	if err := os.Mkdir(path, req.Mode); err != nil {
		return nil, osErrorToFuseError(err)
	}
	return NewDir(d.path, req.Name, d.fs), nil
}

func (d *Dir) Remove(ctx context.Context, req *fuse.RemoveRequest) error {
	path := filepath.Join(d.path, req.Name)
	defer trace(NewRemoveOp(req, path))
	if err := os.Remove(path); err != nil {
		return osErrorToFuseError(err)
	}
	return nil
}

func (d *Dir) Create(ctx context.Context, req *fuse.CreateRequest, resp *fuse.CreateResponse) (fusefs.Node, fusefs.Handle, error) {
	path := filepath.Join(d.path, req.Name)
	defer trace(NewCreateOp(req, path))
	f, err := os.OpenFile(path, int(req.Flags), req.Mode)
	if err != nil {
		return nil, nil, osErrorToFuseError(err)
	}
	newfile := NewOpenFile(d.path, req.Name, d.fs, f)
	return newfile, newfile, nil
}

func (d *Dir) Symlink(ctx context.Context, req *fuse.SymlinkRequest) (fusefs.Node, error) {
	absNewName := filepath.Join(d.path, req.NewName)
	targetIsDir := false
	defer trace(NewSymlinkOp(req, absNewName, req.Target, targetIsDir))

	// Make sure the target of the symbolic link we will create is kept
	// within the boundaries of the shadow file system. This is necessary
	// in order for the link not to be broken when ClueFS is unmounted

	// Replace this file system mount directory by the target directory
	// in the link target path. Do this only when the link target an
	// absolute path
	linkTarget := req.Target
	absTarget := linkTarget
	if !filepath.IsAbs(req.Target) {
		absTarget = filepath.Join(d.path, req.Target)
	}
	if strings.HasPrefix(absTarget, d.fs.mountDir) {
		absTarget = strings.Replace(absTarget, d.fs.mountDir, d.fs.shadowDir, 1)
		linkTarget = absTarget
	}

	// Does the link target actually exist?
	if info, err := os.Lstat(absTarget); err == nil {
		// The symbolic link target does exist
		targetIsDir = info.IsDir()
	}

	// Create the symbolic link: absNewName --> linkTarget
	if err := os.Symlink(linkTarget, absNewName); err != nil {
		return nil, osErrorToFuseError(err)
	}
	if targetIsDir {
		return NewDir(d.path, req.NewName, d.fs), nil
	}
	return NewFile(d.path, req.NewName, d.fs), nil
}

// osErrorToFuseError converts an os.PathError, os.LinkError or
// syscall.Errno into an error
func osErrorToFuseError(err error) error {
	if err == nil {
		return nil
	}
	errno := syscall.EIO
	if patherr, ok := err.(*os.PathError); ok {
		errno = patherr.Err.(syscall.Errno)
	} else if linkerr, ok := err.(*os.LinkError); ok {
		errno = linkerr.Err.(syscall.Errno)
	} else if _, ok := err.(*syscall.Errno); ok {
		errno = err.(syscall.Errno)
	}
	return fuse.Errno(errno)
}
