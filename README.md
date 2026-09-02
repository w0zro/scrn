# scrn

A terminal UI for working on projects at the command line. scrn is a tmux
client: `scrn` brings up a tmux server of its own under its own
configuration, attaches the terminal, and runs the navigator down the left
of the home window. The shell under the navigator's cursor is the tmux pane
beside it; the navigator lists, finds, starts and kills what runs in every
project.

## Install

```sh
curl -fsSL https://scrn.w0zro.com/install.sh | sh
```

That fetches the build for this machine, checks it against the release's
checksums, and puts it in `~/.local/bin`, with the manpage beside it —
`man scrn` is the reference. `SCRN_INSTALL_DIR` says where else to
put it and `SCRN_VERSION` names a release other than the latest.

Builds are published for macOS and Linux, on both arm64 and amd64, and the
test suite runs on both. scrn needs two neighbors installed: `tmux`, which
holds and draws the shells, and `lsof`, which is how the process list is read.

## Build it yourself

```sh
go install github.com/w0zro/scrn@latest
```

## Releasing

Pushing a `v*` tag runs `.github/workflows/release.yml`, which tests on macOS,
cross-compiles the four builds, and publishes them with a `checksums.txt` that
`install.sh` reads. The script is served from `docs/`, with the site.

```sh
git tag v0.1.0 && git push origin v0.1.0
```

## License

MIT; see [LICENSE](LICENSE).
