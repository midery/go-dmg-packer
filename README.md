# go-dmg-packer

A cross-platform CLI that packs a macOS `.app` bundle into a `.dmg`, laid out
as the usual drag-to-install window. No cgo and no `hdiutil`: it builds and
runs on macOS, Linux and Windows.

The disk-image format comes from
[`github.com/jetbrains/go-dmg-writer`](https://github.com/JetBrains/go-dmg-writer);
this repository holds the CLI and the Finder layout.

```console
$ dmg-packer -app ./build/MyApp.app -out MyApp.dmg -name MyApp
wrote MyApp.dmg (72301184 bytes)
```

## Install

`go-dmg-writer` is a private module, so point the toolchain at it first:

```console
$ export GOPRIVATE='github.com/jetbrains/*'
$ go install github.com/jetbrains/go-dmg-packer/cmd/dmg-packer@latest
```

Or from a checkout:

```console
$ CGO_ENABLED=0 go build -o dmg-packer ./cmd/dmg-packer
```

## Flags

| Flag          | Default      | Meaning                                                        |
| ------------- | ------------ | -------------------------------------------------------------- |
| `-app`        | —            | The `.app` bundle, or a zip/tar archive holding one. Required. |
| `-out`        | —            | Output `.dmg` path. Required.                                  |
| `-background` | —            | Picture to stage in `/.background/`.                           |
| `-mode`       | `udzo`       | `udro` (uncompressed UDIF) or `udzo` (per-chunk zlib).         |
| `-name`       | `disk image` | Volume name as it appears in Finder.                           |
| `-time`       | `0`          | Unix timestamp baked into the volume. `0` means now.           |
| `-partition`  | `true`       | Frame the volume in a GUID partition table.                    |

A missing `-app` or `-out` prints the usage and exits 2; any other failure
logs and exits 1. On success the tool prints `wrote <path> (<n> bytes)` to
stderr.

## The bundle

`-app` takes the bundle folder, or a `.zip`, `.tar`, `.tar.gz` or `.tgz`
holding it — the extension picks the format:

```console
$ dmg-packer -app ./out/MyApp-1.2.3-arm64.zip -out MyApp.dmg -name MyApp
```

The archive has to hold the `.app` folder at its top level, which is what a
build produces; loose files beside it are ignored. Anything else — several
folders, or the bundle's bare contents from `ditto -c -k` without
`--keepParent` — is refused rather than guessed at.

Two things to know when packing from an archive:

- **A zip from a Windows tool carries no POSIX modes.** Files under
  `Contents/MacOS` are always made executable, since the bundle cannot run
  otherwise. If it needs the execute bit anywhere else, pass a tar or a
  folder. Folder modes are not carried over either: every folder is `0755`.
- **AppleDouble sidecars are dropped**, both `__MACOSX/` and `._name`. The
  xattrs and resource forks they carry are not written to the volume either
  way.
- **The volume name comes from `-name`**, not from the bundle or the archive.

## The volume

The bundle is all you pass; the rest of the volume root is staged for you.

```
/
├── MyApp.app/          <- from -app
├── Applications        <- symlink to drag onto
├── .background/        <- the -background picture, if any
└── .DS_Store           <- the Finder layout
```

## Reproducible output

Pass a fixed `-time` and the output is byte-stable, whatever the source
mtimes:

```console
$ dmg-packer -app ./MyApp.app -out a.dmg -time "$SOURCE_DATE_EPOCH"
$ dmg-packer -app ./MyApp.app -out b.dmg -time "$SOURCE_DATE_EPOCH"
$ cmp a.dmg b.dmg   # identical
```

With `-partition`, the partition GUIDs are derived from the volume name,
`-time` and size, so two images built from the same three carry the same
GUIDs.

## Limitations

- The volume is HFSX (case-sensitive), not classic HFS+.
- Only `udro` and `udzo`. No `ulfo`/LZFSE, no `udbz`/bzip2.
- Archive input is `.zip`, `.tar`, `.tar.gz` and `.tgz` only — no bzip2, xz
  or zstd, and the name has to match the format.
- No software licence agreement (SLA) resource, no journaling, no resource
  forks, no encryption.
- Hard links are duplicated, and symlink targets are stored verbatim.
- Write-only: it cannot read an existing `.dmg`.
- Windows is a poor build host: NTFS loses the POSIX modes and symlinks a
  signed bundle needs.

Codesigning and notarization are the caller's job, after the image is written.

## Development

```console
$ go test ./...
$ go vet ./...
```

## Licence

Apache 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
