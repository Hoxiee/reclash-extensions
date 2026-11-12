// Command naive-ext is a NaiveProxy client (github.com/klzgrad/naiveproxy) for ReClash: HTTP/2 CONNECT tunnels
// with naive's padding, over a Chrome TLS ClientHello, every connection dialed through the Core's egress.
package main

import (
	"context"

	"github.com/Hoxiee/reclash-extensions/sdk"
)

// version is set by build.sh.
var version = "0.1.0"

type extension struct{}

func (extension) Info() sdk.Info { return sdk.Info{ID: "naive", Version: version} }

func (extension) Describe(link string) (sdk.Node, error) { return describeLink(link) }

func (extension) Start(ctx context.Context, rt *sdk.Runtime) (sdk.Proxy, error) {
	return start(ctx, rt)
}

func main() { sdk.Main(extension{}) }
