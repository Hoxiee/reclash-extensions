// Command qwdtt-ext runs the qWDTT client (github.com/SpaceNeuroX/proxy-turn-vk-android, go_client) as a ReClash
// protocol extension. It must be built with build.sh, which compiles the pinned upstream files into this package
// with the patch in patches/ applied; without it the build fails instead of dialing directly.
package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/Hoxiee/reclash-extensions/sdk"
)

// version is set by build.sh.
var version = "0.1.0"

// upstreamStdout reads what upstream prints for its Android host; the supervisor's events keep the real stdout.
var upstreamStdout io.Reader

type extension struct{}

func (extension) Info() sdk.Info { return sdk.Info{ID: "qwdtt", Version: version} }

func (extension) Describe(link string) (sdk.Node, error) { return describeLink(link) }

func (extension) Start(ctx context.Context, rt *sdk.Runtime) (sdk.Proxy, error) {
	return start(ctx, rt)
}

func main() {
	events := os.Stdout
	if r, w, err := os.Pipe(); err == nil {
		os.Stdout, upstreamStdout = w, r
	} else {
		os.Stdout = os.Stderr
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := sdk.Run(ctx, extension{}, os.Args[1:], os.Stdin, events, os.Stderr)
	stop()
	os.Exit(code)
}
