# systicore_go

Shared Go packages for every Systicore backend — the systicore_* services,
blacklab, bliss, biharvideke, mymosaiq, … — whatever HTTP stack they run.

## Layout: one framework-free core, adapters at the edge

```
systicore_go/                  module github.com/Systicore/systicore_go       ← core: stdlib + zerolog only
├── logging/                   New, Options, context helpers, Middleware (net/http)
├── logging/grpclog/           gRPC interceptors (compiled only when imported)
├── report/                    client of the systicore_reports backend (error reports from services)
├── fiberv3/                   module github.com/Systicore/systicore_go/fiberv3  ← Fiber v3 adapters
├── fiberv2/                   module github.com/Systicore/systicore_go/fiberv2  ← Fiber v2 adapters
└── go.work                    local dev only
```

Rules that keep it usable everywhere:

1. **The core imports no web framework.** Everything transport-independent
   lives in the root module and speaks `context.Context`; the `net/http`
   adapters are plain `func(http.Handler) http.Handler`, which is why chi,
   gorilla/mux and a bare `ServeMux` share them with no chi dependency.
2. **Fiber is a separate module per major.** Fiber is fasthttp-based, so it
   needs its own handlers, and v2 and v3 pin different versions of the
   unversioned `github.com/valyala/fasthttp` path. Keeping `fiberv2/` and
   `fiberv3/` as modules of their own means a chi service never sees
   fasthttp in its graph and a v2 service is never forced onto v3's pin.
3. **Heavy optional deps go in sub-packages.** `logging/grpclog` is a
   package, not a module: a service that never imports it never compiles
   gRPC, while services that do get one version of everything.
4. **Options, not configs.** Packages take small option structs; no
   service's `config` package is imported here.
5. **The domain layer never imports this module.** It gets the logger from
   `zerolog.Ctx(ctx)`, the request id from `logging.RequestID(ctx)` if it
   must — the same call under net/http, Fiber v2, Fiber v3 and gRPC.

## Versioning

Three modules, three tag series, all semver:

| Module | Tag |
|---|---|
| root | `v0.1.0` |
| fiberv3 | `fiberv3/v0.1.0` |
| fiberv2 | `fiberv2/v0.1.0` |

Release order: tag the root, point the adapters' `require` at that tag
(and remove the `replace ../` they carry while unpublished), tag the
adapters. Consumers:

```bash
go get github.com/Systicore/systicore_go@v0.1.0
go get github.com/Systicore/systicore_go/fiberv3@v0.1.0   # Fiber v3 services
go get github.com/Systicore/systicore_go/fiberv2@v0.1.0   # Fiber v2 services
```

## logging

```go
root, closeLog, err := logging.New(logging.Options{
    Service: cfg.App.Name, Level: cfg.Logging.Level, Pretty: cfg.Logging.Pretty,
    File:    &logging.FileOptions{Dir: cfg.Logging.Directory, MaxSizeMB: 50, MaxBackups: 7, MaxAgeDays: 30, Compress: true},
})
defer closeLog()
```

| Stack | Wiring | Logger in the handler |
|---|---|---|
| net/http, chi, gorilla | `r.Use(logging.Middleware(root))` (after RealIP, before Recoverer) | `zerolog.Ctx(r.Context())` |
| Fiber v3 | `app.Use(fiberv3.Logging(root))` | `zerolog.Ctx(c.Context())` |
| Fiber v2 | `app.Use(fiberv2.Logging(root))` | `zerolog.Ctx(c.UserContext())` — **not** `c.Context()`, that is the fasthttp ctx |
| gRPC server | `grpc.ChainUnaryInterceptor(grpclog.UnaryServerInterceptor(root))` | `zerolog.Ctx(ctx)` |
| gRPC client | `grpc.WithChainUnaryInterceptor(grpclog.UnaryClientInterceptor())` | forwards `x-request-id` |
| worker / cron | constructor takes `zerolog.Logger`, derives a per-run ctx | `zerolog.Ctx(ctx)` |

Every adapter does the same three things: request-scoped logger with
`request_id`, `method`, `path`, `ip` into the context; one `request` /
`rpc` line with status, bytes, latency; `X-Request-ID` taken from the
caller or minted and echoed back, so one id follows a request across
services. `logging.Enrich(ctx, fn)` adds fields in place (`user_id` after
auth). No package-level `Info()`/`Fatal()`; tests pass `zerolog.Nop()`.

## report

Error reports from a Go service into the same pipeline the apps use
(reports backend → dedup, severity escalation, triage, projects
automation → task / GitHub issue / push):

```go
rep, err := report.New(report.Options{
    BaseURL: cfg.Reports.URL,            // https://reports.systicore.hu
    Source:  cfg.App.Name,               // the project's report source, e.g. blacklab_backend
    Device:  report.ServerDevice(version),
    Tokens:  serviceTokens,              // client_credentials token source; nil in dev
    Logger:  root,                       // WITHOUT the hook, see below
})
defer rep.Close(ctx)

root = root.Hook(rep.Hook())            // every Error/Fatal/Panic log line → a report

rep.CaptureError(ctx, err, "eventpush.deliver", report.SeverityCritical) // rich: stable code, stack, request id
```

- `Capture` / `CaptureError` never block: bounded queue, one sender with
  retry (transient 5xx/429/401 retried, 400/404/413/422 dropped as
  `ErrRejected`), drops counted (`Dropped()`) and logged once a minute.
  `Close` flushes the queue with a deadline.
- `ErrorCode(err)`: a `Coder`'s own code, else the outermost typed error
  (`*fs.PathError`), else `"error"` — the dedup identity is code + message
  + action per host, so keep codes stable and put detail in the trace.
- `ServerDevice(version)`: brand `server`, model = hostname, GOOS/GOARCH +
  Go version, app version from your ldflags.

**Server side, required before this works:** the reports backend's
`POST /api/v1/report` today accepts user JWTs only (its middleware parses
`sub` as a numeric user id); a client_credentials token (`sub = client:<id>`)
gets 401. Reports v2 must accept service tokens on the ingest route
(user id absent, source from the body) — until then use a user token
(`report.StaticToken`) or leave `Tokens` nil in development.
