package local

import (
	"context"
	"io"
	"os"
	"path/filepath"

	"github.com/restic/restic/internal/errors"
	"github.com/restic/restic/internal/restic"

	"github.com/restic/restic/internal/backend"
	"github.com/restic/restic/internal/debug"
	"github.com/restic/restic/internal/fs"
)

// Local is a backend in a local directory.
type Local struct {
	Config
	backend.Layout
}

// ensure statically that *Local implements restic.Backend.
var _ restic.Backend = &Local{}

const defaultLayout = "default"

// dirExists returns true if the name exists and is a directory.
func dirExists(name string) bool {
	f, err := fs.Open(name)
	if err != nil {
		return false
	}

	fi, err := f.Stat()
	if err != nil {
		return false
	}

	if err = f.Close(); err != nil {
		return false
	}

	return fi.IsDir()
}

// Open opens the local backend as specified by config.
func Open(cfg Config) (*Local, error) {
	debug.Log("open local backend at %v (layout %q)", cfg.Path, cfg.Layout)
	l, err := backend.ParseLayout(&backend.LocalFilesystem{}, cfg.Layout, defaultLayout, cfg.Path)
	if err != nil {
		return nil, err
	}

	be := &Local{Config: cfg, Layout: l}

	// if data dir exists, make sure that all subdirs also exist
	datadir := be.Dirname(restic.Handle{Type: restic.DataFile})
	if dirExists(datadir) {
		debug.Log("datadir %v exists", datadir)
		for _, d := range be.Paths() {
			if !fs.HasPathPrefix(datadir, d) {
				debug.Log("%v is not subdir of datadir %v", d, datadir)
				continue
			}

			debug.Log("MkdirAll %v", d)
			err := fs.MkdirAll(d, backend.Modes.Dir)
			if err != nil {
				return nil, errors.Wrap(err, "MkdirAll")
			}
		}
	}

	return be, nil
}

// Create creates all the necessary files and directories for a new local
// backend at dir. Afterwards a new config blob should be created.
func Create(cfg Config) (*Local, error) {
	debug.Log("create local backend at %v (layout %q)", cfg.Path, cfg.Layout)

	l, err := backend.ParseLayout(&backend.LocalFilesystem{}, cfg.Layout, defaultLayout, cfg.Path)
	if err != nil {
		return nil, err
	}

	be := &Local{
		Config: cfg,
		Layout: l,
	}

	// test if config file already exists
	_, err = fs.Lstat(be.Filename(restic.Handle{Type: restic.ConfigFile}))
	if err == nil {
		return nil, errors.New("config file already exists")
	}

	// create paths for data and refs
	for _, d := range be.Paths() {
		err := fs.MkdirAll(d, backend.Modes.Dir)
		if err != nil {
			return nil, errors.Wrap(err, "MkdirAll")
		}
	}

	return be, nil
}

// Location returns this backend's location (the directory name).
func (b *Local) Location() string {
	return b.Path
}

// IsNotExist returns true if the error is caused by a non existing file.
func (b *Local) IsNotExist(err error) bool {
	return os.IsNotExist(errors.Cause(err))
}

// Save stores data in the backend at the handle.
func (b *Local) Save(ctx context.Context, h restic.Handle, rd io.Reader) (err error) {
	debug.Log("Save %v", h)
	if err := h.Valid(); err != nil {
		return err
	}

	if h.Type == restic.LockFile {
		lockDir := b.Dirname(h)
		if !dirExists(lockDir) {
			debug.Log("locks/ does not exist yet, creating now.")
			if err := fs.MkdirAll(lockDir, backend.Modes.Dir); err != nil {
				return errors.Wrap(err, "MkdirAll")
			}
		}
	}

	filename := b.Filename(h)

	// create new file
	f, err := fs.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, backend.Modes.File)
	if err != nil {
		return errors.Wrap(err, "OpenFile")
	}

	// save data, then sync
	_, err = io.Copy(f, rd)
	if err != nil {
		_ = f.Close()
		return errors.Wrap(err, "Write")
	}

	if err = f.Sync(); err != nil {
		_ = f.Close()
		return errors.Wrap(err, "Sync")
	}

	err = f.Close()
	if err != nil {
		return errors.Wrap(err, "Close")
	}

	return setNewFileMode(filename, backend.Modes.File)
}

// Load returns a reader that yields the contents of the file at h at the
// given offset. If length is nonzero, only a portion of the file is
// returned. rd must be closed after use.
func (b *Local) Load(ctx context.Context, h restic.Handle, length int, offset int64) (io.ReadCloser, error) {
	debug.Log("Load %v, length %v, offset %v", h, length, offset)
	if err := h.Valid(); err != nil {
		return nil, err
	}

	if offset < 0 {
		return nil, errors.New("offset is negative")
	}

	f, err := fs.Open(b.Filename(h))
	if err != nil {
		return nil, err
	}

	if offset > 0 {
		_, err = f.Seek(offset, 0)
		if err != nil {
			_ = f.Close()
			return nil, err
		}
	}

	if length > 0 {
		return backend.LimitReadCloser(f, int64(length)), nil
	}

	return f, nil
}

// Stat returns information about a blob.
func (b *Local) Stat(ctx context.Context, h restic.Handle) (restic.FileInfo, error) {
	debug.Log("Stat %v", h)
	if err := h.Valid(); err != nil {
		return restic.FileInfo{}, err
	}

	fi, err := fs.Stat(b.Filename(h))
	if err != nil {
		return restic.FileInfo{}, errors.Wrap(err, "Stat")
	}

	return restic.FileInfo{Size: fi.Size()}, nil
}

// Test returns true if a blob of the given type and name exists in the backend.
func (b *Local) Test(ctx context.Context, h restic.Handle) (bool, error) {
	debug.Log("Test %v", h)
	_, err := fs.Stat(b.Filename(h))
	if err != nil {
		if os.IsNotExist(errors.Cause(err)) {
			return false, nil
		}
		return false, errors.Wrap(err, "Stat")
	}

	return true, nil
}

// Remove removes the blob with the given name and type.
func (b *Local) Remove(ctx context.Context, h restic.Handle) error {
	debug.Log("Remove %v", h)
	fn := b.Filename(h)

	// reset read-only flag
	err := fs.Chmod(fn, 0666)
	if err != nil {
		return errors.Wrap(err, "Chmod")
	}

	return fs.Remove(fn)
}

func isFile(fi os.FileInfo) bool {
	return fi.Mode()&(os.ModeType|os.ModeCharDevice) == 0
}

// List returns a channel that yields all names of blobs of type t. A
// goroutine is started for this.
func (b *Local) List(ctx context.Context, t restic.FileType) <-chan string {
	debug.Log("List %v", t)

	ch := make(chan string)

	go func() {
		defer close(ch)

		fs.Walk(b.Basedir(t), func(path string, fi os.FileInfo, err error) error {
			if err != nil {
				return err
			}

			if !isFile(fi) {
				return err
			}

			select {
			case ch <- filepath.Base(path):
			case <-ctx.Done():
				return err
			}

			return err
		})
	}()

	return ch
}

// Delete removes the repository and all files.
func (b *Local) Delete() error {
	debug.Log("Delete()")
	return fs.RemoveAll(b.Path)
}

// Close closes all open files.
func (b *Local) Close() error {
	debug.Log("Close()")
	// this does not need to do anything, all open files are closed within the
	// same function.
	return nil
}