package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// resolveApp turns the -app argument into a bundle directory: a directory as
// it stands, or the folder an archive unpacks to. The returned cleanup is
// never nil, so it is safe to defer before checking err.
func resolveApp(appPath string) (string, func(), error) {
	noop := func() {}
	info, err := os.Stat(appPath)
	if err != nil {
		return "", noop, err
	}
	if info.IsDir() {
		return appPath, noop, nil
	}
	dir, err := os.MkdirTemp("", "dmg-packer-unpack-")
	if err != nil {
		return "", noop, err
	}
	cleanup := func() { os.RemoveAll(dir) }
	if err := unpack(appPath, dir); err != nil {
		cleanup()
		return "", noop, fmt.Errorf("%s: %w", appPath, err)
	}
	bundle, err := bundleFolder(dir)
	if err != nil {
		cleanup()
		return "", noop, fmt.Errorf("%s: %w", appPath, err)
	}
	return bundle, cleanup, nil
}

// bundleFolder returns the one folder the archive unpacked to. Loose files
// beside it, like a README, are ignored.
func bundleFolder(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	folders := slices.DeleteFunc(entries, func(e fs.DirEntry) bool { return !e.IsDir() })
	if len(folders) != 1 {
		return "", fmt.Errorf("expected one folder in the archive, found %d", len(folders))
	}
	// A name check, so a wrapped-up archive fails here rather than packing
	// the wrapper as if it were the bundle.
	if name := folders[0].Name(); filepath.Ext(name) != ".app" {
		return "", fmt.Errorf("the archive holds %q, not a `.app` bundle", name)
	}
	return filepath.Join(dir, folders[0].Name()), nil
}

// unpack extracts the archive at src into the existing folder dst. The
// format comes from the file name: `.zip`, `.tar`, `.tar.gz` or `.tgz`.
func unpack(src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	switch name := strings.ToLower(src); {
	case strings.HasSuffix(name, ".zip"):
		info, err := f.Stat()
		if err != nil {
			return err
		}
		return extractZip(f, info.Size(), dst)
	case strings.HasSuffix(name, ".tar"):
		return extractTar(f, dst)
	case strings.HasSuffix(name, ".tar.gz"), strings.HasSuffix(name, ".tgz"):
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gz.Close()
		return extractTar(gz, dst)
	default:
		return errors.New("expected a folder, or a .zip, .tar, .tar.gz or .tgz archive")
	}
}

func extractZip(r io.ReaderAt, size int64, dst string) error {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return err
	}
	var links []link
	for _, entry := range zr.File {
		if skipEntry(entry.Name) {
			continue
		}
		target, err := dest(dst, entry.Name)
		if err != nil {
			return err
		}
		body, err := entry.Open()
		if err != nil {
			return err
		}
		mode := entry.Mode()
		switch {
		case mode&fs.ModeSymlink != 0:
			var to []byte
			to, err = io.ReadAll(body)
			links = append(links, link{target, string(to)})
		case entry.FileInfo().IsDir():
			err = os.MkdirAll(target, 0o755)
		case mode.IsRegular():
			err = writeFile(body, target, filePerm(mode.Perm(), entry.Name))
		default:
			err = fmt.Errorf("%s is a %s, which cannot go in a bundle", entry.Name, mode.Type())
		}
		if err = errors.Join(err, body.Close()); err != nil {
			return err
		}
	}
	return createLinks(links)
}

func extractTar(r io.Reader, dst string) error {
	tr := tar.NewReader(r)
	var links []link
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return createLinks(links)
		}
		if err != nil {
			return err
		}
		if skipEntry(header.Name) {
			continue
		}
		target, err := dest(dst, header.Name)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			err = os.MkdirAll(target, 0o755)
		case tar.TypeReg:
			err = writeFile(tr, target, filePerm(header.FileInfo().Mode().Perm(), header.Name))
		case tar.TypeSymlink:
			links = append(links, link{target, header.Linkname})
		case tar.TypeLink:
			var source string
			if source, err = dest(dst, header.Linkname); err == nil {
				err = os.Link(source, target)
			}
		default:
			err = fmt.Errorf("%s is a tar type %q entry, which cannot go in a bundle", header.Name, header.Typeflag)
		}
		if err != nil {
			return err
		}
	}
}

// link is a symlink waiting for the rest of the archive. They go in last so
// that no earlier entry can be written through one, which is how an archive
// escapes its destination: `link -> /etc` followed by `link/passwd`.
type link struct{ dest, target string }

func createLinks(links []link) error {
	for _, l := range links {
		if err := os.Symlink(l.target, l.dest); err != nil {
			return err
		}
	}
	return nil
}

// dest joins an archive entry name onto dst and creates its parent folders.
// A `..` element is refused; a leading `/` is dropped, as `tar` does.
func dest(dst, name string) (string, error) {
	rel := path.Clean(strings.TrimLeft(filepath.ToSlash(name), "/"))
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("archive entry %q points outside the archive", name)
	}
	target := filepath.Join(dst, rel)
	return target, os.MkdirAll(filepath.Dir(target), 0o755)
}

func writeFile(r io.Reader, target string, perm fs.FileMode) error {
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r)
	return errors.Join(err, f.Close())
}

// filePerm keeps the mode the archive recorded, except in `Contents/MacOS`,
// which has to be executable for the bundle to run: a zip written on Windows
// records no execute bit at all.
func filePerm(perm fs.FileMode, name string) fs.FileMode {
	switch {
	case strings.Contains(filepath.ToSlash(name), "Contents/MacOS/"):
		return 0o755
	case perm == 0:
		return 0o644
	default:
		return perm
	}
}

// skipEntry drops archiver bookkeeping: both spellings of AppleDouble
// (`__MACOSX/` from zips, `._name` from `tar`), which carry the xattrs and
// resource forks the writer does not store, and tar's pax header.
func skipEntry(name string) bool {
	rel := path.Clean(strings.TrimLeft(filepath.ToSlash(name), "/"))
	return rel == "pax_global_header" || rel == "__MACOSX" ||
		strings.HasPrefix(rel, "__MACOSX/") || strings.HasPrefix(path.Base(rel), "._")
}
