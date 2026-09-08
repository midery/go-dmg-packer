// Command dmg-packer builds a macOS disk image out of an application bundle.
// It is a CLI over [dmg.DMG.Create] in `github.com/jetbrains/go-dmg-writer`.
//
// Usage:
//
//	dmg-packer -app ./build/MyApp.app -out MyApp.dmg -mode udzo -name MyApp
//
// The `-app` argument is the bundle folder, or a zip, tar or gzipped tar
// archive holding one: see [resolveApp].
//
// The tool takes the bundle alone and stages the volume root itself, because
// [dmg.DMG.Create] copies a source tree verbatim: see [stage]. It also
// formats the volume itself, so there is no seed image to supply.
package main

import (
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jetbrains/go-dmg-packer/internal/dsstore"
	"github.com/jetbrains/go-dmg-writer"
)

func main() {
	var (
		appFlag  = flag.String("app", "", "the `.app` bundle to install, or a zip/tar archive holding one (required)")
		outFlag  = flag.String("out", "", "output .dmg path (required)")
		bgFlag   = flag.String("background", "", "picture to stage in `/.background/`")
		modeFlag = flag.String("mode", "udzo", "output mode: udro | udzo")
		nameFlag = flag.String("name", "disk image", "volume name as it appears in Finder")
		timeFlag = flag.Int64("time", 0, "Unix timestamp baked into the volume (0 = now)")
		partFlag = flag.Bool("partition", true, "frame the volume in a GUID partition table")
	)
	flag.Parse()
	if *appFlag == "" || *outFlag == "" {
		flag.Usage()
		os.Exit(2)
	}

	var mode dmg.Mode
	switch strings.ToLower(*modeFlag) {
	case "udro":
		mode = dmg.ModeReadOnly
	case "udzo":
		mode = dmg.ModeReadOnlyCompressed
	default:
		log.Fatalf("unknown -mode %q (want udro or udzo)", *modeFlag)
	}

	// A fixed time makes the output byte-stable. The writer stamps every catalog record with it, so the caller does
	// not have to normalize the source mtimes.
	var when time.Time
	if *timeFlag != 0 {
		when = time.Unix(*timeFlag, 0).UTC()
	}

	app, unpacked, err := resolveApp(*appFlag)
	defer unpacked()
	if err != nil {
		log.Fatalf("dmg-packer: %v", err)
	}

	root, err := stage(app, *bgFlag)
	if err != nil {
		log.Fatalf("dmg-packer: %v", err)
	}
	defer os.RemoveAll(root)

	d := &dmg.DMG{
		VolumeName: *nameFlag,
		Time:       when,
		// `hdiutil`'s default. HFS+ reads owner 99 as "no specific owner", so the volume mounts with the same
		// permissions on any host. The struct's zero value is the literal root UID, which is NOT what we want.
		OwnerID: dmg.OwnerIDUnset,
		GroupID: dmg.OwnerIDUnset,
		// A whole-disk layout, as `hdiutil create -layout GPTSPUD` writes it. macOS mounts a map-less image
		// too; a tool that parses one without mounting it cannot.
		PartitionMap: *partFlag,
	}
	if err := d.Create(root, *outFlag, mode); err != nil {
		log.Fatalf("dmg create: %v", err)
	}
	st, _ := os.Stat(*outFlag)
	fmt.Fprintf(os.Stderr, "wrote %s (%d bytes)\n", *outFlag, st.Size())
}

// stage builds the volume root in a temporary folder and returns it: the
// bundle, the `/Applications` symlink it is dragged onto, the `/.background/`
// folder, and the Finder layout keyed on the bundle's name. The caller
// removes the folder. bgPath may be empty, which stages the folder empty.
func stage(appPath, bgPath string) (string, error) {
	appEntry := filepath.Base(filepath.Clean(appPath))
	store, err := dsstore.Encode(appEntry)
	if err != nil {
		return "", err
	}
	root, err := os.MkdirTemp("", "dmg-packer-")
	if err != nil {
		return "", err
	}
	background := filepath.Join(root, ".background")
	err = func() error {
		if err := copyTree(appPath, filepath.Join(root, appEntry)); err != nil {
			return err
		}
		// An absolute target, so it resolves to the host's `/Applications` rather than into the mounted volume.
		if err := os.Symlink("/Applications", filepath.Join(root, dsstore.AliasName)); err != nil {
			return err
		}
		if err := os.Mkdir(background, 0o755); err != nil {
			return err
		}
		if bgPath != "" {
			if err := copyFile(bgPath, filepath.Join(background, filepath.Base(bgPath)), 0o644); err != nil {
				return err
			}
		}
		return os.WriteFile(filepath.Join(root, ".DS_Store"), store, 0o644)
	}()
	if err != nil {
		os.RemoveAll(root)
		return "", fmt.Errorf("stage the volume root: %w", err)
	}
	return root, nil
}

// copyTree copies the tree rooted at src to dst. A bundle's symlinks are
// copied as symlinks rather than followed, because the ones inside a
// framework are what make it loadable. Modification times are not carried
// over: the writer stamps the catalog with one time of its own.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if entry.Type()&fs.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			// The owner bits are forced on so the walk can write into a folder even if the source is read-only.
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case entry.Type().IsRegular():
			return copyFile(path, target, info.Mode().Perm())
		default:
			return fmt.Errorf("%s is a %s, which cannot go in a disk image", path, entry.Type())
		}
	})
}

// copyFile copies one file, creating the destination with perm.
func copyFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
