package app

import (
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/metal-automata/conditionorc/internal/model"
	"github.com/metal-automata/conditionorc/internal/orchestrator/notify"
	"github.com/metal-automata/rivets/events"
	"github.com/metal-automata/rivets/ginjwt"
	"github.com/pkg/errors"

	rctypes "github.com/metal-automata/rivets/condition"
)

var ErrConfig = errors.New("configuration error")

// Configuration holds application configuration read from a YAML or set by env variables.
//
// nolint:govet // prefer readability over field alignment optimization for this case.
type Configuration struct {
	// file is the configuration file path
	file string `mapstructure:"-"`

	// LogLevel is the app verbose logging level.
	// one of - info, debug, trace
	LogLevel string `mapstructure:"log_level"`

	// ListenAddress is the server listen address
	ListenAddress string `mapstructure:"listen_address"`

	// Concurrency declares how many events the orchestrator will process concurrently.
	Concurrency int `mapstructure:"concurrency"`

	// StoreKind indicates the kind of store to store server conditions
	// supported parameter value - NATS
	StoreKind model.StoreKind `mapstructure:"store_kind"`

	// APIServerJWTAuth sets the JWT verification configuration for the conditionorc API service.
	APIServerJWTAuth []ginjwt.AuthConfig `mapstructure:"ginjwt_auth"`

	// ConditionDefinitions holds one or more condition definitions the conditionorc API, orchestrator support.
	ConditionDefinitions rctypes.Definitions `mapstructure:"conditions"`

	// FleetDBAPIOptions defines the serverservice client configuration parameters
	//
	// This parameter is required when StoreKind is set to serverservice.
	FleetDBAPIOptions FleetDBAPIOptions `mapstructure:"serverservice"`

	// EventsBrokerKind indicates the kind of event broker configuration to enable,
	//
	// Supported parameter value - nats
	EventsBrokerKind model.EventBrokerKind `mapstructure:"events_broker_kind"`

	// NatsOptions defines the NATs events broker configuration parameters.
	//
	// This parameter is required when EventsBrokerKind is set to nats.
	NatsOptions events.NatsOptions `mapstructure:"nats"`

	// Notifications defines the properties for alerting external parties
	Notifications notify.Configuration `mapstructure:"notifications"`
}

// https://github.com/metal-automata/hollow-serverservice
type FleetDBAPIOptions struct {
	EndpointURL          *url.URL
	Endpoint             string   `mapstructure:"endpoint"`
	OidcIssuerEndpoint   string   `mapstructure:"oidc_issuer_endpoint"`
	OidcAudienceEndpoint string   `mapstructure:"oidc_audience_endpoint"`
	OidcClientSecret     string   `mapstructure:"oidc_client_secret"`
	OidcClientID         string   `mapstructure:"oidc_client_id"`
	OidcClientScopes     []string `mapstructure:"oidc_client_scopes"`
	DisableOAuth         bool     `mapstructure:"disable_oauth"`
}

func (a *App) LoadConfiguration() error {
	a.v.SetConfigType("yaml")
	a.v.SetEnvPrefix(model.AppName)
	a.v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	a.v.AutomaticEnv()

	fh, err := os.Open(a.Config.file)
	if err != nil {
		return errors.Wrap(err, ErrConfig.Error())
	}

	if err = a.v.ReadConfig(fh); err != nil {
		return errors.Wrap(err, ErrConfig.Error()+" "+a.Config.file)
	}

	if err := a.v.Unmarshal(a.Config); err != nil {
		return errors.Wrap(err, a.Config.file)
	}

	if err := a.envVarOverrides(); err != nil {
		return errors.Wrap(err, ErrConfig.Error())
	}

	return nil
}

func (a *App) OidcEnabled() bool {
	if a.v.GetBool("oidc.enabled") {
		if len(a.Config.APIServerJWTAuth) == 0 {
			a.Logger.Fatal("OIDC enabled without configuration")
		}
		a.Logger.Info("OIDC enabled")

		return true
	}

	a.Logger.Info("OIDC disabled")
	return false
}

func (a *App) envVarOverrides() error {
	if a.v.GetString("log.level") != "" {
		a.Config.LogLevel = a.v.GetString("log.level")
	}

	if a.v.GetString("app.kind") != "" {
		a.AppKind = model.AppKind(a.v.GetString("app.kind"))
	}

	if a.v.GetInt("concurrency") != 0 {
		a.Config.Concurrency = a.v.GetInt("concurrency")
	}

	if a.v.GetString("listen.address") != "" {
		a.Config.ListenAddress = a.v.GetString("listen.address")
	}

	if a.v.GetString("store.kind") != "" {
		a.Config.ListenAddress = a.v.GetString("store.kind")
	}

	if len(a.Config.ConditionDefinitions) == 0 {
		return errors.Wrap(ErrConfig, "expected one or more condition definitions")
	}

	if a.Config.EventsBrokerKind == model.NatsEventBroker {
		if err := a.envVarNatsOverrides(); err != nil {
			return err
		}
	}

	if a.Config.Notifications.Enabled {
		// load the token from the environment b/c it's a secret
		a.Config.Notifications.Token = a.v.GetString("notifications.token")
	}

	if err := a.envVarServerserviceOverrides(); err != nil {
		return err
	}

	return nil
}

