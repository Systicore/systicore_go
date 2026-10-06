# systicore_go

Shared Go packages for every Systicore backend — the systicore_* services,
blacklab, bliss, biharvideke, mymosaiq, … — whatever HTTP stack they run.

## Layout: one framework-free core, adapters at the edge

```
systicore_go/                  module github.com/Systicore/systicore_go       ← core: stdlib + zerolog only
├── logging/                   New, Options, context helpers, Middleware (net/http)
├── logging/grpclog/           gRPC interceptors (compiled only when imported)
├── httpx/                     net/http toolkit: responses, client IP, recover, security headers, body limit
├── authn/                     users against the Systicore authenticator: JWT, permissions, CSRF (compiled only when imported)
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

| Module | Released | Next (see [Changelog](#changelog)) |
|---|---|---|
| root | `v0.1.0` | `v0.2.0` |
| fiberv3 | `fiberv3/v0.2.0` | `fiberv3/v0.3.0` |
| fiberv2 | `fiberv2/v0.2.0` | `fiberv2/v0.3.0` |

Consumers, once published (see [Releasing](#releasing)):

```bash
go get github.com/Systicore/systicore_go@v0.2.0
go get github.com/Systicore/systicore_go/fiberv3@v0.3.0   # Fiber v3 services
go get github.com/Systicore/systicore_go/fiberv2@v0.3.0   # Fiber v2 services
```

These are the versions this README documents. Probe lines at debug
(`logging.AccessLevel`) need the root at `v0.2.0` and the adapters at
`v0.3.0`; `WrittenServerErrors` needs the adapters at `v0.2.0` or later;
everything else works from `v0.1.0`.

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

The HTTP adapters write the `request` line at the level
`logging.AccessLevel(path, status)` returns: debug for a probe that
succeeded — `/health`, `/health/live`, `/health/ready`, `/healthz`,
`/ready`, `/metrics` with a status under 400 — so orchestrator and
Prometheus polling every few seconds does not bury real traffic, and info
for everything else, a failing probe included. `logging.IsProbePath` is
the same set, for a service that wants it elsewhere (rate-limit skips).

## httpx

The net/http toolkit for a service on chi (or any router taking
`func(http.Handler) http.Handler`). Lifted from systicore_reports_backend,
the first Systicore backend on chi, so the answers are still the ones the
Fiber services gave: a framework error is `{"error": <status text>}`, a
route nothing matches `{"error": "Cannot GET /path"}`.

| | |
|---|---|
| `ClientIP(ClientIPOptions)` | rewrites `r.RemoteAddr` to the bare client IP; `X-Forwarded-For` only from trusted proxies, walked from the nearest hop, so a forged leftmost entry is ignored. Mount it first |
| `Recover(RecoverOptions)` | panic → 500 and an error line on the request logger; `Stack` for development, `OnPanic` to hand it to an error reporter. After `logging.Middleware` |
| `SecurityHeaders(SecurityHeadersOptions)` | Fiber helmet's defaults written out; options for CORP, `X-Frame-Options`, CSP, HSTS |
| `MaxBody(max, reject)` | 413 up front for a declared length over the cap, `http.MaxBytesReader` for the rest |
| `Unless(skip, mw)`, `SkipPaths(mw, paths...)` | keep one middleware off some requests (probes, the Swagger UI) |
| `JSON`, `Error`, `StatusError`, `NotFound`, `MethodNotAllowed`, `DecodeJSON` | answers and body decoding; `DecodeJSON` returns `ErrBodyTooLarge` past the `MaxBody` cap |
| `IP(r)`, `ResponseRecorder` | the client address without a port; status and byte count for middleware that needs them |

The order the reports backend runs, and the one to copy:

```go
r := chi.NewRouter()
r.Use(httpx.ClientIP(proxyOptions))
r.Use(logging.Middleware(root))
r.Use(httpx.Recover(httpx.RecoverOptions{Stack: cfg.IsDevelopment()}))
r.Use(httpx.SecurityHeaders(httpx.SecurityHeadersOptions{CrossOriginResourcePolicy: "cross-origin"}))
r.Use(chimiddleware.GetHead) // the Dockerfile healthcheck sends HEAD
// CORS, rate limiter (httpx.SkipPaths for the probes), metrics …
r.Use(httpx.MaxBody(cfg.Server.BodyLimit, nil))
r.NotFound(httpx.NotFound)
r.MethodNotAllowed(httpx.MethodNotAllowed)
```

## authn

Authenticates a request against the Systicore authenticator — the code
each of the four backends kept a copy of, with the answers they all gave
(`{"error":"invalid or expired token"}` and the rest, byte for byte: the
web client keys its "session expired" handling on them). A package, not a
module: jwt, keyfunc and singleflight are compiled only where it is
imported.

```go
keys, err := authn.FetchJWKS(appCtx, cfg.Auth.IssuerURL+"/api/v1/oauth/jwks", 15*time.Second)
// development may boot on without it: every token then gets a 401
verifier := authn.NewVerifier(authn.VerifierOptions{Keyfunc: keys, Issuer: cfg.Auth.IssuerURL})

