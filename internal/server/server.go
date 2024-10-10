package server

import (
	"net/http"
	"time"

	docs "github.com/metal-automata/conditionorc/docs"
	swaggerfiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"

	"github.com/gin-gonic/gin"
	"github.com/metal-automata/conditionorc/internal/fleetdb"
	"github.com/metal-automata/conditionorc/internal/store"
	condRoutes "github.com/metal-automata/conditionorc/pkg/api/v1/conditions/routes"
	orcRoutes "github.com/metal-automata/conditionorc/pkg/api/v1/orchestrator/routes"
	"github.com/metal-automata/rivets/events"
	"github.com/metal-automata/rivets/ginauth"
	"github.com/metal-automata/rivets/ginjwt"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	rctypes "github.com/metal-automata/rivets/condition"
)

var (
	// Request read timeout.
	readTimeout = 10 * time.Second
	// Request write timeout.
	writeTimeout = 20 * time.Second

	ErrRoutes = errors.New("error in routes")
)

type kind string

const (
	ConditionsAPI   kind = "conditionsAPI"
	OrchestratorAPI kind = "orchestratorAPI"
)

// Server type holds attributes of the condition orc server
type Server struct {
	authMWConfigs        []ginjwt.AuthConfig
	kind                 kind // server kind
	logger               *logrus.Logger
	streamBroker         events.Stream
	streamSubjectPrefix  string
	listenAddress        string
	facilityCode         string
	conditionDefinitions rctypes.Definitions
	repository           store.Repository
	fleetDBClient        fleetdb.FleetDB
}

// Option type sets a parameter on the Server type.
type Option func(*Server)

// WithStore sets the storage repository on the Server type.
func WithStore(repository store.Repository) Option {
	return func(s *Server) {
		s.repository = repository
	}
}

// WithFleetDBClient sets the client communicating with the fleet db.
func WithFleetDBClient(client fleetdb.FleetDB) Option {
	return func(s *Server) {
		s.fleetDBClient = client
	}
}

// WithLogger sets the logger on the Server type.
func WithLogger(logger *logrus.Logger) Option {
	return func(s *Server) {
		s.logger = logger
	}
}

// WithListenAddress sets the Server listen address.
func WithListenAddress(addr string) Option {
	return func(s *Server) {
		s.listenAddress = addr
	}
}

// WithStreamBroker sets the event stream broker.
func WithStreamBroker(broker events.Stream, streamSubjectPrefix string) Option {
	return func(s *Server) {
		s.streamBroker = broker
		s.streamSubjectPrefix = streamSubjectPrefix
	}
}

// WithConditionDefinitions sets the supported condition types.
func WithConditionDefinitions(defs rctypes.Definitions) Option {
	return func(s *Server) {
		s.conditionDefinitions = defs
	}
}

// WithAuthMiddlewareConfig sets the auth middleware configuration.
func WithAuthMiddlewareConfig(authMWConfigs []ginjwt.AuthConfig) Option {
	return func(s *Server) {
		s.authMWConfigs = authMWConfigs
	}
}

// By default the Condition API routes are registered,
//
// With this option the server will register the Orchestrator API routes.
func WithOrchestratorAPI(facilityCode string) Option {
	return func(s *Server) {
		s.kind = OrchestratorAPI
		s.facilityCode = facilityCode
	}
}

func New(opts ...Option) *http.Server {
	s := &Server{
		kind: ConditionsAPI,
	}

	for _, opt := range opts {
		opt(s)
	}

	g := gin.New()
	g.Use(loggerMiddleware(s.logger), gin.Recovery())

	g.GET("/healthz/readiness", s.ping)

	var authMW *ginauth.MultiTokenMiddleware
	if s.authMWConfigs != nil {
		var err error
		authMW, err = ginjwt.NewMultiTokenMiddlewareFromConfigs(s.authMWConfigs...)
		if err != nil {
			s.logger.Fatal("failed to initialize auth middleware: ", err)
		}
	}

	switch s.kind {
	case ConditionsAPI:
		s.ConditionsAPIRoutes(g, authMW)
	case OrchestratorAPI:
		s.OrchestratorAPIRoutes(g, authMW)
	default:
		s.logger.Fatal("unexpected server kind: ", s.kind)
	}

	g.NoRoute(func(c *gin.Context) {
		c.JSON(http.StatusNotFound, gin.H{"message": "invalid request - route not found"})
	})

	// Swagger Doc API Endpoint. <IP:Port>/api/v1/docs/index.html is the URL you want
	docs.SwaggerInfo.BasePath = "/api/v1"
	g.GET("/api/v1/docs/*any", ginSwagger.WrapHandler(swaggerfiles.Handler))

	return &http.Server{
		Addr:         s.listenAddress,
		Handler:      g,
		ReadTimeout:  readTimeout,
		WriteTimeout: writeTimeout,
	}
}

func (s *Server) ConditionsAPIRoutes(g *gin.Engine, authMW *ginauth.MultiTokenMiddleware) {
	options := []condRoutes.Option{
		condRoutes.WithLogger(s.logger),
		condRoutes.WithStore(s.repository),
		condRoutes.WithFleetDBClient(s.fleetDBClient),
		condRoutes.WithStreamBroker(s.streamBroker, s.streamSubjectPrefix),
		condRoutes.WithConditionDefinitions(s.conditionDefinitions),
	}

	if authMW != nil {
		options = append(options, condRoutes.WithAuthMiddleware(authMW))
	}

	v1CondRouter, err := condRoutes.NewRoutes(options...)
	if err != nil {
		s.logger.Fatal(errors.Wrap(err, ErrRoutes.Error()))
	}

	v1CondRouter.Routes(g.Group(condRoutes.PathPrefix))
	s.logger.Info("Condition API server routes registered.")
}

func (s *Server) OrchestratorAPIRoutes(g *gin.Engine, authMW *ginauth.MultiTokenMiddleware) {
	options := []orcRoutes.Option{
		orcRoutes.WithLogger(s.logger),
		orcRoutes.WithStore(s.repository),
		orcRoutes.WithFleetDBClient(s.fleetDBClient),
		orcRoutes.WithStreamBroker(s.streamBroker, s.streamSubjectPrefix),
		orcRoutes.WithConditionDefinitions(s.conditionDefinitions),
		orcRoutes.WithFacilityCode(s.facilityCode),
	}

	if authMW != nil {
		options = append(options, orcRoutes.WithAuthMiddleware(authMW))
	}

	v1OrcRouter, err := orcRoutes.NewRoutes(options...)
	if err != nil {
		s.logger.Fatal(errors.Wrap(err, ErrRoutes.Error()))
	}

	v1OrcRouter.Routes(g.Group(orcRoutes.PathPrefix))
	s.logger.Info("Orchestrator API server routes registered.")
}

// this is a placeholder for a more comprehensive readiness check
func (s *Server) ping(c *gin.Context) {
	// XXX: the repository Ping method was removed because it returns a nil error unconditionally
	c.JSON(http.StatusOK, gin.H{
		"status": "UP",
	})
}
