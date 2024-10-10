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

	v1types "github.com/metal-automata/conditionorc/pkg/api/v1/orchestrator/types"
	rctypes "github.com/metal-automata/rivets/condition"
)

const (
	PathPrefix = "/api/v1"
)

var pkgName = "pkg/api/v1/orchestrator/routes"

var ginNoOp = func(_ *gin.Context) {
}

// Routes type sets up the conditionorc API  router routes.
type Routes struct {
	authMW               *ginauth.MultiTokenMiddleware
	fleetDBClient        fleetdb.FleetDB
	repository           store.Repository
	streamBroker         events.Stream
	facilityCode         string
	streamSubjectPrefix  string
	conditionDefinitions rctypes.Definitions
	logger               *logrus.Logger
	statusValueKV        statusValueKV
	taskKV               taskKV
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

// WithStatusKVPublisher sets the conditions status KV publisher.
func WithStatusKVPublisher(p statusValueKV) Option {
	return func(r *Routes) {
		r.statusValueKV = p
	}
}

// WithTaskKV sets the condition task queryor, publisher.
func WithTaskKV(t taskKV) Option {
	return func(r *Routes) {
		r.taskKV = t
	}
}

// apiHandler is a function that performs real work for the Orchestrator API
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

// NewRoutes returns a new Orchestrator API routes with handlers registered.
func NewRoutes(options ...Option) (*Routes, error) {
	routes := &Routes{}

	for _, opt := range options {
		opt(routes)
	}

	if routes.statusValueKV == nil {
		routes.statusValueKV = initStatusValueKV()
	}

	if routes.taskKV == nil {
		tkv, err := initTaskKVImpl(routes.facilityCode, routes.logger, routes.streamBroker)
		if err != nil {
			return nil, errors.Wrap(err, "task KV init error")
		}

		routes.taskKV = tkv
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

// Routes returns routes for the Orchestrator API service.
func (r *Routes) Routes(g *gin.RouterGroup) {
	controller := g.Group("/servers/:uuid")

	controller.PUT(
		"/condition-status/:conditionKind/:conditionID",
		r.composeAuthHandler(createScopes("orchestratorAPI")),
		wrapAPICall(r.conditionStatusUpdate),
	)

	controller.GET(
		"/condition-task/:conditionKind",
		r.composeAuthHandler(createScopes("orchestratorAPI")),
		wrapAPICall(r.taskQuery),
	)

	controller.POST(
		"/condition-task/:conditionKind/:conditionID",
		r.composeAuthHandler(createScopes("orchestratorAPI")),
		wrapAPICall(r.taskPublish),
	)

	controller.GET(
		"/condition",
		r.composeAuthHandler(readScopes("orchestratorAPI")),
		wrapAPICall(r.conditionGet),
	)
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
