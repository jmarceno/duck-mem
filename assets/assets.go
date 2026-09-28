// Package assets embeds the project artwork so the binary ships with the
// logo the README and the tray icon both use.
package assets

import _ "embed"

// Logo is the duck-mem logo: a PNG with a transparent background.
//
//go:embed logo.png
var Logo []byte
