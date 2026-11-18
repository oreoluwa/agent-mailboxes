# Third-party notices

Agent Emails' own code is licensed under MIT; see LICENSE. Third-party software retains its original licenses and copyrights. The MIT license does not replace the terms below.

## Native server and CLI

`third_party/go/` contains license and notice files collected for `cmd/email` and `cmd/emaild` with Google's go-licenses v1.6.0 across Linux/macOS amd64/arm64 targets. `third_party/go-dependencies.csv` records the detected licenses and versioned upstream URLs. `third_party/go-runtime/` contains the Go 1.25.7 runtime license, patent grant, and vendored library notices. These texts accompany native release bundles.

## Containers and external services

Native bundles do not contain Poppler, Node.js, PostgreSQL, pgvector, or an operating-system image. Those components are installed or pulled separately by deployment tools. Their own licenses remain applicable.

The daemon Dockerfile installs Alpine's Poppler utilities (GPL-2.0-or-later), and Alpine images contain other separately licensed packages. Before distributing prebuilt images, retain the licenses for the exact image packages and satisfy applicable corresponding-source requirements, including distribution patches/build instructions. This notice is not a substitute for supplying corresponding source. The packaging helper does not publish images.

## Updating notices

When changing dependencies or the Go toolchain, refresh the affected notices with established tools and inspect their output. Do not relabel third-party code as MIT. For the Go commands:

```sh
go install github.com/google/go-licenses@v1.6.0
GOOS=linux GOARCH=amd64 go-licenses save ./cmd/email ./cmd/emaild --save_path=/tmp/agent-emails-go-licenses --ignore=github.com/oreoluwa/agent-mailboxes
GOOS=linux GOARCH=amd64 go-licenses report ./cmd/email ./cmd/emaild --ignore=github.com/oreoluwa/agent-mailboxes
```

Repeat for the other supported targets (linux/arm64, darwin/amd64, darwin/arm64), taking the union of dependency notices. Review and copy the resulting texts into `third_party/go/`; refresh the Go runtime texts from the selected toolchain. Test-only dependencies are not part of the native runtime inventory. Preserve any licenses attached to copied source or generated code as well.
