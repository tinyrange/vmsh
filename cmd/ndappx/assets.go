package main

import _ "embed"

// neurodeskWordmarkPNG combines the official Neurodesk logo and wordmark into
// one high-resolution, transparent texture for the native startup window.
//
//go:embed assets/neurodesk-wordmark.png
var neurodeskWordmarkPNG []byte
