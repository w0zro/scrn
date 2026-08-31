# scrn

A terminal UI for working on projects at the command line.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/w0zro/scrn/main/install.sh | sh
```

That fetches the build for this machine, checks it against the release's
checksums, and puts it in `~/.local/bin`, with the manpage beside it —
`man scrn` is the reference. `SCRN_INSTALL_DIR` says where else to
put it and `SCRN_VERSION` names a release other than the latest.

Builds are published for macOS and Linux, on both arm64 and amd64. The Linux
ones are cross-compiled and untested; scrn reads the process list through
`lsof`, which has to be installed for it to see anything.

## Build it yourself

```sh
go install github.com/w0zro/scrn@latest
```

## Releasing

Pushing a `v*` tag runs `.github/workflows/release.yml`, which tests on macOS,
cross-compiles the four builds, and publishes them with a `checksums.txt` that
`install.sh` reads.

```sh
git tag v0.1.0 && git push origin v0.1.0
```
