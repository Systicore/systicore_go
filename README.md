# systicore_go

Shared Go packages for the SYSTICORE backends. Each package is small,
transport-agnostic where it can be, and takes options instead of a
service's config struct.

```bash
go get github.com/Systicore/systicore_go@latest
```

| Package | What |
|---|---|
| `logging` | zerolog bootstrap (level, console/file with rotation) + request-scoped logger for chi and gRPC, request-id propagation |

## logging

The domain layer never imports `logging`: it calls `zerolog.Ctx(ctx)` and
gets the request-scoped logger the transport put there (request id,
method/rpc, client ip, and whatever `logging.Enrich` added later, e.g.
`user_id`), or the root logger outside a request.

```go
root, closeLog, err := logging.New(logging.Options{
    Service: cfg.App.Name, Level: cfg.Logging.Level, Pretty: cfg.Logging.Pretty,
    File:    &logging.FileOptions{Dir: cfg.Logging.Directory, MaxSizeMB: 50, MaxBackups: 7, MaxAgeDays: 30, Compress: true},
})
defer closeLog()

r.Use(middleware.RequestID, middleware.RealIP, logging.Middleware(root), middleware.Recoverer)
grpc.NewServer(grpc.ChainUnaryInterceptor(logging.UnaryServerInterceptor(root)))
grpc.NewClient(target, grpc.WithChainUnaryInterceptor(logging.UnaryClientInterceptor()))
```

- `Middleware` (after chi's `RequestID` + `RealIP`): scoped logger in
  `r.Context()`, one `request` line per call with status/bytes/latency.
- `UnaryServerInterceptor`: the same for gRPC (`rpc`, `code`), request id
  from the `x-request-id` metadata or minted.
- `UnaryClientInterceptor`: forwards the request id, so one id spans a
  reports → projects → core chain.
- `Enrich(ctx, fn)`: add fields in place for the rest of the request.
- Workers take the logger in their constructor and derive a per-run
  context (`ExampleNew`).

No package-level `Info()`/`Fatal()` wrappers on purpose: the dependency is
explicit, tests pass `zerolog.Nop()`. The only global is
`zerolog.DefaultContextLogger`, set to the root so a bare
`context.Background()` still logs with the service name.
