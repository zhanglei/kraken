package tagstore

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"code.uber.internal/infra/kraken/core"
	"code.uber.internal/infra/kraken/lib/backend"
	"code.uber.internal/infra/kraken/lib/backend/backenderrors"
	"code.uber.internal/infra/kraken/lib/persistedretry"
	"code.uber.internal/infra/kraken/lib/persistedretry/writeback"
	"code.uber.internal/infra/kraken/lib/store"
	"code.uber.internal/infra/kraken/lib/store/metadata"
	"code.uber.internal/infra/kraken/utils/log"

	"github.com/uber-go/tally"
)

// Store errors.
var (
	ErrTagNotFound = errors.New("tag not found")
)

// FileStore defines operations required for storing tags on disk.
type FileStore interface {
	CreateCacheFile(name string, r io.Reader) error
	SetCacheFileMetadata(name string, md metadata.Metadata) (bool, error)
	GetCacheFileReader(name string) (store.FileReader, error)
}

// Store defines tag storage operations.
type Store interface {
	Put(tag string, d core.Digest, writeBackDelay time.Duration) error
	Get(tag string) (core.Digest, error)
}

// tagStore encapsulates two-level tag storage:
// 1. On-disk file store: persists tags for availability / write-back purposes.
// 2. Remote storage: durable tag storage.
type tagStore struct {
	fs               FileStore
	backends         *backend.Manager
	writeBackManager persistedretry.Manager
}

// New creates a new Store.
func New(
	stats tally.Scope,
	fs FileStore,
	backends *backend.Manager,
	writeBackManager persistedretry.Manager) Store {

	stats = stats.Tagged(map[string]string{
		"module": "tagstore",
	})

	return &tagStore{
		fs:               fs,
		backends:         backends,
		writeBackManager: writeBackManager,
	}
}

func (s *tagStore) Put(tag string, d core.Digest, writeBackDelay time.Duration) error {
	if err := s.writeTagToDisk(tag, d); err != nil {
		return fmt.Errorf("write tag to disk: %s", err)
	}
	if _, err := s.fs.SetCacheFileMetadata(tag, metadata.NewPersist(true)); err != nil {
		return fmt.Errorf("set persist metadata: %s", err)
	}
	task := writeback.NewTaskWithDelay(tag, tag, writeBackDelay)
	if err := s.writeBackManager.Add(task); err != nil {
		return fmt.Errorf("add write-back task: %s", err)
	}
	return nil
}

func (s *tagStore) Get(tag string) (d core.Digest, err error) {
	for _, resolve := range []func(tag string) (core.Digest, error){
		s.resolveFromDisk,
		s.resolveFromBackend,
	} {
		d, err = resolve(tag)
		if err == ErrTagNotFound {
			continue
		}
		break
	}
	return d, err
}

func (s *tagStore) writeTagToDisk(tag string, d core.Digest) error {
	buf := bytes.NewBufferString(d.String())
	if err := s.fs.CreateCacheFile(tag, buf); err != nil && !os.IsExist(err) {
		return err
	}
	return nil
}

func (s *tagStore) resolveFromDisk(tag string) (core.Digest, error) {
	f, err := s.fs.GetCacheFileReader(tag)
	if err != nil {
		if os.IsNotExist(err) {
			return core.Digest{}, ErrTagNotFound
		}
		return core.Digest{}, fmt.Errorf("fs: %s", err)
	}
	defer f.Close()
	var b bytes.Buffer
	if _, err := io.Copy(&b, f); err != nil {
		return core.Digest{}, fmt.Errorf("copy from fs: %s", err)
	}
	d, err := core.ParseSHA256Digest(b.String())
	if err != nil {
		return core.Digest{}, fmt.Errorf("parse fs digest: %s", err)
	}
	return d, nil
}

func (s *tagStore) resolveFromBackend(tag string) (core.Digest, error) {
	backendClient, err := s.backends.GetClient(tag)
	if err != nil {
		return core.Digest{}, fmt.Errorf("backend manager: %s", err)
	}
	var b bytes.Buffer
	if err := backendClient.Download(tag, &b); err != nil {
		if err == backenderrors.ErrBlobNotFound {
			return core.Digest{}, ErrTagNotFound
		}
		return core.Digest{}, fmt.Errorf("backend client: %s", err)
	}
	d, err := core.ParseSHA256Digest(b.String())
	if err != nil {
		return core.Digest{}, fmt.Errorf("parse backend digest: %s", err)
	}
	if err := s.writeTagToDisk(tag, d); err != nil {
		log.With("tag", tag).Errorf("Error writing tag to disk: %s", err)
	}
	return d, nil
}
