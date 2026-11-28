module github.com/ourchive/ourchive/client

go 1.26.0

require (
	github.com/ourchive/ourchive v0.0.0
	github.com/wailsapp/wails/v3 v3.0.0
)

// The client is built from this repository's hub/agent packages, never a published copy.
replace github.com/ourchive/ourchive => ../