// NATs streaming configuration
var (
	defaultNatsConnectTimeout = 100 * time.Millisecond
)

// nolint:gocyclo // nats env config load is cyclomatic
func (a *App) envVarNatsOverrides() error {
	if a.v.GetString("nats.url") != "" {
		a.Config.NatsOptions.URL = a.v.GetString("nats.url")
	}

	if a.Config.NatsOptions.URL == "" {
		return errors.New("missing parameter: nats.url")
	}

	if a.v.GetString("nats.publisherSubjectPrefix") != "" {
		a.Config.NatsOptions.PublisherSubjectPrefix = a.v.GetString("nats.publisherSubjectPrefix")
	}

	if a.Config.NatsOptions.PublisherSubjectPrefix == "" {
		return errors.New("missing parameter: nats.publisherSubjectPrefix")
	}

	if a.v.GetString("nats.stream.user") != "" {
		a.Config.NatsOptions.StreamUser = a.v.GetString("nats.stream.user")
	}

	if a.v.GetString("nats.stream.pass") != "" {
		a.Config.NatsOptions.StreamPass = a.v.GetString("nats.stream.pass")
	}

	if a.v.GetString("nats.creds.file") != "" {
		a.Config.NatsOptions.CredsFile = a.v.GetString("nats.creds.file")
	}

	if a.v.GetString("nats.stream.name") != "" {
		a.Config.NatsOptions.Stream.Name = a.v.GetString("nats.stream.name")
	}

	if a.v.GetDuration("nats.connect.timeout") != 0 {
		a.Config.NatsOptions.ConnectTimeout = a.v.GetDuration("nats.connect.timeout")
	}

	if a.Config.NatsOptions.ConnectTimeout == 0 {
		a.Config.NatsOptions.ConnectTimeout = defaultNatsConnectTimeout
	}
	return nil
}

// Server service configuration options

// nolint:gocyclo // parameter validation is cyclomatic
func (a *App) envVarServerserviceOverrides() error {
	if a.v.GetString("serverservice.endpoint") != "" {
		a.Config.FleetDBAPIOptions.Endpoint = a.v.GetString("serverservice.endpoint")
	}

	endpointURL, err := url.Parse(a.Config.FleetDBAPIOptions.Endpoint)
	if err != nil {
		return errors.New("serverservice endpoint URL error: " + err.Error())
	}

	a.Config.FleetDBAPIOptions.EndpointURL = endpointURL

	if a.v.GetString("serverservice.disable.oauth") != "" {
		a.Config.FleetDBAPIOptions.DisableOAuth = a.v.GetBool("serverservice.disable.oauth")
	}

	if a.Config.FleetDBAPIOptions.DisableOAuth {
		return nil
	}

	if a.v.GetString("serverservice.oidc.issuer.endpoint") != "" {
		a.Config.FleetDBAPIOptions.OidcIssuerEndpoint = a.v.GetString("serverservice.oidc.issuer.endpoint")
	}

	if a.Config.FleetDBAPIOptions.OidcIssuerEndpoint == "" {
		return errors.New("serverservice oidc.issuer.endpoint not defined")
	}

	if a.v.GetString("serverservice.oidc.audience.endpoint") != "" {
		a.Config.FleetDBAPIOptions.OidcAudienceEndpoint = a.v.GetString("serverservice.oidc.audience.endpoint")
	}

	if a.Config.FleetDBAPIOptions.OidcAudienceEndpoint == "" {
		return errors.New("serverservice oidc.audience.endpoint not defined")
	}

	if a.v.GetString("serverservice.oidc.client.secret") != "" {
		a.Config.FleetDBAPIOptions.OidcClientSecret = a.v.GetString("serverservice.oidc.client.secret")
	}

	if a.Config.FleetDBAPIOptions.OidcClientSecret == "" {
		return errors.New("serverservice oidc.client.secret not defined")
	}

	if a.v.GetString("serverservice.oidc.client.id") != "" {
		a.Config.FleetDBAPIOptions.OidcClientID = a.v.GetString("serverservice.oidc.client.id")
	}

	if a.Config.FleetDBAPIOptions.OidcClientID == "" {
		return errors.New("serverservice oidc.client.id not defined")
	}

	if a.v.GetString("serverservice.oidc.client.scopes") != "" {
		a.Config.FleetDBAPIOptions.OidcClientScopes = a.v.GetStringSlice("serverservice.oidc.client.scopes")
	}

	if len(a.Config.FleetDBAPIOptions.OidcClientScopes) == 0 {
		return errors.New("serverservice oidc.client.scopes not defined")
	}

	return nil
}
