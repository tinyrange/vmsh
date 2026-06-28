module github.com/tinyrange/vmsh

go 1.25.5

require (
	github.com/creack/pty v1.1.24
	golang.org/x/crypto v0.50.0
	golang.org/x/sys v0.43.0
	golang.org/x/term v0.42.0
	j5.nz/cc v0.0.0
)

require (
	github.com/ebitengine/purego v0.10.0 // indirect
	github.com/klauspost/compress v1.18.6 // indirect
	github.com/ulikunitz/xz v0.5.15 // indirect
	golang.org/x/net v0.53.0 // indirect
)

replace j5.nz/cc => ./cc
