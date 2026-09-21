package logging_test

import (
	"context"
	"net/http"

	"github.com/rs/zerolog"
	"google.golang.org/grpc"

	"github.com/Systicore/systicore_go/logging"
	"github.com/Systicore/systicore_go/logging/grpclog"
)

// ExampleNew shows the wiring in a service's main: one root logger, chi and
// gRPC get it through the adapters, workers through their constructors.
func ExampleNew() {
	root, closeLog, err := logging.New(logging.Options{Service: "systicore_reports_backend", Level: "info"})
	if err != nil {
		panic(err)
	}
	defer closeLog()

	// net/http, chi, gorilla — all take the same func(http.Handler) http.Handler.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/admin/reports", func(w http.ResponseWriter, r *http.Request) {
		// Handlers and everything below them use the context logger.
		zerolog.Ctx(r.Context()).Info().Msg("listing reports")
	})
	_ = logging.Middleware(root)(mux)

	_ = grpc.NewServer(grpc.ChainUnaryInterceptor(grpclog.UnaryServerInterceptor(root)))
	_ = newWorker(root)
}

// A background worker has no request: it takes the logger explicitly and
// derives a per-run context, so repository code keeps using zerolog.Ctx.
type worker struct{ log zerolog.Logger }

func newWorker(root zerolog.Logger) *worker {
	return &worker{log: root.With().Str("component", "eventpush").Logger()}
}

func (w *worker) runOnce(ctx context.Context) {
	ctx = w.log.With().Str("run_id", "…").Logger().WithContext(ctx)
	zerolog.Ctx(ctx).Info().Msg("sweep")
}
