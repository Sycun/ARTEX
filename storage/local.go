package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type LocalFSStore struct {
	dir  string
	info fs.FileInfo
}

func NewLocalFSStore(dir string) (*LocalFSStore, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("blob root %q is not a directory: %w", dir, fs.ErrInvalid)
	}
	return &LocalFSStore{dir: dir, info: info}, nil
}

func (s *LocalFSStore) open(ctx context.Context, key string, createParents bool) (*os.Root, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if err := validateKey(key); err != nil {
		return nil, "", err
	}
	name, err := filepath.Localize(key)
	if err != nil {
		return nil, "", err
	}
	info, err := os.Lstat(s.dir)
	if err != nil {
		return nil, "", err
	}
	if !os.SameFile(s.info, info) {
		return nil, "", fmt.Errorf("blob root %q has changed: %w", s.dir, fs.ErrInvalid)
	}
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return nil, "", err
	}
	info, err = root.Stat(".")
	if err == nil && !os.SameFile(s.info, info) {
		err = fmt.Errorf("blob root %q has changed: %w", s.dir, fs.ErrInvalid)
	}
	if err == nil {
		err = checkLocalPath(root, name, createParents)
	}
	if err != nil {
		return nil, "", errors.Join(err, root.Close())
	}
	return root, name, nil
}

// Lstat rejects links even when they point inside the store. os.Root also
// confines the filesystem operation if an entry changes after this check.
func checkLocalPath(root *os.Root, name string, createParents bool) error {
	parts := strings.Split(name, string(filepath.Separator))
	for i := range parts {
		path := filepath.Join(parts[:i+1]...)
		last := i == len(parts)-1
		info, err := root.Lstat(path)
		if createParents && errors.Is(err, fs.ErrNotExist) {
			if last {
				return nil
			}
			if err = root.Mkdir(path, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			info, err = root.Lstat(path)
		}
		if err != nil {
			return err
		}
		if (last && !info.Mode().IsRegular()) || (!last && !info.IsDir()) {
			return fmt.Errorf("invalid blob path %q: %w", path, fs.ErrInvalid)
		}
	}
	return nil
}

func (s *LocalFSStore) Put(ctx context.Context, key string, data []byte) error {
	return s.PutReader(ctx, key, bytes.NewReader(data))
}

func (s *LocalFSStore) PutReader(ctx context.Context, key string, data io.ReadSeeker) (err error) {
	root, name, err := s.open(ctx, key, true)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	temp := filepath.Join(filepath.Dir(name), ".blob-"+rand.Text())
	f, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, root.Remove(temp))
		}
	}()
	_, writeErr := io.Copy(f, contextReader{ctx, data})
	var syncErr error
	if writeErr == nil {
		syncErr = f.Sync()
	}
	if err := errors.Join(writeErr, syncErr, f.Close(), ctx.Err()); err != nil {
		return err
	}
	return root.Rename(temp, name)
}

func (s *LocalFSStore) Get(ctx context.Context, key string) (_ []byte, err error) {
	root, name, err := s.open(ctx, key, false)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	return root.ReadFile(name)
}

func (s *LocalFSStore) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	root, name, err := s.open(ctx, key, false)
	if err != nil {
		return nil, err
	}
	file, openErr := root.Open(name)
	if err := errors.Join(openErr, root.Close()); err != nil {
		if openErr == nil {
			err = errors.Join(err, file.Close())
		}
		return nil, err
	}
	return file, nil
}

func (s *LocalFSStore) Delete(ctx context.Context, key string) (err error) {
	root, name, err := s.open(ctx, key, false)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	if err := root.Remove(name); !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (s *LocalFSStore) Stat(ctx context.Context, key string) (_ BlobInfo, err error) {
	root, name, err := s.open(ctx, key, false)
	if err != nil {
		return BlobInfo{}, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	info, err := root.Stat(name)
	if err != nil {
		return BlobInfo{}, err
	}
	return BlobInfo{Size: info.Size(), ModTime: info.ModTime()}, nil
}
