// Command openflux-ext runs OpenFlux (github.com/p1neappleXpress/OpenFlux) as a ReClash protocol extension.
// It must be built with build.sh: the upstream patch in patches/ routes every carrier socket through the egress,
// and without it the build fails instead of dialing directly.
package main

import (
	"context"

	"github.com/Hoxiee/reclash-extensions/sdk"
)

// version is set by build.sh.
var version = "0.1.0"

type extension struct{}

func (extension) Info() sdk.Info { return sdk.Info{ID: "openflux", Version: version} }

func (extension) Describe(link string) (sdk.Node, error) { return describeLink(link) }

func (extension) Start(ctx context.Context, rt *sdk.Runtime) (sdk.Proxy, error) {
	return start(ctx, rt)
}

func main() { sdk.Main(extension{}) }
