module github.com/Systicore/systicore_go/fiberv3

go 1.26.0

// Own module so the root stays framework-free and fiber v3's fasthttp pin
// never leaks into chi / net/http consumers.
require github.com/Systicore/systicore_go v0.1.0

require (
	github.com/gofiber/fiber/v3 v3.5.0
	github.com/google/uuid v1.6.0
	github.com/rs/zerolog v1.35.1
)

require (
	github.com/andybalholm/brotli v1.2.2 // indirect
	github.com/gofiber/schema v1.8.3 // indirect
	github.com/gofiber/utils/v2 v2.4.1 // indirect
	github.com/klauspost/compress v1.19.2 // indirect
	github.com/mattn/go-colorable v0.1.15 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/philhofer/fwd v1.2.0 // indirect
	github.com/tinylib/msgp v1.6.4 // indirect
	github.com/valyala/bytebufferpool v1.0.0 // indirect
	github.com/valyala/fasthttp v1.73.0 // indirect
	golang.org/x/crypto v0.57.0 // indirect
	golang.org/x/net v0.60.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	gopkg.in/natefinch/lumberjack.v2 v2.2.1 // indirect
)
