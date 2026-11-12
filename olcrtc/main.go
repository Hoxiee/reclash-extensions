// Command olcrtc-ext runs the olcRTC client (github.com/openlibrecommunity/olcrtc) as a ReClash protocol extension.
// It must be built with build.sh, which compiles the pinned upstream module with the overlay in patches/; without
// it the build fails instead of dialing directly.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/Hoxiee/reclash-extensions/sdk"
)

// version is set by build.sh.
var version = "0.1.0"

type extension struct{}

func (extension) Info() sdk.Info { return sdk.Info{ID: "olcrtc", Version: version} }

func (extension) Describe(link string) (sdk.Node, error) { return describeLink(link) }

func (extension) Start(ctx context.Context, rt *sdk.Runtime) (sdk.Proxy, error) {
	return start(ctx, rt)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := sdk.Run(ctx, extension{}, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
