module github.com/Hoxiee/reclash-extensions/naive

go 1.26.6

require (
	github.com/Hoxiee/reclash-extensions/sdk v0.0.0
	github.com/bogdanfinn/fhttp v0.6.8
	github.com/bogdanfinn/tls-client v1.15.1
	github.com/bogdanfinn/utls v1.7.7-barnius
	golang.org/x/net v0.57.0
)

require (
	github.com/andybalholm/brotli v1.2.0 // indirect
	github.com/klauspost/compress v1.18.2 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
)

replace github.com/Hoxiee/reclash-extensions/sdk => ../sdk
