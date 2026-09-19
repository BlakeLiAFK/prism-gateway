# Third-party components

The Go source, CGo wrapper, interface graphics and frontend code in this repository
are provided under the repository's MIT license. There are no downloaded Go
modules, npm dependencies, bundled font files, or remote frontend CDN assets.

The build uses the Go standard library and the system SQLite implementation.
SQLite upstream describes its core as public domain: https://www.sqlite.org/copyright.html
Your OS distribution may package additional patches or build features; consult
its notices. The SQLite source is not vendored in this source archive.

The optional Linux executable statically links the distribution's SQLite library
and dynamically uses the system C runtime and libm. The operating system / Go
runtime licenses remain applicable. No vendor API brands imply endorsement or
an official relationship with Prism Gateway.
