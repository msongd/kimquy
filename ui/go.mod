module github.com/msongd/kimquy/ui

go 1.27.3

require (
	github.com/egoist/mygo v0.6.4
	github.com/msongd/kimquy v0.0.0
)

require (
	github.com/ebitengine/purego v0.11.1 // indirect
	github.com/go-text/typesetting v0.3.5 // indirect
	golang.org/x/image v0.46.0 // indirect
	golang.org/x/net v0.59.0 // indirect
	golang.org/x/text v0.43.0 // indirect
)

// The UI is its own module so that semd's go.mod keeps no third-party
// dependencies; it uses the daemon's pure-Go client packages.
replace github.com/msongd/kimquy => ../
