package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// entry describes one archive member for the builders below.
type entry struct {
	name    string
	body    string
	mode    fs.FileMode
	link    string
	dir     bool
	nomodes bool // stored with no unix mode bits, as a Windows zip does
}

func buildZip(t *testing.T, entries []entry) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		header := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		switch {
		case e.dir:
			header.Name += "/"
			header.SetMode(e.mode | fs.ModeDir)
		case e.link != "":
			header.SetMode(e.mode | fs.ModeSymlink)
		case e.nomodes: // CreatorVersion stays 0 (FAT), so no unix bits
		default:
			header.SetMode(e.mode)
		}
		w, err := zw.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		body := e.body
		if e.link != "" {
			body = e.link
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return writeTemp(t, "archive.zip", buf.Bytes())
}

func buildTar(t *testing.T, entries []entry, compress bool) string {
	t.Helper()
	var buf bytes.Buffer
	var body io.Writer = &buf
	var gz *gzip.Writer
	if compress {
		gz = gzip.NewWriter(&buf)
		body = gz
	}
	tw := tar.NewWriter(body)
	for _, e := range entries {
		header := &tar.Header{Name: e.name, Mode: int64(e.mode.Perm())}
		switch {
		case e.dir:
			header.Typeflag, header.Name = tar.TypeDir, e.name+"/"
		case e.link != "":
			header.Typeflag, header.Linkname = tar.TypeSymlink, e.link
		default:
			header.Typeflag, header.Size = tar.TypeReg, int64(len(e.body))
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			if _, err := io.WriteString(tw, e.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if gz != nil {
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
	}
	name := "archive.tar"
	if compress {
		name = "archive.tar.gz"
	}
	return writeTemp(t, name, buf.Bytes())
}

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// wantPerm checks the unix mode bits an entry was unpacked with. Windows
// records none of them — a writable file stats as 0666 whatever the archive
// said — so there the check is only that the file arrived.
func wantPerm(t *testing.T, path string, want fs.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	if perm := info.Mode().Perm(); perm != want {
		t.Errorf("%s is %v, want %v", filepath.Base(path), perm, want)
	}
}

// bundle is the entry set every format test unpacks.
func bundle() []entry {
	return []entry{
		{name: "Demo.app", dir: true, mode: 0o755},
		{name: "Demo.app/Contents", dir: true, mode: 0o755},
		{name: "Demo.app/Contents/Info.plist", body: "plist", mode: 0o644},
		{name: "Demo.app/Contents/MacOS", dir: true, mode: 0o755},
		{name: "Demo.app/Contents/MacOS/Demo", body: "#!/bin/sh\n", mode: 0o755},
		{name: "Demo.app/Contents/Frameworks", dir: true, mode: 0o755},
		{name: "Demo.app/Contents/Frameworks/Current", link: "A", mode: 0o755},
	}
}

func TestResolveAppFormats(t *testing.T) {
	for _, tc := range []struct {
		name    string
		archive func(t *testing.T) string
	}{
		{"zip", func(t *testing.T) string { return buildZip(t, bundle()) }},
		{"tar", func(t *testing.T) string { return buildTar(t, bundle(), false) }},
		{"tar.gz", func(t *testing.T) string { return buildTar(t, bundle(), true) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, cleanup, err := resolveApp(tc.archive(t))
			defer cleanup()
			if err != nil {
				t.Fatalf("resolveApp: %v", err)
			}
			if filepath.Base(app) != "Demo.app" {
				t.Errorf("bundle is %q, want .../Demo.app", app)
			}
			wantPerm(t, filepath.Join(app, "Contents/MacOS/Demo"), 0o755)
			plist, err := os.ReadFile(filepath.Join(app, "Contents/Info.plist"))
			if err != nil || string(plist) != "plist" {
				t.Errorf("Info.plist = %q, %v; want \"plist\"", plist, err)
			}
			link, err := os.Readlink(filepath.Join(app, "Contents/Frameworks/Current"))
			if err != nil {
				t.Fatalf("readlink: %v", err)
			}
			if link != "A" {
				t.Errorf("symlink points at %q, want \"A\"", link)
			}
		})
	}
}

// A zip from a Windows tool stores no unix mode bits, so the executable's
// has to be assumed.
func TestResolveAppZipWithoutModeBits(t *testing.T) {
	archive := buildZip(t, []entry{
		{name: "Demo.app/Contents/MacOS/Demo", body: "#!/bin/sh\n", nomodes: true},
		{name: "Demo.app/Contents/Info.plist", body: "plist", nomodes: true},
	})
	app, cleanup, err := resolveApp(archive)
	defer cleanup()
	if err != nil {
		t.Fatalf("resolveApp: %v", err)
	}
	wantPerm(t, filepath.Join(app, "Contents/MacOS/Demo"), 0o755)
	wantPerm(t, filepath.Join(app, "Contents/Info.plist"), 0o644)
}

// __MACOSX sidecars must not reach the volume.
func TestResolveAppSkipsAppleDouble(t *testing.T) {
	entries := append(bundle(),
		entry{name: "__MACOSX", dir: true, mode: 0o755},
		entry{name: "__MACOSX/Demo.app", dir: true, mode: 0o755},
		entry{name: "__MACOSX/Demo.app/._Info.plist", body: "sidecar", mode: 0o644},
	)
	app, cleanup, err := resolveApp(buildZip(t, entries))
	defer cleanup()
	if err != nil {
		t.Fatalf("resolveApp: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(app), "__MACOSX")); !os.IsNotExist(err) {
		t.Errorf("__MACOSX was unpacked (stat error %v), want it skipped", err)
	}
}

// macOS `tar` writes AppleDouble beside its file, not in a folder.
func TestUnpackSkipsAppleDoubleSidecars(t *testing.T) {
	dst := t.TempDir()
	entries := append(bundle(),
		entry{name: "Demo.app/._Contents", body: "sidecar", mode: 0o644},
		entry{name: "Demo.app/Contents/._Info.plist", body: "sidecar", mode: 0o644},
	)
	if err := unpack(buildTar(t, entries, false), dst); err != nil {
		t.Fatalf("unpack: %v", err)
	}
	for _, junk := range []string{"Demo.app/._Contents", "Demo.app/Contents/._Info.plist"} {
		if _, err := os.Lstat(filepath.Join(dst, junk)); !os.IsNotExist(err) {
			t.Errorf("%s was unpacked (stat error %v), want it skipped", junk, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "Demo.app/Contents/Info.plist")); err != nil {
		t.Errorf("real file did not survive: %v", err)
	}
}

func TestUnpackRefusesEscapingEntries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []entry
	}{
		{"parent", []entry{{name: "../escape", body: "x", mode: 0o644}}},
		{"parent below a folder", []entry{{name: "payload/../../escape", body: "x", mode: 0o644}}},
		{"through a symlink", []entry{
			{name: "link", link: "/tmp", mode: 0o755},
			{name: "link/escape", body: "x", mode: 0o644},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := unpack(buildTar(t, tc.entries, false), t.TempDir()); err == nil {
				t.Fatal("unpack accepted an entry that writes outside the destination")
			}
		})
	}
}

// A leading slash is dropped rather than refused, as `tar` does.
func TestUnpackRootsAbsoluteEntries(t *testing.T) {
	dst := t.TempDir()
	archive := buildTar(t, []entry{
		{name: "/build/Demo.app", dir: true, mode: 0o755},
		{name: "/build/Demo.app/Contents", dir: true, mode: 0o755},
		{name: "/build/Demo.app/Contents/Info.plist", body: "plist", mode: 0o644},
	}, false)
	if err := unpack(archive, dst); err != nil {
		t.Fatalf("unpack: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "build/Demo.app/Contents/Info.plist")); err != nil {
		t.Errorf("absolute entry did not land under the destination: %v", err)
	}
}

func TestUnpackUnsupportedExtension(t *testing.T) {
	if err := unpack(writeTemp(t, "bundle.7z", []byte("not an archive we read")), t.TempDir()); err == nil {
		t.Fatal("unpack accepted a file name it cannot handle")
	}
}

// An archive has to hold exactly one folder: the bundle.
func TestResolveAppArchiveWithoutOneFolder(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []entry
	}{
		{"no folder", []entry{{name: "notes.txt", body: "nothing here", mode: 0o644}}},
		{"two folders", []entry{
			{name: "Demo.app", dir: true, mode: 0o755},
			{name: "Other.app", dir: true, mode: 0o755},
		}},
		{"not a bundle", []entry{
			{name: "build", dir: true, mode: 0o755},
			{name: "build/Demo.app", dir: true, mode: 0o755},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, cleanup, err := resolveApp(buildTar(t, tc.entries, false))
			defer cleanup()
			if err == nil {
				t.Fatal("resolveApp accepted an archive that is not one folder")
			}
		})
	}
}

// Loose files beside the bundle, as a release archive carries, are ignored.
func TestBundleFolderIgnoresLooseFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Demo.app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	found, err := bundleFolder(root)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(found) != "Demo.app" {
		t.Errorf("found %q, want Demo.app", found)
	}
}

// A directory argument is the bundle itself and must not be copied.
func TestResolveAppDirectoryPassthrough(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Demo.app")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	app, cleanup, err := resolveApp(dir)
	defer cleanup()
	if err != nil {
		t.Fatal(err)
	}
	if app != dir {
		t.Errorf("resolveApp(%q) = %q, want it unchanged", dir, app)
	}
}
