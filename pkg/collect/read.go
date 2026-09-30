package collect

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// Size limits for metadata reads.
const (
	maxMetaBytes   = 1 << 20
	maxPackedBytes = 64 << 20
)

var errNotRegular = errors.New("not a regular file")

// readMeta reads a metadata file of at most limit bytes. The file must be a
// regular file when stat'ed, so a FIFO or device never blocks the read;
// noSymlink additionally refuses a symlink (os.Lstat).
func readMeta(path string, limit int64, noSymlink bool) ([]byte, error) {
	stat := os.Stat
	if noSymlink {
		stat = os.Lstat
	}
	info, err := stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: %w", path, errNotRegular)
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("%s: larger than %d bytes", path, limit)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s: larger than %d bytes", path, limit)
	}

	return data, nil
}
