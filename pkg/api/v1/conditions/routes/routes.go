package routes

import (
	"fmt"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/metal-automata/conditionorc/internal/fleetdb"
	"github.com/metal-automata/conditionorc/internal/metrics"
	"github.com/metal-automata/conditionorc/internal/store"
	"github.com/metal-automata/rivets/events"
	"github.com/metal-automata/rivets/ginauth"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	v1types "github.com/metal-automata/conditionorc/pkg/api/v1/conditions/types"
	rctypes "github.com/metal-automata/rivets/condition"
)

const (
	PathPrefix = "/api/v1"
)

var pkgName = "pkg/api/v1/conditions/routes"

var ginNoOp = func(_ *gin.Context) {
}

// Routes type sets up the Conditions API router routes.
type Routes struct {
	authMW               *ginauth.MultiTokenMiddleware
	fleetDBClient        fleetdb.FleetDB
	repository           store.Repository
	streamBroker         events.Stream
	facilityCode         string
	streamSubjectPrefix  string
	conditionDefinitions rctypes.Definitions
	logger               *logrus.Logger
}

// Option type sets a parameter on the Routes type.
type Option func(*Routes)

// WithStore sets the storage repository on the routes type.
func WithStore(repository store.Repository) Option {
	return func(r *Routes) {
		r.repository = repository
	}
}

// WithFleetDBClient sets the client communicating with fleet db.
func WithFleetDBClient(client fleetdb.FleetDB) Option {
	return func(r *Routes) {
		r.fleetDBClient = client
	}
}

// WithStreamBroker sets the event stream broker.
func WithStreamBroker(broker events.Stream, streamSubjectPrefix string) Option {
	return func(r *Routes) {
		r.streamBroker = broker
		r.streamSubjectPrefix = streamSubjectPrefix
	}
}

func WithFacilityCode(fc string) Option {
	return func(r *Routes) {
		r.facilityCode = fc
	}
}

// WithLogger sets the logger on the routes type.
func WithLogger(logger *logrus.Logger) Option {
	return func(r *Routes) {
		r.logger = logger
	}
}

// WithAuthMiddleware sets the auth middleware on the routes type.
func WithAuthMiddleware(authMW *ginauth.MultiTokenMiddleware) Option {
	return func(r *Routes) {
		r.authMW = authMW
	}
}

// WithConditionDefinitions sets the supported condition types.
func WithConditionDefinitions(defs rctypes.Definitions) Option {
	return func(r *Routes) {
		r.conditionDefinitions = defs
	}
}

// apiHandler is a function that performs real work for the ConditionOrc API
type apiHandler func(c *gin.Context) (int, *v1types.ServerResponse)

// wrapAPICall wraps a conditionorc routine that does work with some prometheus
// metrics collection and returns a gin.HandlerFunc so the middleware can execute
// directly
func wrapAPICall(fn apiHandler) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		start := time.Now()
		endpoint := ctx.FullPath()
		responseCode, obj := fn(ctx)
		ctx.JSON(responseCode, obj)
		metrics.APICallEpilog(start, endpoint, responseCode)
	}
}

// NewRoutes returns a new conditionorc API routes with handlers registered.
func NewRoutes(options ...Option) (*Routes, error) {
	routes := &Routes{}

	for _, opt := range options {
		opt(routes)
	}

	if routes.repository == nil {
		return nil, errors.Wrap(ErrStore, "no store repository defined")
	}

	supported := []string{}
	for _, def := range routes.conditionDefinitions {
		supported = append(supported, string(def.Kind))
	}

	routes.logger.Debug(
		"routes initialized with support for conditions: ",
		strings.Join(supported, ", "),
	)

	return routes, nil
}

func (r *Routes) composeAuthHandler(scopes []string) gin.HandlerFunc {
	if r.authMW == nil {
		return ginNoOp
	}
	return r.authMW.AuthRequired(scopes)
}

// Routes returns routes for the Conditions API service.
func (r *Routes) Routes(g *gin.RouterGroup) {
	// API for server provision
	g.POST("/serverProvision", r.composeAuthHandler(createScopes("server")), wrapAPICall(r.serverProvision))

	servers := g.Group("/servers/:uuid")

	serverEnroll := g.Group("/serverEnroll")
	serverEnroll.POST("/:uuid", r.composeAuthHandler(createScopes("server")), wrapAPICall(r.serverEnroll))
	// Create a new server ID when uuid is not provided.
	serverEnroll.POST("/", r.composeAuthHandler(createScopes("server")), wrapAPICall(r.serverEnroll))
	servers.DELETE("", r.composeAuthHandler(createScopes("server")), wrapAPICall(r.serverDelete))

	{
		// Combined API for firmwareInstall
		servers.POST("/firmwareInstall", r.composeAuthHandler(createScopes("condition")),
			wrapAPICall(r.firmwareInstall))

		// BIOS
		servers.POST("/biosControl", r.composeAuthHandler(createScopes("condition")),
			wrapAPICall(r.biosControl))

		// Generalized API for any condition status (for cases where some server work
		// has multiple conditions involved and the caller doesn't know what they might be)
		servers.GET("/status", r.composeAuthHandler(readScopes("condition")),
			wrapAPICall(r.conditionStatus))

		// /servers/:uuid/condition/:conditionKind
		serverConditionBySlug := servers.Group("/condition")

		// Create a condition on a server.
		// XXX: refactor me! see comments
		serverConditionBySlug.POST("/:conditionKind",
			r.composeAuthHandler(createScopes("condition")),
			wrapAPICall(r.serverConditionCreate))
	}

	// fwValidate as a group doesn't assume you know a server that you want to act on. It is in the payload
	// in the first iteration but is not intended to be a parameter forever.
	fwValidate := g.Group("/validateFirmware")
	fwValidate.POST("", r.composeAuthHandler(createScopes("condition")), wrapAPICall(r.validateFirmware))
}

func createScopes(items ...string) []string {
	s := []string{"write", "create"}
	for _, i := range items {
		s = append(s, fmt.Sprintf("create:%s", i))
	}

	return s
}

func readScopes(items ...string) []string {
	s := []string{"read"}
	for _, i := range items {
		s = append(s, fmt.Sprintf("read:%s", i))
	}

	return s
}
