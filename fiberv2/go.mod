module github.com/Systicore/systicore_go/fiberv2

go 1.26

// Own module: Fiber v2 pins an older fasthttp than v3 (both import the
// unversioned github.com/valyala/fasthttp path), so keeping the majors in
// separate modules stops MVS from dragging v2 users onto v3's pin. Until
// the root is tagged the adapter builds against the sibling checkout; on
// release require a real root version and drop the replace.
require github.com/Systicore/systicore_go v0.0.0

require (
	github.com/gofiber/fiber/v2 v2.52.15
	github.com/google/uuid v1.6.0
	github.com/rs/zerolog v1.35.1
)

require (
	github.com/andybalholm/brotli v1.2.2 // indirect
	github.com/klauspost/compress v1.19.2 // indirect
	github.com/mattn/go-colorable v0.1.15 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/mattn/go-runewidth v0.0.16 // indirect
	github.com/rivo/uniseg v0.2.0 // indirect
	github.com/valyala/bytebufferpool v1.0.0 // indirect
	github.com/valyala/fasthttp v1.73.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	gopkg.in/natefinch/lumberjack.v2 v2.2.1 // indirect
)

replace github.com/Systicore/systicore_go => ../
