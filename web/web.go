package web

import "embed"

//go:embed index.html
var IndexHTML string

//go:embed lighttest.html
var LightTestHTML string

//go:embed layerwatch.svg layerwatch-icon.png layerwatch-512.png layerwatch-192.png favicon-32.png
var Assets embed.FS
