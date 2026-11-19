package backup

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"filippo.io/age"

	"nori/internal/store"
)

const manifestName = "manifest.json"
const maxManifest = 16 << 20
const maxArchiveBytes int64 = 1 << 40
const maxPayloads = 256

var payloadName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,80}$`)

type Manifest struct {
	Version  int
	Service  store.Service
	Policy   store.BackupPolicy
	Env      string
	Digest   string
	Files    map[string]string
	Captured map[string]time.Time
	Postgres *PostgresInfo
}

func Encode(dst io.Writer, recipient age.Recipient, manifest Manifest, dir string) error {
	encrypted, err := age.Encrypt(dst, recipient)
	if err != nil {
		return err
	}
	compressed := gzip.NewWriter(encrypted)
	archive := tar.NewWriter(compressed)
	manifest.Version = 1
	manifest.Files = make(map[string]string)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) > maxPayloads {
		return errors.New("too many backup payloads")
	}
	var total int64
	for _, entry := range entries {
		name := entry.Name()
		if !payloadName.MatchString(name) {
			return errors.New("invalid backup payload name")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() > maxArchiveBytes-total {
			return errors.New("backup exceeds the archive size limit")
		}
		total += info.Size()
		if !info.Mode().IsRegular() {
			return errors.New("backup payload is not a regular file")
		}
		if err := archive.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: info.Size()}); err != nil {
			return err
		}
		file, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, err = io.Copy(io.MultiWriter(archive, hash), file)
		if err := errors.Join(err, file.Close()); err != nil {
			return err
		}
		manifest.Files[name] = hex.EncodeToString(hash.Sum(nil))
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if len(data) > maxManifest || int64(len(data)) > maxArchiveBytes-total {
		return errors.New("backup manifest is too large")
	}
	if err := archive.WriteHeader(&tar.Header{Name: manifestName, Mode: 0600, Size: int64(len(data))}); err != nil {
		return err
	}
	if _, err := archive.Write(data); err != nil {
		return err
	}
	if err := archive.Close(); err != nil {
		return err
	}
	if err := compressed.Close(); err != nil {
		return err
	}
	return encrypted.Close()
}

func Decode(src io.Reader, identity age.Identity, dir string, limit int64) (Manifest, error) {
	return decode(src, identity, dir, limit, func() error { return nil })
}

func decode(src io.Reader, identity age.Identity, dir string, limit int64, checkSpace func() error) (Manifest, error) {
	var manifest Manifest
	decrypted, err := age.Decrypt(src, identity)
	if err != nil {
		return manifest, err
	}
	compressed, err := gzip.NewReader(decrypted)
	if err != nil {
		return manifest, err
	}
	defer compressed.Close()
	archive := tar.NewReader(compressed)
	files := make(map[string]string)
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return manifest, err
		}
		if manifest.Version != 0 || header.Typeflag != tar.TypeReg || header.Size > limit {
			return manifest, errors.New("invalid backup archive entry or size limit exceeded")
		}
		limit -= header.Size
		if header.Name == manifestName {
			if header.Size > maxManifest {
				return manifest, errors.New("backup manifest is too large")
			}
			data, err := io.ReadAll(archive)
			if err != nil {
				return manifest, err
			}
			if err := json.Unmarshal(data, &manifest); err != nil || manifest.Version != 1 {
				return manifest, errors.New("unsupported backup manifest")
			}
			continue
		}
		if !payloadName.MatchString(header.Name) || len(files) >= maxPayloads {
			return manifest, errors.New("invalid backup payload name")
		}
		file, err := os.OpenFile(filepath.Join(dir, header.Name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return manifest, err
		}
		hash := sha256.New()
		_, err = io.Copy(checkedWriter{writer: io.MultiWriter(file, hash), check: checkSpace}, archive)
		if err := errors.Join(err, file.Close()); err != nil {
			return manifest, err
		}
		files[header.Name] = hex.EncodeToString(hash.Sum(nil))
	}
	// Drain through the gzip checksum and age final authentication tag.
	if n, err := io.Copy(io.Discard, io.LimitReader(compressed, 1)); err != nil || n != 0 {
		return manifest, errors.New("invalid backup trailer")
	}
	if manifest.Version != 1 || len(files) != len(manifest.Files) {
		return manifest, errors.New("backup manifest does not match payloads")
	}
	for name, hash := range files {
		if manifest.Files[name] != hash {
			return manifest, fmt.Errorf("invalid checksum for payload %s", name)
		}
	}
	return manifest, nil
}

type checkedWriter struct {
	writer io.Writer
	check  func() error
}

func (w checkedWriter) Write(p []byte) (int, error) {
	if err := w.check(); err != nil {
		return 0, err
	}
	return w.writer.Write(p)
}
