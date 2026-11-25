package server

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	pgdb "github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/storage"
)

const archiveBlobPrefix = "cold-archives/"

func taskArchiveBlobKey(job *pgdb.TaskArchive) string {
	return fmt.Sprintf("%stask-%d-%d.tar.zst", archiveBlobPrefix, job.TaskID, job.ID)
}

func (s *Server) archiveStoreFor(job *pgdb.TaskArchive) (storage.StreamBlobStore, error) {
	if !strings.HasPrefix(job.ArchivePath, archiveBlobPrefix) {
		return nil, validateArchivePath(s.m.dir, job.ArchivePath)
	}
	if s.ent == nil || job.ArchivePath != taskArchiveBlobKey(job) {
		return nil, errors.New("归档包对象键不属于当前任务的受限库")
	}
	return s.ent.BlobStore(), nil
}

func (s *Server) putTaskArchiveBlob(job *pgdb.TaskArchive, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	putErr := s.ent.BlobStore().PutReader(s.ctx, taskArchiveBlobKey(job), file)
	return errors.Join(putErr, file.Close())
}

func (s *Server) extractTaskArchive(job *pgdb.TaskArchive, store storage.StreamBlobStore, dir string) (err error) {
	if store == nil {
		return extractTaskArchivePackage(job.ArchivePath, job.SHA256, dir)
	}
	body, err := store.Open(s.ctx, job.ArchivePath)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".package-")
	if err != nil {
		return errors.Join(err, body.Close())
	}
	defer func() {
		err = errors.Join(err, file.Close(), os.Remove(file.Name()))
	}()
	size, copyErr := io.Copy(file, body)
	if err := errors.Join(copyErr, body.Close()); err != nil {
		return err
	}
	if size != job.CompressedSize {
		return errors.New("归档包大小与记录不一致")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return extractTaskArchiveReader(file, job.SHA256, dir)
}

func (s *Server) consumeTaskArchive(job *pgdb.TaskArchive) error {
	store, err := s.archiveStoreFor(job)
	if err != nil {
		return err
	}
	if store != nil {
		return store.Delete(s.ctx, job.ArchivePath)
	}
	if err := os.Remove(job.ArchivePath); !os.IsNotExist(err) {
		return err
	}
	return nil
}
