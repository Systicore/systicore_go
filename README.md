# systicore_go

Shared Go packages for every Systicore backend — the systicore_* services,
blacklab, bliss, biharvideke, mymosaiq, … — whatever HTTP stack they run.

## Layout: one framework-free core, adapters at the edge

```
systicore_go/                  module github.com/Systicore/systicore_go       ← core: stdlib + zerolog only
├── logging/                   New, Options, context helpers, Middleware (net/http)
├── logging/grpclog/           gRPC interceptors (compiled only when imported)
├── report/                    client of the systicore_reports backend (error reports from services)
├── report/reporttest/         fake ingest endpoint for tests
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

Consumers, once published (see [Releasing](#releasing)):

```bash
go get github.com/Systicore/systicore_go@v0.1.0
go get github.com/Systicore/systicore_go/fiberv3@v0.1.0   # Fiber v3 services
go get github.com/Systicore/systicore_go/fiberv2@v0.1.0   # Fiber v2 services
```

The module path is case-sensitive: `github.com/Systicore/...` with a
capital S, in imports and in any `GOPRIVATE` pattern.

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

Error reports from a Go service into the pipeline the apps use: reports
backend (grouping, triage) → projects automation → one GitHub issue per
new error group.

### Configuration

| Env | Option | |
|---|---|---|
| `REPORTS_URL` | `BaseURL` | e.g. `https://reports.systicore.hu`; plain `http://` only on a private network, since the key travels in a header |
| `REPORTS_KEY` | `Key` | the component's secret ingest key, `scsk_live_…` (production) or `scsk_test_…`, sent as `X-Systicore-Key`; lives in the VPS `.env`, never in a repo |
| `INSTANCE_ID` | `InstanceID` | optional stable instance name; default `<Source>-<Environment>` |
| `APP_ENV` (or the service's own name for it) | `Environment` | informational; the key's environment is authoritative |
| `-ldflags -X` | `Release{Version, Commit, BuildTime}` | the build stamped into the binary |

**Reporting is off when `REPORTS_URL` or `REPORTS_KEY` is empty.** `New`
then returns a disabled client and no error, and every method of a
disabled or nil `*report.Client` is a no-op — wire it unconditionally and
let the environment decide. A service that also reads `REPORTS_ENABLED`
passes an empty `Key` when it is false. Both values are trimmed first,
so the `\r` a CRLF `.env` leaves behind is harmless. `New` fails only on
a malformed URL or a key that is not shaped like an ingest key
(`(scsk|scpk)_(live|test)_` + 32 base62 characters; the error never
echoes the key); the nil client it returns then is still safe to use.
The pre-v1.1 `sk_` / `pk_` types are rejected: they have the shape of
Stripe keys, so secret scanners flag every config or test holding one.

```go
reports, err := report.New(report.Options{
    BaseURL:     cfg.Reports.URL,        // REPORTS_URL
    Key:         cfg.Reports.Key,        // REPORTS_KEY
    Source:      cfg.App.Name,           // blacklab_backend
    Environment: cfg.App.Env,            // production | development | staging
    InstanceID:  cfg.Reports.InstanceID, // INSTANCE_ID, optional
    Release:     report.Release{Version: version.Version, Commit: version.Commit, BuildTime: version.BuildTime},
    Logger:      root,                   // WITHOUT report.Hook attached
})
if err != nil {
    root.Warn().Err(err).Msg("error reporting disabled") // reports is nil: a no-op client
}
defer reports.Close(shutdownCtx) // flushes the queue, bounded by the ctx
```

### What is sent

`POST {REPORTS_URL}/api/v1/ingest`, body per the ingest contract:
`error{type, code, message, trace, action, severity, fingerprint}`,
`release{version, commit, buildTime}`, `environment`, `platform: "server"`,
`device{brand: "server", model: GOOS/GOARCH, osVersion: Go version,
apiLevel: 0, appVersion, installId: InstanceID}`, `user{id, issuer}`,
`context{route, requestId, tags}`. Fields are capped to the backend's
limits on rune boundaries before sending (user, release, environment
and device, which the contract leaves open, at 200 bytes; tag keys are
truncated before the 10 tags are chosen). The body is encoded without
HTML escaping, so `<`, `>` and `&` stay one byte each. Control
characters, invalid UTF-8 and U+2028/9 are still escaped as six bytes
each, so a body that encodes above 63 KB has its trace cut, then its
message, until it fits under the backend's 64 KB limit.

The backend groups by source + type (or code) + normalized message (ids,
numbers, UUIDs, emails stripped) + the top 3 in-app stack frames, or the
action when there are no frames. So:

- `CaptureError(ctx, err, action, severity)` keeps the error text as the
  message, sets `type` from the error chain (`ErrorType`: the first
  non-wrapper Go type, e.g. `*fs.PathError`, else `error`), `code` from a
  `Coder` in the chain, the caller's stack as trace and the request id
  from ctx. `action` is a route template or operation name
  (`GET /orders/:id`, `eventpush.deliver`), never a raw URL.
- `Capture(ctx, report.Report{…})` sends a hand-built report; set
  `Fingerprint` to override grouping, `User{ID, Issuer}` for the user the
  request ran for (with an `scsk_` key the backend stores it as verified,
  the issuer defaulting to `backend:<source>`).

### Delivery

`Capture` / `CaptureError` never block: a bounded queue (`QueueSize`,
default 256) and one sender goroutine; drops (queue full, closed, circuit
open) are counted by `Dropped()` and logged at most once a minute. Every
string is copied when queued, so values handed out by Fiber (which alias
reused request buffers) are safe to pass. The sender follows the
contract:

| Answer | Behaviour |
|---|---|
| 202 | done |
| 3xx | never followed: `net/http` would copy the `X-Systicore-Key` header to the redirect target, even another host. Dropped (`ErrRejected`) and logged; point `REPORTS_URL` at the final address |
| 400, 404, 413, 422 | dropped (`ErrRejected`), logged with type/code/action |
| 401, 403 | dropped (`ErrUnauthorized`); the circuit opens for 5 minutes — nothing is sent (`ErrCircuitOpen`), one log line per opening |
| 429 | waits `Retry-After` (default 60 s, capped at 5 min), then retries |
| 5xx, network | retried with doubling backoff (`Backoff`, default 1 s), `Attempts` (default 3) in total |

`Submit` is the synchronous variant for scripts and tests.

### Fiber v3

```go
reporter := fiberv3.NewReporter(reports, fiberv3.ReportOptions{
    User: func(c fiber.Ctx) report.User { // optional; read what your auth middleware stored
        claims, _ := c.Locals("claims").(*auth.Claims)
        if claims == nil {
            return report.User{}
        }
        return report.User{ID: claims.Subject, Issuer: claims.Issuer}
    },
})
app := fiber.New(fiber.Config{ErrorHandler: reporter.ErrorHandler(errorHandler)})
app.Use(recover.New(reporter.RecoverConfig())) // replaces recover.New()
app.Use(fiberv3.Logging(root))
```

- `RecoverConfig()` sets recover's `PanicHandler`, which runs inside the
  deferred recover — before the stack unwinds — so the report carries the
  real panic site (`debug.Stack()` trimmed to the panicking frame),
  severity critical. It returns a `*report.PanicError` (the same `Error()`
  text as Fiber's default; `.Stack` for your own log line).
- `ErrorHandler(next)` wraps your handler (nil = Fiber's default), which
  still writes the response. Afterwards it reports only when the answer
  is a **5xx**, once (not again after a reported panic), with
  `Action = "<METHOD> <c.Route().Path>"`, the route, the request id
  (`logging.RequestID`, else the `X-Request-ID` response header), a
  `status` tag and the user. 4xx are expected traffic and never
  reported. These reports carry no trace on purpose: the error handler's
  stack is the same for every request, and without frames the backend
  groups per route.
- It also marks `c.Context()` as reported, so log your "request error"
  line with `.Ctx(c.Context())` and an attached `report.Hook` skips it.
- A 5xx is skipped only when the request's scope was really captured
  into (`Capture` / `CaptureError` on a `report.WithScope` ctx, see
  `report.Captured`). A handler's `report.MarkReported` silences its log
  line and nothing else, so the adapter still reports the 5xx.

### Fiber v2

Same API over `*fiber.Ctx`; the request's context is `c.UserContext()`.
Fiber v2's recover has no `PanicHandler`, so `RecoverConfig()` reports
from `StackTraceHandler` (with `EnableStackTrace: true`; the stderr dump
is replaced by the report):

```go
reporter := fiberv2.NewReporter(reports, fiberv2.ReportOptions{User: currentUser})
app := fiber.New(fiber.Config{ErrorHandler: reporter.ErrorHandler(errorHandler)})
app.Use(recover.New(reporter.RecoverConfig()))
app.Use(fiberv2.Logging(root))
```

### Goroutines, workers, jobs

No HTTP recover covers a goroutine. Defer `report.Recover` directly: it
captures a panic with the real stack (critical), logs it through
`zerolog.Ctx(ctx)` and swallows it, so one bad job cannot take the
process down. When the ctx carries no logger, the line goes to stderr
as JSON (where Go would have printed the crash), even with reporting
off, so a swallowed panic is never silent.

```go
go func() {
    defer report.Recover(ctx, reports, "orders.sync")
    syncOrders(ctx)
}()

for job := range jobs {
    jobContext := report.WithScope(ctx) // CaptureError marks it; the Hook then skips this job's log line
    if err := run(jobContext, job); err != nil {
        reports.CaptureError(jobContext, err, "push.deliver", report.SeverityError)
        jobLog.Error().Ctx(jobContext).Err(err).Msg("push failed")
    }
}
```

### log/slog services (conduo)

```go
slog.SetDefault(slog.New(reports.SlogHandler(slog.NewJSONHandler(os.Stdout, nil))))
```

Records at `slog.LevelError` and above become reports (code `log.error`,
trace from the logging call site); the first error attribute sets the
type and is appended to the message, so
`slog.ErrorContext(ctx, "sync failed", "err", err)` groups by the error.
The record's own attributes are searched first, then those bound with
`logger.With(...)`, so `logger.With("err", err).Error("sync failed")`
groups by the error too.
Records whose ctx is marked (see above) are skipped; every record still
reaches the wrapped handler. On a disabled client `SlogHandler` returns
the handler it was given.

### zerolog Hook — opt-in, not for HTTP error handlers

`reports.Hook()` turns Error/Fatal/Panic events into reports (code
`log.<level>`, trace from the logging call site, request id from the
event's ctx). A line without a message (`log.Error().Err(err).Send()`)
is reported with a placeholder message and groups by its call site.
Fatal and Panic lines are delivered before the hook returns (at most
2 s): zerolog exits or panics right after its hooks, and a queued
report would be lost. A zerolog hook sees only the level and the message — not
the fields, not the error. Error handlers log one constant line
("request error") for every 4xx and 5xx, so a hook on their logger folds
**every HTTP failure, 404s included, into a single "log.error / request
error" group** and double-reports what the Fiber adapter already
captured. Attach it only to loggers of background code that logs
instead of capturing:

```go
jobLog := root.Hook(reports.Hook()) // root itself stays hook-free: it is the client's Logger
```

Events whose ctx (`Event.Ctx(ctx)` / `Logger.With().Ctx(ctx)`) carries a
scope marked by `Capture`, `CaptureError` or `report.MarkReported` are
skipped. `MarkReported` is only for keeping a line out of the bridges: it
does not count as a capture for the Fiber adapters.

### Tests

`report/reporttest` is a fake ingest endpoint: point `BaseURL` at
`reporttest.NewServer(t).URL`, `Close` the client to flush, then assert
on `server.Requests()` (decoded body, headers). `NewStatusServer` and
`NewScriptedServer` script 401 / 429 / 5xx answers.

## Releasing

Publishing is a separate, explicitly confirmed step: it creates the
public GitHub repository and pushes tags. The order matters, because
once the `replace` is gone the adapters resolve the root through the
module proxy:

1. On a green `main` (CI runs gofmt, vet, `go test -race` and a
   `go mod tidy` diff in all three modules), tag and push the root:
   ```bash
   git tag v0.1.0 && git push origin v0.1.0
   ```
2. Point both adapters at that tag and drop the local `replace`. In
   `fiberv3/go.mod` and `fiberv2/go.mod` change
   `require github.com/Systicore/systicore_go v0.0.0` to `v0.1.0`, delete
   the line `replace github.com/Systicore/systicore_go => ../` together
   with the comment about it, then in each adapter run:
   ```bash
   GOWORK=off go mod tidy && GOWORK=off go test ./...
   ```
   `GOWORK=off` keeps `go.work` from masking a missing requirement; the
   workspace file itself stays for local development. Commit
   (`fiberv2, fiberv3: require systicore_go v0.1.0`) and push.
3. Tag the adapters on that commit and push the tags:
   ```bash
   git tag fiberv3/v0.1.0 && git tag fiberv2/v0.1.0
   git push origin fiberv3/v0.1.0 fiberv2/v0.1.0
   ```
4. Check from outside the workspace that the proxy serves all three:
   ```bash
   GOWORK=off go list -m github.com/Systicore/systicore_go@v0.1.0 \
       github.com/Systicore/systicore_go/fiberv3@v0.1.0 \
       github.com/Systicore/systicore_go/fiberv2@v0.1.0
   ```

Later releases repeat the order: root tag first, then bump the adapters'
`require`, then their tags. A release that changes only an adapter needs
only that adapter's tag.