r.Route("/api/v1", func(v1 chi.Router) {
    v1.Use(authn.CSRF(authn.CSRFOptions{Secure: …, SameSite: …, Domain: ".systicore.hu"}))
    v1.Use(authn.Authenticate(authn.AuthOptions{Verifier: verifier, CookieName: cfg.Auth.SessionCookieName, DevFallback: cfg.IsDevelopment()}))
    v1.Use(authn.ResolvePermissions(authn.PermissionOptions{Source: authn.UserInfo(cfg.Auth.IssuerURL, nil), Cache: appCache}))
    v1.With(authn.RequirePermission("systicore.reports.read")).Get("/admin/reports", h.List)
})
```

- `FetchJWKS(ctx, url, timeout)`: `ctx` is the server's lifetime, it ends
  the background refresh. Pass the context main cancels on shutdown, not a
  boot timeout: that stops the refresh seconds after start, and a key the
  authenticator withdraws stays trusted until the next restart. `timeout`
  bounds the first fetch and each refresh.
- `Verifier`: RS256/ES256, the pinned issuer (required), the audience when
  set, an expiry in the future. The username comes from
  `preferred_username`, the claim the authenticator issues.
- `Authenticate`: the user goes into the context (`PrincipalFrom`,
  `UserIDFrom`) and `user_id` onto the request logger. `DevFallback` runs
  a token-less request as user 1 — development only.
- `ResolvePermissions`: `UserInfo` asks `GET /api/v1/user` with the
  caller's own credential; the authenticator plugs in its store with
  `PermissionSourceFunc` and no cache. Concurrent requests of one user
  share a lookup. **A failed lookup answers 503 at `RequirePermission`**,
  not a 403 that blames the user for an outage.
- `CSRF`: the double-submit cookie; a Bearer request skips it. Mount it
  before `Authenticate`, so a forged request gets the 403 and not a 401.

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
app.Use(reporter.WrittenServerErrors(fiberv3.WrittenErrorOptions{ // 5xx a handler wrote itself
    Cause: func(c fiber.Ctx) error { // optional: the error your "log and answer 500" helper stored
        cause, _ := c.Locals(localServerError).(error)
        return cause
    },
    Code: func(c fiber.Ctx) string { // optional: the code your error envelope answered with
        code, _ := c.Locals(localErrorCode).(string)
        return code
    },
    Skip: func(c fiber.Ctx) bool { return c.Route().Path == "/health" }, // optional
}))
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
- `WrittenServerErrors(options)` (fiberv3 v0.2.0) covers the handlers
  that write a 5xx themselves — `c.Status(500).JSON(…)`, then `return
  nil` — which hand Fiber no error, so the ErrorHandler never sees them.
  After the chain it reports a 5xx status once, at severity error, with
  the same action, route, request id, status tag and user. The response
  body is never read: it can echo internal details. Without a cause the
  report is code `http.<status>` (or the app's own code, see `Code`)
  with the message `handler answered 503 Service Unavailable`, no type
  and no trace, so the backend groups it per route, status and code.
  - `Cause` returns the error the app recorded for the 5xx, usually from
    `c.Locals`; the report is then built from it like a returned error
    (type from the chain, code from a `report.Coder`, message the error
    text). A nil return falls back to the status-only report. Such a
    report always has a type (the error's Go type, or `error`), and the
    backend groups by type before code: causes of one type whose
    messages normalize alike share a group whatever their `Coder` codes,
    unless `Error()` includes the code.
  - `Code` returns the application code the handler answered with, e.g.
    the error code its response envelope carried, stored in `c.Locals`.
    A non-empty code replaces `http.<status>` in a report without a
    cause; that report keeps no type, so different codes on one route
    and status stay separate groups. With a cause, the cause's own
    `Coder` code wins and `Code` fills in only when it has none.
  - `Skip` leaves expected 5xx out, e.g. a health probe's 503 that
    restates an outage the failing routes already report. It runs after
    the handler, so `c.Route()` is the route that answered.
  - A panic inside any hook is swallowed (Cause → status-only report,
    Code → `http.<status>` or the cause's code, Skip → reported); the
    written answer always reaches the client.
- Nothing is reported twice, in whatever order the middlewares are
  registered: a returned error passes through to the ErrorHandler (also
  when Fiber's `logger` middleware calls it inside the chain and returns
  nil), a panic stays with the recover reporter, and every path marks
  the request as reported, so registering the middleware twice is
  harmless too. The middleware installs `report.WithScope` on
  `c.Context()`, so a handler's own `Capture` / `CaptureError` there
  counts and the 5xx it then writes is not reported again.
- Replacing an app's own "report written 5xx" middleware: pass its
  cause getter as `Cause`, its response-code getter as `Code` and its
  route exclusions as `Skip`. An app that records only a response code
  passes it as `Code`, not wrapped in a `report.Coder` error for
  `Cause`: the wrapper would give every report a type, which then
  decides the group instead of the code. A glue that reported
  `{code: <response code>, message: "handler answered <status>
  <text>"}` with no type keeps its groups with `Code` alone (except its
  reports without a code, which become `http.<status>`). If the glue
  reported a different type or message, the first occurrence after the
  switch opens a new group per route.

