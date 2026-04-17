// Standalone benchmark module. Isolated go.mod so its dependencies
// (go-bsdiff, librsync-go) never leak into the production project
// until a migration decision is made.
module benchmark

go 1.22

require (
	github.com/balena-os/librsync-go v0.9.0
	github.com/gabstv/go-bsdiff v1.0.5
	github.com/klauspost/compress v1.17.9
)

require (
	github.com/balena-os/circbuf v0.1.3 // indirect
	github.com/dsnet/compress v0.0.0-20171208185109-cc9eb1d7ad76 // indirect
	golang.org/x/crypto v0.7.0 // indirect
	golang.org/x/sys v0.6.0 // indirect
)
