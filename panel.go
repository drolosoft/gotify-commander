package main

import (
	_ "embed"
	"encoding/base64"
	"strings"
)

// The control panel is one HTML page. Its markup, its stylesheet and its two
// scripts are files of their own under web/, so they are edited, formatted
// and reviewed as what they are; they are embedded in the plugin and put
// together once, when it loads.

// panelHTML is the markup, with a placeholder where each other piece goes.
//
//go:embed web/panel.html
var panelHTML string

// panelCSS is the page's only stylesheet. It is written for this page, with
// no framework under it.
//
//go:embed web/panel.css
var panelCSS string

// panelBootJS runs in <head> and sets the stored theme before the first
// paint.
//
//go:embed web/panel-boot.js
var panelBootJS string

// panelJS is the page's script.
//
//go:embed web/panel.js
var panelJS string

// panelIcon is the icon of the page, 96 px: its favicon, the image beside
// the title and what GET /icon serves.
//
//go:embed web/icon.png
var panelIcon []byte

// panelTouchIcon is the square 180 px icon a phone uses when the page is
// added to its home screen.
//
//go:embed web/touch-icon.png
var panelTouchIcon []byte

// commandUI is the whole page, with everything inlined except the nonce.
// One response carries all of it: the page needs no second request, no third
// party, and the CSP can stay at a nonce with no 'self'.
var commandUI = buildCommandUI()

// buildCommandUI puts the pieces of the page into its markup. The icons go in
// as data: URIs, which img-src allows, because a relative URL would depend on
// the page being opened with its final slash.
func buildCommandUI() string {
	pieces := strings.NewReplacer(
		"{{css}}", panelCSS,
		"{{boot}}", panelBootJS,
		"{{js}}", panelJS,
		"{{icon}}", pngDataURI(panelIcon),
		"{{touch_icon}}", pngDataURI(panelTouchIcon),
	)

	return pieces.Replace(panelHTML)
}

// pngDataURI returns a PNG image as a data: URI.
func pngDataURI(image []byte) string {
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(image)
}

// renderPanel returns the control panel page for one response, with nonce on
// every <script> and <style> block.
func renderPanel(nonce string) string {
	return strings.ReplaceAll(commandUI, "{{nonce}}", nonce)
}
