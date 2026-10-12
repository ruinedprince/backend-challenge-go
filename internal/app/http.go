package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"time"

	"go.uber.org/fx"
)

type HTTPServer struct {
	server          *http.Server
	listener        net.Listener
	shutdownTimeout time.Duration
	logger          *slog.Logger
}

func NewHTTPServer(config Config, logger *slog.Logger, readiness *Readiness, metrics *Metrics, api *API) *HTTPServer {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", healthLive)
	mux.HandleFunc("GET /health/ready", readiness.Handler)
	mux.HandleFunc("GET /metrics", metrics.Handler)
	mux.HandleFunc("POST /wagering/transactions", api.CreateTransaction)
	mux.HandleFunc("GET /wagering/transactions/{transactionId}", api.GetTransaction)
	mux.HandleFunc("GET /providers/{providerId}/wagering/transactions/{externalTransactionId}", api.GetExternalTransaction)
	mux.HandleFunc("POST /wallets", api.OpenWallet)
	mux.HandleFunc("GET /wallets/{walletId}", api.GetWallet)
	mux.HandleFunc("GET /wallets/{walletId}/ledger", api.GetLedger)
	mux.HandleFunc("POST /wallets/{walletId}/reconciliation", api.ReconcileWallet)

	return &HTTPServer{
		server:          &http.Server{Addr: config.HTTPAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second},
		shutdownTimeout: config.ShutdownTimeout,
		logger:          logger,
	}
}

func RegisterHTTPServer(lifecycle fx.Lifecycle, httpServer *HTTPServer) {
	lifecycle.Append(fx.Hook{
		OnStart: func(context.Context) error {
			listener, err := net.Listen("tcp", httpServer.server.Addr)
			if err != nil {
				return err
			}
			httpServer.listener = listener
			go func() {
				httpServer.logger.Info("http server started", "address", listener.Addr().String())
				if err := httpServer.server.Serve(listener); err != nil && err != http.ErrServerClosed {
					httpServer.logger.Error("http server stopped unexpectedly", "error", err)
				}
			}()
			return nil
		},
		OnStop: func(context.Context) error {
			ctx, cancel := context.WithTimeout(context.Background(), httpServer.shutdownTimeout)
			defer cancel()
			return httpServer.server.Shutdown(ctx)
		},
	})
}

func healthLive(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]string{"status": "live"})
}

func writeJSON(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}
