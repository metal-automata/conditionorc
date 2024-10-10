package cmd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/equinix-labs/otel-init-go/otelinit"

	"github.com/metal-automata/conditionorc/internal/app"
	"github.com/metal-automata/conditionorc/internal/fleetdb"
	"github.com/metal-automata/conditionorc/internal/metrics"
	"github.com/metal-automata/conditionorc/internal/model"
	"github.com/metal-automata/conditionorc/internal/orchestrator"
	"github.com/metal-automata/conditionorc/internal/orchestrator/notify"
	"github.com/metal-automata/conditionorc/internal/server"
	"github.com/metal-automata/conditionorc/internal/store"
	"github.com/metal-automata/conditionorc/internal/version"
	"github.com/metal-automata/rivets/events"
	"github.com/spf13/cobra"
)

var (
	facility string
)

// install orchestrator command
var cmdOrchestrator = &cobra.Command{
	Use:   "orchestrator",
	Short: "Run condition orchestrator service",
	Run: func(cmd *cobra.Command, _ []string) {
		app, termCh, err := app.New(cmd.Context(), model.AppKindOrchestrator, cfgFile, model.LogLevel(logLevel))
		if err != nil {
			log.Fatal(err)
		}

		_, otelShutdown := otelinit.InitOpenTelemetry(cmd.Context(), "conditionorc-orchestrator")
		defer otelShutdown(cmd.Context())

		// serve metrics
		metrics.ListenAndServe()

		// setup cancel context with cancel func
		ctx, cancelFunc := context.WithCancel(cmd.Context())

		// routine listens for termination signal and cancels the context
		go func() {
			<-termCh
			app.Logger.Info("got TERM signal, exiting...")
			cancelFunc()
		}()

		streamBroker, err := events.NewStream(app.Config.NatsOptions)
		if err != nil {
			app.Logger.Fatal(err)
		}

		if err = streamBroker.Open(); err != nil {
			app.Logger.Fatal(err)
		}

		repository, err := store.NewStore(app.Config, app.Logger, streamBroker)
		if err != nil {
			app.Logger.Fatal(err)
		}

		fleetDBClient, err := fleetdb.NewFleetDBClient(ctx, app.Config, app.Logger)
		if err != nil {
			app.Logger.Fatal(err)
		}

		// init Orchestrator API server
		optionsOrcAPI := []server.Option{
			server.WithLogger(app.Logger),
			server.WithListenAddress(app.Config.ListenAddress),
			server.WithStore(repository),
			server.WithFleetDBClient(fleetDBClient),
			server.WithStreamBroker(streamBroker, app.Config.NatsOptions.PublisherSubjectPrefix),
			server.WithConditionDefinitions(app.Config.ConditionDefinitions),
			server.WithOrchestratorAPI(facility),
		}

		if app.OidcEnabled() {
			optionsOrcAPI = append(optionsOrcAPI, server.WithAuthMiddlewareConfig(app.Config.APIServerJWTAuth))
		}

		app.Logger.Info(version.Current().String())

		srv := server.New(optionsOrcAPI...)
		go func() {
			if err := srv.ListenAndServe(); err != nil && errors.Is(err, http.ErrServerClosed) {
				app.Logger.Fatal(err)
			}
		}()

		notifier := notify.New(app.Logger, app.Config.Notifications)

		// init Orchestrator service
		options := []orchestrator.Option{
			orchestrator.WithLogger(app.Logger),
			orchestrator.WithListenAddress(app.Config.ListenAddress),
			orchestrator.WithStore(repository),
			orchestrator.WithStreamBroker(streamBroker),
			orchestrator.WithNotifier(notifier),
			orchestrator.WithFacility(facility),
			orchestrator.WithConditionDefs(app.Config.ConditionDefinitions),
			orchestrator.WithFleetDBClient(fleetDBClient),
		}

		if app.Config.NatsOptions.KVReplicationFactor > 0 {
			app.Logger.WithField("replication",
				fmt.Sprintf("%d", app.Config.NatsOptions.KVReplicationFactor)).
				Info("configuring status KV support")
			options = append(options, orchestrator.WithReplicas(app.Config.NatsOptions.KVReplicationFactor))
		}

		app.Logger.Info(version.Current().String())

		orc := orchestrator.New(options...)
		orc.Run(ctx)

		// shutdown server when orchestrator Run method returns
		ctx, cancel := context.WithTimeout(cmd.Context(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			app.Logger.Fatal("server shutdown error:", err)
		}
	},
}

// install command flags
func init() {
	pflags := cmdOrchestrator.PersistentFlags()
	pflags.StringVarP(&facility, "facility", "f", "", "a site-specific token to focus this orchestrator's activities")

	if err := cmdOrchestrator.MarkPersistentFlagRequired("facility"); err != nil {
		log.Fatal("marking facility as required:", err)
	}

	rootCmd.AddCommand(cmdOrchestrator)
}
