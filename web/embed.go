//go:build webembed

package web

import (
	"embed"
	"io/fs"
	"net/http"
)

// The real production bundle. `make build` runs `npm run build` first and compiles with
// -tags webembed; the default build tag set never reaches go:embed.
//
//go:embed all:dist
var distFS embed.FS

func distHandler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return nil
	}
	return spaHandler(sub)
}
