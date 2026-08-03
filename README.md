# bsftp

A terminal SFTP file browser for macOS with native QuickLook previews.

`bsftp` connects to any host in your `~/.ssh/config`, gives you a fast
vim-style file listing, and lets you hit <kbd>Space</kbd> to preview remote
files in a real macOS QuickLook window — images, PDFs, video, anything
QuickLook can render. Files are streamed into a local cache in the background
so flipping through a directory of photos feels local.

## Platform support

**macOS only.** The previewing pipeline is built on QuickLook
(`QLPreviewPanel` / `qlmanage`), and the tool shells out to `open` and
`pbcopy`. The core browser would in principle run elsewhere, but no other
platform is supported or tested.

## Requirements

- Go 1.26+
- Xcode Command Line Tools (`swiftc`) — only needed for the optional preview
  helper
- An SSH setup that already works: host aliases in `~/.ssh/config`, keys in
  `~/.ssh` or loaded into `ssh-agent`

## Building

```sh
make            # builds the `bsftp` binary and the Swift preview helper
make go         # just the Go binary
make helper     # just the Swift QuickLook helper (bsftp-preview)
```

Keep `bsftp-preview` next to the `bsftp` binary (or on `$PATH`). Without it,
previews still work through a slower `qlmanage` fallback.

## Usage

```sh
bsftp <ssh-host-alias> [remote-path]
```

The host argument is any alias from your `~/.ssh/config`. You can set a
default host with the `BSFTP_HOST` environment variable and then run plain
`bsftp`.

```sh
bsftp -ls <host> [path]   # one-shot directory listing, no TUI (smoke test)
```

Authentication uses your `ssh-agent` if one is running, then falls back to
the identity files listed in `~/.ssh/config` and the standard key names
(`id_ed25519`, `id_rsa`, `id_ecdsa`), prompting for a passphrase if needed.
Host keys are verified against `~/.ssh/known_hosts`. Dropped connections
reconnect automatically with backoff.

## Keys

| Key | Action |
| --- | --- |
| <kbd>j</kbd>/<kbd>k</kbd>, arrows | move cursor |
| <kbd>h</kbd>/<kbd>l</kbd>, <kbd>Enter</kbd> | parent / enter directory |
| <kbd>Space</kbd> | QuickLook preview (arrow keys flip between files) |
| <kbd>:</kbd> | jump to path, with Tab completion |
| <kbd>/</kbd> | filter the listing |
| <kbd>d</kbd> / <kbd>D</kbd> | download to `~/Downloads` / to a chosen directory |
| <kbd>u</kbd> | upload (type or drag a local file into the terminal) |
| <kbd>o</kbd> | download and reveal in Finder |
| <kbd>y</kbd> | copy remote path to clipboard |
| <kbd>c</kbd> / <kbd>n</kbd> / <kbd>x</kbd> | rename / mkdir / delete |
| <kbd>b</kbd> / <kbd>B</kbd> / <kbd>1</kbd>–<kbd>9</kbd> | bookmarks pane / add bookmark / jump |
| <kbd>?</kbd> | full help |
| <kbd>q</kbd> | quit |

## Data locations

- Bookmarks: `~/Library/Application Support/bsftp/bookmarks.json`
- Preview cache: `~/Library/Caches/bsftp/preview` (LRU-evicted, 2 GB cap)
