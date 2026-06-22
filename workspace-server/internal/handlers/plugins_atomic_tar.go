package handlers

// plugins_atomic_tar.go — tar-walk helpers split out so the main atomic
// install flow stays readable. The prefix argument lets the caller
// arrange where the tar's contents land at extract time.

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
)

// newTarWriter is a thin wrapper so atomic_test.go can swap the writer
// destination if it needs to.
func newTarWriter(w io.Writer) *tar.Writer {
	return tar.NewWriter(w)
}

// tarWalk walks hostDir and writes every regular file + dir to the tar
// writer with paths of the form `<prefix>/<relative>`. Symlinks are
// skipped — same posture as streamDirAsTar in plugins_install_pipeline.go.
//
// The trailing-slash on prefix is normalized away: prefix "foo" and
// prefix "foo/" produce identical archives.
func tarWalk(hostDir, prefix string, tw *tar.Writer) error {
	prefix = filepath.Clean(prefix)
	return filepath.Walk(hostDir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil // skip symlinks; see doc above
		}
		rel, err := filepath.Rel(hostDir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			// Emit the prefix dir itself once, with the source dir's mode.
			hdr, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			hdr.Name = prefix + "/"
			return tw.WriteHeader(hdr)
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.Join(prefix, rel)
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
}
