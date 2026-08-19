package launcher

import (
	"embed"
	"io/fs"
)

//go:embed assets/*
var launcherAssets embed.FS

func Assets() fs.FS {
	assets, err := fs.Sub(launcherAssets, "assets")
	if err != nil {
		panic(err)
	}
	return assets
}
