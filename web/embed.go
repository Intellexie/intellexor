package web

import (
	"embed"
	"io/fs"
)

//go:embed all:assets
var assets embed.FS

func FS() fs.FS {
	f, err := fs.Sub(assets, "assets")
	if err != nil {
		panic(err)
	}
	return f
}
