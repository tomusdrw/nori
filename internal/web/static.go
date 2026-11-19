package web

import (
	"crypto/sha256"
	"embed"
	"fmt"
)

//go:embed static
var staticFS embed.FS

// Change the URL with the CSS so cached styles always match the templates.
var appStylesheet = func() string {
	css, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("/static/app.css?v=%x", sha256.Sum256(css))
}()
