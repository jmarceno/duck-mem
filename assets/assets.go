// Package assets embeds the project artwork so the binary ships with the
// logo the README and the tray icon both use. The canonical editable artwork
// is assets/logo.svg; logo.png is its transparent raster export.
package assets

import _ "embed"

// Logo is the duck-mem logo: a transparent PNG rasterized from logo.svg.
//
//go:embed logo.png
var Logo []byte
