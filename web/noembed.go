//go:build !webembed

package web

import "net/http"

// distHandler returns nil when the browser bundle was not embedded at build time.
func distHandler() http.Handler { return nil }
