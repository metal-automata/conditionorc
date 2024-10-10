package app

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	runtime "github.com/banzaicloud/logrus-runtime-formatter"
	"github.com/metal-automata/conditionorc/internal/model"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

// Config holds configuration data when running mctl
// App holds attributes for the mtl application
type App struct {
	// Viper loads configuration parameters.
	v *viper.Viper
	// The kind of application - server/orchestrator
	AppKind model.AppKind
	// Flasher configuration.
	Config *Configuration
	// Logger is the app logger.
	Logger *logrus.Logger
}

// New returns returns a new instance of the conditionorc app
func New(_ context.Context, appKind model.AppKind, cfgFile string, loglevel model.LogLevel) (*App, <-chan os.Signal, error) {
	app := &App{
		v:       viper.GetViper(),
		AppKind: appKind,
		Config: &Configuration{
			file: cfgFile,
		},
		Logger: logrus.New(),
	}

	termCh := make(chan os.Signal, 1)

	if err := app.LoadConfiguration(); err != nil {
		return nil, termCh, err
	}

	app.SetLogger(loglevel)

	// register for SIGINT, SIGTERM
	signal.Notify(termCh, syscall.SIGINT, syscall.SIGTERM)

	return app, termCh, nil
}

func (a *App) SetLogger(level model.LogLevel) {
	runtimeFormatter := &runtime.Formatter{
		ChildFormatter: &logrus.JSONFormatter{},
		File:           true,
		Line:           true,
		BaseNameOnly:   true,
	}

	// set log level, format
	switch level {
	case model.LogLevelDebug:
		a.Logger.Level = logrus.DebugLevel

		// set runtime formatter options
		runtimeFormatter.BaseNameOnly = true
		runtimeFormatter.File = true
		runtimeFormatter.Line = true

	case model.LogLevelTrace:
		a.Logger.Level = logrus.TraceLevel

		// set runtime formatter options
		runtimeFormatter.File = true
		runtimeFormatter.Line = true
		runtimeFormatter.Package = true
	default:
		a.Logger.Level = logrus.InfoLevel
	}

	a.Logger.SetFormatter(runtimeFormatter)
}
