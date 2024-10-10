package cmd

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/equinix-labs/otel-init-go/otelinit"

	"github.com/metal-automata/conditionorc/internal/app"
	"github.com/metal-automata/conditionorc/internal/fleetdb"
	"github.com/metal-automata/conditionorc/internal/metrics"
	"github.com/metal-automata/conditionorc/internal/model"
	"github.com/metal-automata/conditionorc/internal/server"
	"github.com/metal-automata/conditionorc/internal/store"
	"github.com/metal-automata/conditionorc/internal/version"
	"github.com/metal-automata/rivets/events"
	"github.com/spf13/cobra"
)

var shutdownTimeout = 10 * time.Second

// install server command
var cmdServer = &cobra.Command{
	Use:   "server",
	Short: "Run condition orchestrator API service",
	Run: func(cmd *cobra.Command, _ []string) {
		app, termCh, err := app.New(cmd.Context(), model.AppKindServer, cfgFile, model.LogLevel(logLevel))
		if err != nil {
			log.Fatal(err)
		}

		streamBroker, err := events.NewStream(app.Config.NatsOptions)
		if err != nil {
			app.Logger.Fatal(err)
		}

		if err = streamBroker.Open(); err != nil {
			app.Logger.Fatal(err)
		}

		// setup cancel context with cancel func
		ctx, serverCancel := context.WithCancel(cmd.Context())

		repository, err := store.NewStore(app.Config, app.Logger, streamBroker)
		if err != nil {
			app.Logger.Fatal(err)
		}

		fleetDBClient, err := fleetdb.NewFleetDBClient(ctx, app.Config, app.Logger)
		if err != nil {
			app.Logger.Fatal(err)
		}

		// serve metrics on port 9090
		metrics.ListenAndServe()

		// the ignored parameter here is a context annotated with otel-init-go configuration
		_, otelShutdown := otelinit.InitOpenTelemetry(cmd.Context(), "conditionorc-api-server")

		options := []server.Option{
			server.WithLogger(app.Logger),
			server.WithListenAddress(app.Config.ListenAddress),
			server.WithStore(repository),
			server.WithFleetDBClient(fleetDBClient),
			server.WithStreamBroker(streamBroker, app.Config.NatsOptions.PublisherSubjectPrefix),
			server.WithConditionDefinitions(app.Config.ConditionDefinitions),
		}

		if app.OidcEnabled() {
			options = append(options, server.WithAuthMiddlewareConfig(app.Config.APIServerJWTAuth))
		}

		app.Logger.Info(version.Current().String())

		srv := server.New(options...)
		go func() {
			if err := srv.ListenAndServe(); err != nil && errors.Is(err, http.ErrServerClosed) {
				app.Logger.Fatal(err)
			}
		}()

		// sit around for term signal
		<-termCh
		app.Logger.Info("got TERM signal, shutting down server...")
		serverCancel()

		// call server shutdown with timeout
		ctx, cancel := context.WithTimeout(cmd.Context(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			app.Logger.Fatal("server shutdown error:", err)
		}
		otelShutdown(ctx)
	},
}

// install command flags
func init() {
	rootCmd.AddCommand(cmdServer)
}