### Fiber v2

Same API over `*fiber.Ctx`; the request's context is `c.UserContext()`.
Fiber v2's recover has no `PanicHandler`, so `RecoverConfig()` reports
from `StackTraceHandler` (with `EnableStackTrace: true`; the stderr dump
is replaced by the report). `WrittenServerErrors` (fiberv2 v0.2.0)
installs its scope on `c.UserContext()`:

```go
reporter := fiberv2.NewReporter(reports, fiberv2.ReportOptions{User: currentUser})
app := fiber.New(fiber.Config{ErrorHandler: reporter.ErrorHandler(errorHandler)})
app.Use(recover.New(reporter.RecoverConfig()))
app.Use(fiberv2.Logging(root))
app.Use(reporter.WrittenServerErrors(fiberv2.WrittenErrorOptions{Cause: recordedCause}))
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

## Changelog

### v0.2.0, fiberv3/v0.3.0, fiberv2/v0.3.0 — unreleased

Minor releases. The adapters need the root at `v0.2.0`, so this is a full
release (see [Releasing](#releasing)).

- Added `authn`: access-token verification (`FetchJWKS`, `Verifier`),
  `Authenticate`, `ResolvePermissions` with `UserInfo`, `RequirePermission`,
  `CSRF`. Lifted from the four backends' copies, with their answers. It
  pins the issuer on every token (one copy did not), answers 503 instead
  of 403 when the permission lookup fails (one copy did, three did not),
  reads the username from `preferred_username` (three copies read a
  `username` claim the authenticator never issues), and keeps the JWKS
  refresh alive for the server's lifetime (three copies stopped it 15
  seconds after boot).
- Added `httpx`, the net/http toolkit for services on chi: `ClientIP`,
  `Recover`, `SecurityHeaders`, `MaxBody`, `Unless`, `SkipPaths` and the
  answer helpers. Lifted from systicore_reports_backend; the bodies are
  the Fiber ones, except that a panic's 500 now reads
  `{"error":"Internal Server Error"}` in every service (it was
  "internal server error" in three of them and "Internal server error"
  in the fourth). `Recover` gained `OnPanic` for an error reporter.
- Added `logging.AccessLevel` and `logging.IsProbePath`. `Middleware`,
  `fiberv3.Logging` and `fiberv2.Logging` write the access line of a probe
  that succeeded at debug instead of info; a failing probe stays at info.
  A service logging at info no longer prints its health checks and
  scrapes; at debug they are back. Taken from systicore_reports_backend,
  which carried it in its copy of `logging` with a shorter path list.
- Root, tests only: the fake ingest keys are built at runtime from
  repeated characters, so secret scanners have no key-shaped
  high-entropy literal to flag.

### fiberv3/v0.2.0, fiberv2/v0.2.0 — 2026-09-25

Minor releases: new API, nothing existing changes behaviour. Both still
require the root at `v0.1.0`.

- Added `Reporter.WrittenServerErrors(WrittenErrorOptions)`, the
  middleware for 5xx answers a handler writes itself and returns nil
  for, which the `ErrorHandler` never sees (see [Fiber v3](#fiber-v3)).
  It reports each such 5xx once, never from the response body, and
  cooperates with the `ErrorHandler` and `RecoverConfig` so a request is
  reported by whichever path saw it first and by no other. It replaces
  the app-side middlewares reverba_backend, spike_backend and
  tarp-crm_backend built for this.
- Added `WrittenErrorOptions.Cause`, the documented hook for the error an
  app recorded for a written 5xx (e.g. in `c.Locals`),
  `WrittenErrorOptions.Code` for the response code an app recorded
  instead (it names a report without a cause and keeps it without a
  type, so each code groups on its own), and `WrittenErrorOptions.Skip`
  for expected 5xx such as a health probe's.
- `ErrorHandler`, `RecoverConfig` and `ReportOptions.User` behave as in
  v0.1.0; they now share the "already reported" mark with the new
  middleware.

### v0.1.0, fiberv3/v0.1.0, fiberv2/v0.1.0

First release: `logging` with the net/http, gRPC and Fiber adapters, the
`report` client for the key-based ingest (contract v1.1), and the Fiber
panic and 5xx reporters (`RecoverConfig`, `ErrorHandler`).

## Releasing

Publishing is a separate, explicitly confirmed step: it pushes tags. The
adapters require the root by version, with no `replace`, so they resolve
it through the module proxy and the order matters:

1. On a green `main` (CI runs gofmt, vet, `go test -race` and a
   `go mod tidy` diff in all three modules), tag and push the root when
   it changed, e.g.:
   ```bash
   git tag vX.Y.Z && git push origin vX.Y.Z
   ```
2. When an adapter needs that new root API, bump its `require` to the
   tag just pushed — never to an untagged version — then in that adapter
   run:
   ```bash
   GOWORK=off go get github.com/Systicore/systicore_go@vX.Y.Z
   GOWORK=off go mod tidy && GOWORK=off go test ./...
   ```
   `GOWORK=off` keeps `go.work` from masking a missing requirement; the
   workspace file itself stays for local development. Commit
   (`fiberv2, fiberv3: require systicore_go vX.Y.Z`) and push.
3. Tag the adapters on that commit (`fiberv3/vA.B.C`, `fiberv2/vA.B.C`)
   and push the tags.
4. Check from outside the workspace that the proxy serves them:
   ```bash
   GOWORK=off go list -m github.com/Systicore/systicore_go@vX.Y.Z \
       github.com/Systicore/systicore_go/fiberv3@vA.B.C
   ```

A release that changes only an adapter skips steps 1 and 2.

The pending release is the full kind: the adapters call
`logging.AccessLevel`, which only the root at `v0.2.0` has. Until their
`require` is bumped, `GOWORK=off go build` in an adapter fails with
`undefined: logging.AccessLevel`, while CI, which builds inside the
workspace, stays green — so all four steps, in order:

```bash
git tag v0.2.0 && git push origin v0.2.0
(cd fiberv3 && GOWORK=off go get github.com/Systicore/systicore_go@v0.2.0 && GOWORK=off go mod tidy && GOWORK=off go test ./...)
(cd fiberv2 && GOWORK=off go get github.com/Systicore/systicore_go@v0.2.0 && GOWORK=off go mod tidy && GOWORK=off go test ./...)
git commit -am "fiberv2, fiberv3: require systicore_go v0.2.0" && git push
git tag fiberv3/v0.3.0 && git tag fiberv2/v0.3.0
git push origin fiberv3/v0.3.0 fiberv2/v0.3.0
GOWORK=off go list -m github.com/Systicore/systicore_go@v0.2.0 \
    github.com/Systicore/systicore_go/fiberv3@v0.3.0 \
    github.com/Systicore/systicore_go/fiberv2@v0.3.0
```
