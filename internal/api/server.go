package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/actions"
	"github.com/devopsmike2/squadron/internal/ai"
	"github.com/devopsmike2/squadron/internal/api/handlers"
	"github.com/devopsmike2/squadron/internal/api/middleware"
	"github.com/devopsmike2/squadron/internal/billing"
	"github.com/devopsmike2/squadron/internal/configs"
	"github.com/devopsmike2/squadron/internal/costspikes"
	"github.com/devopsmike2/squadron/internal/deploy"
	"github.com/devopsmike2/squadron/internal/discovery/azureconnstore"
	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/gcpconnstore"
	"github.com/devopsmike2/squadron/internal/discovery/iacconnstore"
	"github.com/devopsmike2/squadron/internal/discovery/ociconnstore"
	"github.com/devopsmike2/squadron/internal/events"
	"github.com/devopsmike2/squadron/internal/incidents"
	"github.com/devopsmike2/squadron/internal/insights"
	"github.com/devopsmike2/squadron/internal/inventory"
	"github.com/devopsmike2/squadron/internal/metrics"
	"github.com/devopsmike2/squadron/internal/pipelinehealth"
	"github.com/devopsmike2/squadron/internal/pricing"
	"github.com/devopsmike2/squadron/internal/proposer"
	"github.com/devopsmike2/squadron/internal/proposer/verdictsel"
	"github.com/devopsmike2/squadron/internal/recommendations"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
)

// AgentCommander defines the interface for sending commands to agents.
//
// SendConfigToAgentWithContext is the trace-aware variant used by the
// per-agent direct-push handler — it propagates the per-push span
// context into the OpAMP CustomMessage so an OTel-aware agent can join
// the originating trace. SendConfigToAgent stays for non-traced and
// group-fanout callers and to preserve back-compat for downstream
// embedders.
type AgentCommander interface {
	SendConfigToAgent(agentId uuid.UUID, configContent string) error
	SendConfigToAgentWithContext(ctx context.Context, agentId uuid.UUID, configContent string) error
	RestartAgent(agentId uuid.UUID) error
	RestartAgentsInGroup(groupId string) ([]uuid.UUID, []error)
	SendConfigToAgentsInGroup(groupId string, configContent string) ([]uuid.UUID, []error)
}

// AuthConfig controls the API auth middleware. When Enabled is true,
// every /api/v1/* request must carry a valid Bearer token; /metrics
// and /health stay public. When false, no auth middleware is mounted
// and the API behaves as it did pre-v0.8 — useful for development,
// dangerous in production.
type AuthConfig struct {
	Enabled bool
}

// Server represents the HTTP API server
type Server struct {
	router            *gin.Engine
	agentService      services.AgentService
	telemetryService  services.TelemetryQueryService
	savedQueryService services.SavedQueryService
	alertService      services.AlertService
	auditService      services.AuditService
	rolloutService    services.RolloutService
	authService       services.AuthService
	authConfig        AuthConfig
	commander         AgentCommander
	broker            *events.Broker
	configsTracer     *configs.Tracer   // optional; nil disables config-push spans on direct handler pushes
	opampPort         int               // v0.27.1: the OpAMP port we tell quickstart-generated agents to dial
	insightsService   *insights.Service // optional; nil disables the /api/v1/insights/* routes (no telemetry reader configured)
	recsEngine        *recommendations.Engine
	recsDismissals    handlers.DismissalStore  // optional; nil disables /api/v1/recommendations/* (paired with recsEngine)
	aiService         *ai.Service              // optional; nil keeps /api/v1/ai/status responsive (returns enabled=false) but mutation routes 503
	pricer            *pricing.Projector       // optional; v0.27 $/month projection. nil → /api/v1/pricing/* returns enabled=false
	costSpikes        handlers.CostSpikeStore  // optional; v0.29 cost-spike alerting storage
	costSpikeDetector *costspikes.Detector     // optional; nil disables /tick + the background detector loop
	pipelineHealth    *pipelinehealth.Service  // optional; v0.31 collector self-metrics surface — nil → /api/v1/pipeline-health/* returns 503
	inventory         *inventory.Service       // optional; v0.32 expected-vs-actual reconciliation — nil → /api/v1/inventory/* returns 503
	deploy            *deploy.Service          // optional; v0.34 GitHub Actions deploy trigger — nil or Enabled()==false → /api/v1/deploy/* returns 503
	billingProvider   billing.SnapshotProvider // optional; v0.42 — nil → /api/v1/billing/snapshot returns 204
	siemService       services.SiemService     // optional; v0.50.2 — nil → /api/v1/siem/* returns 503
	// v0.85 Stream 2C — discovery substrate. The credstore is
	// optional: the validate endpoint doesn't read or write it
	// (zero records by design), so a nil credStore still serves
	// the wizard's test-before-commit flow. The trampoline 503s
	// when the entire discovery surface is unwired so the test
	// server's existing no-config posture stays intact.
	discoveryCredStore credstore.Store
	// v0.85 Stream 2D — discovery credstore Key. Optional at
	// construction; SetDiscoveryCredKey wires the key the substrate
	// was opened with so the Save handler can encrypt
	// AWSCredentials with the same key the later scan engine
	// decrypts them with. Without it, the Save endpoint 500s with a
	// "key not wired" humanized error; the validate endpoint stays
	// unaffected because it never touches the store.
	discoveryCredKey *credstore.Key
	// v0.85 Stream 2F — discovery-side AI proposer. Optional: only
	// the recommendations route consumes it; the list / scan /
	// validate / save routes never call it. A nil service makes the
	// recommendations route 503 with a clear "AI assist not
	// configured" message; the rest of the discovery surface stays
	// reachable. Wired by main.go right beside the credstore block.
	discoveryAIService *ai.Service
	// v0.89.3 Stream 19 — Connect IaC repo substrate. Sibling to
	// discoveryCredStore: the IaC-connection rows live in a separate
	// SQLite database (iacconnstore.db) but reuse the same
	// credstore.Key for sealing the PAT. Optional at construction —
	// when nil, the /api/v1/iac/github/* routes 503 with a clear
	// "IaC connect not configured" message; the rest of the discovery
	// surface stays unaffected.
	iacConnStore iacconnstore.Store
	// v0.89.47 (#667 Stream 67, GCP discovery slice 1 chunk 3) — GCP
	// discovery substrate. Optional at construction; the
	// /api/v1/discovery/gcp/* routes 503 when the store is unwired
	// (test_server.go path stays unaffected). The GCP credstore key
	// reuses s.discoveryCredKey — chunk 1 of #667 sealed the SA JSON
	// under the same SQUADRON_SECRETS_KEY the AWS / IaC paths use,
	// with a domain-tagged AAD ("squadron.gcp_sa.v1") that prevents
	// cross-shape unsealing.
	discoveryGCPStore          gcpconnstore.Store
	discoveryGCPScannerFactory handlers.GCPScannerFactory
	// v0.89.52 (#676 Stream 74, Azure discovery slice 1 chunk 3) — Azure
	// discovery substrate. Optional at construction; the
	// /api/v1/discovery/azure/* routes 503 when the store is unwired
	// (test_server.go path stays unaffected). The Azure credstore key
	// reuses s.discoveryCredKey — chunk 1 of #674 sealed the SP
	// client_secret under the same SQUADRON_SECRETS_KEY the AWS / GCP
	// / IaC paths use, with a domain-tagged AAD
	// ("squadron.azure_client_secret.v1") that prevents cross-shape
	// unsealing.
	discoveryAzureStore          azureconnstore.Store
	discoveryAzureScannerFactory handlers.AzureScannerFactory
	// v0.89.57 (#683 Stream 81, OCI discovery slice 1 chunk 3) — OCI
	// discovery substrate. Optional at construction; the
	// /api/v1/discovery/oci/* routes 503 when the store is unwired
	// (test_server.go path stays unaffected). The OCI credstore key
	// reuses s.discoveryCredKey — chunk 1 of #681 sealed the RSA
	// private key under the same SQUADRON_SECRETS_KEY the AWS / GCP /
	// Azure / IaC paths use, with a domain-tagged AAD
	// ("squadron.oci_signing_key.v1") that prevents cross-shape
	// unsealing. OCI is the fourth sealed credential type — defense-
	// in-depth posture extends across PAT, webhook secret, GCP SA,
	// Azure SP client_secret, AND OCI API Signing Key private key.
	discoveryOCIStore          ociconnstore.Store
	discoveryOCIScannerFactory handlers.OCIScannerFactory
	// v0.89.61 (#688 Stream 86, Unified Discovery dashboard slice 1
	// chunk 1) — unified summary handler. Lazily constructed by the
	// summary trampoline on first request so the cache lives across
	// requests rather than being reset per call. The summary handler
	// nil-tolerantly wraps every per-provider store (a deployment
	// that hasn't wired any provider still serves the welcome
	// empty-state response). Per design doc §5.1 the cache TTL is
	// 30s; the constructor pins that via DefaultSummaryCacheTTL.
	discoverySummaryHandler *handlers.DiscoverySummaryHandlers
	summaryHandlerOnce      sync.Once
	// v0.89.76 (#707 Stream 105, Trace integration slice 1 chunk 3) —
	// unified trace coverage handler. Same lazily-constructed pattern as
	// discoverySummaryHandler so the 30s cache lives across requests
	// rather than being reset per call. The handler nil-tolerantly wraps
	// every per-provider store (a deployment that hasn't wired any
	// provider still serves a 200 with zero counts per provider). The
	// traceIndexForDiscovery field is wired post-construction via
	// SetTraceIndexForDiscovery — nil leaves the endpoint serving
	// all-zero coverage (the same posture as a deployment that hasn't
	// observed any spans yet). Per design doc §7 the cache TTL is 30s;
	// DefaultTraceCoverageCacheTTL pins that.
	discoveryTraceCoverageHandler *handlers.DiscoveryTraceCoverageHandlers
	traceCoverageHandlerOnce      sync.Once
	traceIndexForDiscovery        handlers.TraceIndex
	// v0.89.86 (#717 Stream 115, Span quality slice 1 chunk 2) — the
	// span-quality dashboard + per-resource detail handler. Same
	// lazily-constructed pattern as discoveryTraceCoverageHandler so
	// the 30s aggregate cache lives across requests rather than being
	// rebuilt per call. qualitySnapshotIndexForDiscovery /
	// resourceKeyProjectorForDiscovery are wired post-construction
	// via SetQualitySnapshotIndexForDiscovery /
	// SetResourceKeyProjectorForDiscovery — nil leaves the endpoints
	// serving cold-start zeros / 404 respectively.
	discoverySpanQualityHandler *handlers.DiscoverySpanQualityHandlers
	spanQualityHandlerOnce      sync.Once

	// discoveryWorkloadHealthHandler — v0.89.132 (#772 Stream 170,
	// Workload Health dashboard panel slice 1 chunk 1). Same lazily-
	// constructed pattern as discoverySpanQualityHandler so the 30s
	// aggregate cache lives across requests rather than being rebuilt
	// per call. workloadHealthInventoryReader is wired post-
	// construction via SetWorkloadHealthInventoryReader — nil leaves
	// the endpoint serving every provider as zero-counts (the panel
	// hides itself on the UI side, matching the design doc §5.3 hide
	// conditions). Per design doc §6 the cache TTL is 30s;
	// DefaultWorkloadHealthCacheTTL pins that.
	discoveryWorkloadHealthHandler *handlers.DiscoveryWorkloadHealthHandlers
	workloadHealthHandlerOnce      sync.Once
	workloadHealthInventoryReader  handlers.ServerlessHealthInventoryReader

	// discoveryServerlessColdStartHandler — v0.89.114 (#752 Stream 150,
	// Cold-start latency slice 1 chunk 2). The per-resource cold-start
	// endpoint handler. Built lazily by discoveryServerlessColdStartTrampoline
	// the first time the route is hit; nil store at construction time
	// short-circuits to 404 the same way the trace-coverage handler
	// degrades when its substrate isn't wired.
	discoveryServerlessColdStartHandler *handlers.DiscoveryServerlessColdStartHandlers
	coldStartHandlerOnce                sync.Once
	coldStartObservationReader          handlers.ColdStartObservationReader

	// discoveryServerlessSamplingHandler — v0.89.123 (#763 Stream 161,
	// Sampling rate analysis slice 1 chunk 2). Per-resource sampling
	// endpoint handler. Built lazily by
	// discoveryServerlessSamplingTrampoline the first time the route
	// is hit; nil lookup OR nil detector at construction time
	// short-circuits to 404. Same degrade-when-substrate-not-wired
	// posture as the cold-start handler from v0.89.114.
	discoveryServerlessSamplingHandler *handlers.DiscoveryServerlessSamplingHandlers
	samplingHandlerOnce                sync.Once
	samplingResourceLookup             handlers.SamplingResourceLookup
	samplingDetector                   handlers.SamplingDetector

	// discoveryServerlessErrorRateHandler — v0.89.128 (#768 Stream
	// 166, Error rate correlation slice 1 chunk 2). The per-resource
	// error-rate endpoint handler. Built lazily by
	// discoveryServerlessErrorRateTrampoline the first time the
	// route is hit; nil store at construction time short-circuits to
	// 404 the same way the cold-start handler from v0.89.114
	// degrades when its substrate isn't wired.
	discoveryServerlessErrorRateHandler *handlers.DiscoveryServerlessErrorRateHandlers
	errorRateHandlerOnce                sync.Once
	errorRateObservationReader          handlers.ErrorRateObservationReader
	qualitySnapshotIndexForDiscovery    handlers.QualitySnapshotIndex
	resourceKeyProjectorForDiscovery    handlers.ResourceKeyProjector
	// traceIndexLookupForDiscovery — v0.89.77 (#708 Stream 106,
	// Trace integration slice 1 chunk 4) — the LastSeenAt-shaped
	// slice of the traceindex consumed by the per-provider scan
	// handlers' annotation step. Production wires the same
	// *traceindex.Index this struct's traceIndexForDiscovery field
	// holds (the type satisfies both Coverage and LastSeenAt
	// interfaces); keeping it as a separate field keeps the chunk-3
	// stubTraceIndex test type unchanged (it only implements
	// Coverage). A nil lookup leaves every scan response's rows with
	// LastSeenAt unset — the UI then renders "never" for every
	// resource, matching the cold-start posture.
	traceIndexLookupForDiscovery handlers.TraceIndexLookup
	// v0.89.23 Stream 40 (#639) — GitHub webhook listener secret.
	// Cached at startup from os.Getenv(SQUADRON_GITHUB_WEBHOOK_SECRET);
	// an empty value leaves the /api/v1/webhooks/github route mounted
	// but 503ing with a clear "set SQUADRON_GITHUB_WEBHOOK_SECRET"
	// body so the operator reading the GitHub webhook delivery log
	// sees exactly which knob to turn. The webhook route is PUBLIC
	// by design — the HMAC signature IS the authentication; GitHub
	// does not carry a bearer token Squadron could validate.
	iacGitHubWebhookSecret []byte
	// v0.89.30 (#649) — webhook delivery dedupe store. Wired post-
	// construction via SetIaCGitHubWebhookStore. Nil-safe — the
	// handler logs a warning on every inbound delivery when the
	// store isn't wired, but legitimate flows keep working. Production
	// callers always wire it with the same applicationstore the
	// rest of the server uses; the cmd/all-in-one wiring also starts
	// the background GC sweep via handlers.StartWebhookDedupeGC.
	iacGitHubWebhookStore handlers.WebhookDedupeStore
	// v0.89.43 (#663 Stream 61, slice 1 chunk 2 of the GitHub Checks
	// API back-signal arc). Optional wires consumed by the chunk-2
	// follow-up on the IaC PR-open handler:
	//   - iacChecksClient: the *iacgithub.PATClient that posts the
	//     check-run create. Per-request PAT is supplied at call time
	//     so a single client serves every operator. Nil leaves the
	//     follow-up dormant (slice-1 fail-open posture for
	//     deployments upgrading PAT scope).
	//   - squadronHost: base URL the "View in Squadron" deep link in
	//     the check-run summary targets. Empty value suppresses the
	//     link line.
	//   - checkRunName: operator override for the slice-1 default
	//     "Squadron recommendation" name (design doc §11 Q2).
	iacChecksClient handlers.ChecksAPI
	squadronHost    string
	checkRunName    string
	// v0.89.44 (#664 Stream 62, slice 1 chunk 3 of the GitHub Checks
	// API back-signal arc). Optional wires consumed by the chunk-3
	// follow-up on the webhook handler when an inbound merge / close
	// event lands:
	//   - iacWebhookChecksClient: the *iacgithub.PATClient that issues
	//     the UpdateCheckRun PATCH. Production wires the SAME client
	//     used for iacChecksClient (chunk 2) since one
	//     *iacgithub.PATClient satisfies both interfaces; the
	//     field is separately typed because the two interfaces
	//     deliberately stay narrow per their respective chunks. Nil
	//     leaves the chunk-3 follow-up dormant.
	//   - iacWebhookChecksPAT: the deployment-wide PAT used to
	//     authenticate the UpdateCheckRun PATCH. The webhook
	//     receiver has no per-request operator credential (events
	//     come from GitHub via HMAC); the PAT is supplied at startup
	//     and reused for every chunk-3 update. Empty keeps the
	//     follow-up dormant. Slice 2 candidate: per-connection PAT
	//     lookup off the connection row.
	iacWebhookChecksClient handlers.WebhookChecksAPI
	iacWebhookChecksPAT    string
	// v0.89.44 (#665 Stream 63, slice 1 chunk 4) — deployment-wide PAT
	// used by the discovery exclusion handler's PATCH-to-neutral
	// follow-up. The discovery handler does not unseal the IaC
	// credstore (it lives in a different connection model), so the
	// chunk-4 follow-up uses an explicit PAT wired at deployment
	// startup. Per design doc §3 option A this is the same Checks-API-
	// scoped PAT the IaC connection's open-PR path consumes. Empty
	// value keeps the chunk-4 follow-up dormant (fail-open posture).
	iacChecksPAT string
	// accessAuditMiddleware records an api.request audit event for
	// every authenticated mutating request. Wired by the build-edition
	// layer in cmd/all-in-one: OSS leaves it nil (middleware unmounted,
	// state-change events still recorded by the service layer);
	// Compliance Pack wires middleware.APIAccessAudit which produces
	// per-call evidence for NIST CSF PR.AA-04 and CIP-007-6 R4.1.2.
	// Added in v0.52.
	accessAuditMiddleware gin.HandlerFunc
	// v0.53 Move 2 — action runner. The actions handler needs the
	// raw applicationstore to read/write action_runner_registrations
	// and action_requests; it also needs the Ed25519 signer to
	// produce signed action requests. Both wired post-construction
	// via SetActionStoreAndSigner so main.go can load the signing
	// key from env after NewServer.
	appStore     applicationstore.ApplicationStore
	actionSigner *actions.Signer
	// v0.54 Move 3 — incident drafter publishers. Map of provider
	// name to Publisher. Always contains the clipboard publisher;
	// main.go conditionally registers github when the matching env
	// vars are set.
	incidentsPublishers incidents.PublisherRegistry
	logger              *zap.Logger
	httpServer          *http.Server
	metrics             *metrics.APIMetrics
	registry            *prometheus.Registry
}

// NewServer creates a new API server.
//
// The caller owns the Prometheus registry — pass the same registry used to
// register OpAMP, OTLP, and worker metrics so that /metrics exposes a single,
// unified view of the process. (Previously this constructor created its own
// registry, which silently hid every non-API metric from /metrics.)
func NewServer(agentService services.AgentService, telemetryService services.TelemetryQueryService, savedQueryService services.SavedQueryService, alertService services.AlertService, auditService services.AuditService, rolloutService services.RolloutService, authService services.AuthService, authConfig AuthConfig, commander AgentCommander, broker *events.Broker, configsTracer *configs.Tracer, registry *prometheus.Registry, logger *zap.Logger) *Server {
	// Set Gin to release mode for production
	gin.SetMode(gin.ReleaseMode)

	router := gin.New()

	// Initialize API metrics on the caller-provided registry.
	metricsFactory := metrics.NewPrometheusFactory("squadron", registry)
	apiMetrics := metrics.NewAPIMetrics(metricsFactory)

	// Add middleware
	router.Use(gin.Recovery())
	router.Use(corsMiddleware())
	router.Use(loggingMiddleware(logger))
	// OTel trace propagation: extracts the W3C traceparent header on
	// inbound requests into the gin context, and creates a server
	// span named by the route. When selftel is disabled, the global
	// propagator + tracer are no-ops so this layer is effectively
	// free. Mounted ABOVE auth so the span exists even for 401
	// rejections — operators trace-debugging a misauthed CI run can
	// still find their request in the trace UI.
	router.Use(otelgin.Middleware("squadron"))

	server := &Server{
		router:            router,
		agentService:      agentService,
		telemetryService:  telemetryService,
		savedQueryService: savedQueryService,
		alertService:      alertService,
		auditService:      auditService,
		rolloutService:    rolloutService,
		authService:       authService,
		authConfig:        authConfig,
		commander:         commander,
		broker:            broker,
		configsTracer:     configsTracer,
		logger:            logger,
		metrics:           apiMetrics,
		registry:          registry,
	}

	// Add metrics middleware
	router.Use(server.metricsMiddleware())

	// Register routes
	server.registerRoutes()

	return server
}

// insightsTrampoline late-binds an insights handler call so the
// route table can be registered before SetInsightsService is
// called. Returns a gin.HandlerFunc that resolves s.insightsService
// at request time; 503s with a clear error if still nil.
func (s *Server) insightsTrampoline(fn func(*handlers.InsightsHandlers, *gin.Context)) gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.insightsService == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": "Telemetry Volume Insights are not available — no telemetry backend wired",
			})
			return
		}
		h := handlers.NewInsightsHandlers(s.insightsService, s.logger)
		fn(h, c)
	}
}

// SetInsightsService wires the (optional) insights query service.
// When unset, the /api/v1/insights/* routes return 503 — operators
// running without a telemetry backend (e.g. a test harness) don't
// see the routes break, just respond "not available".
//
// Setter pattern rather than a constructor argument so existing
// callers (test_server.go and friends) don't grow yet another
// positional parameter for an optional feature.
func (s *Server) SetInsightsService(svc *insights.Service) {
	s.insightsService = svc
}

// SetRecommendationsEngine wires the v0.25 recommendations engine
// + its dismissals store. Both go together — an engine without a
// store can still Evaluate but can't honor dismissals. When unset,
// the /api/v1/recommendations/* routes return 503 with a clear
// message (same trampoline pattern as the insights routes).
func (s *Server) SetRecommendationsEngine(engine *recommendations.Engine, dismissals handlers.DismissalStore) {
	s.recsEngine = engine
	s.recsDismissals = dismissals
}

// SetOpAMPPort tells the Server which port the OpAMP server is
// listening on. v0.27.1 uses this to construct the dial URL that
// the Quickstart wizard hands to operators (the API runs on a
// different port from OpAMP, so the request Host alone isn't
// enough). Defaults to 4320 if never set.
func (s *Server) SetOpAMPPort(p int) { s.opampPort = p }

// SetPricer wires the v0.27 pricing projector. Always non-nil at
// runtime (main.go always constructs one — disabled state lives
// inside the projector). The pricingTrampoline still guards
// against nil for the test_server.go path.
func (s *Server) SetPricer(p *pricing.Projector) { s.pricer = p }

// SetCostSpikes wires the v0.29 cost-spike alerting layer: the
// storage slice (always the application store) + an optional
// detector. When the detector is non-nil, the server's Start
// will also launch the background Tick loop. When the store is
// nil, the /alerts/cost-spikes routes 503.
func (s *Server) SetCostSpikes(store handlers.CostSpikeStore, det *costspikes.Detector) {
	s.costSpikes = store
	s.costSpikeDetector = det
}

// SetPipelineHealth wires the v0.31 collector-self-metrics surface.
// nil disables the /api/v1/pipeline-health/* routes (503) — this is
// the right state for the test_server.go path that doesn't have a
// telemetry reader, since the service needs DuckDB to function.
func (s *Server) SetPipelineHealth(svc *pipelinehealth.Service) {
	s.pipelineHealth = svc
}

// SetInventory wires the v0.32 expected-vs-actual reconciliation
// surface. Always non-nil at production runtime (main.go constructs
// it unconditionally against the application store). The
// nil-guard exists for the test_server.go path.
func (s *Server) SetInventory(svc *inventory.Service) {
	s.inventory = svc
}

// SetDeploy wires the v0.34 GitHub Actions deploy surface. Pass
// nil (or a service whose Enabled() returns false) to disable the
// /api/v1/deploy/* routes (they 503). Disabled is the right state
// when SQUADRON_DEPLOY_KEY is unset — main.go decides.
// SetBillingProvider wires the v0.42 billing connector. Pass nil to
// disable — the /api/v1/billing/snapshot endpoint returns 204 in
// that case and the UI's billing tile silently hides.
func (s *Server) SetBillingProvider(p billing.SnapshotProvider) {
	s.billingProvider = p
}

func (s *Server) SetDeploy(svc *deploy.Service) {
	s.deploy = svc
}

// SetSiemService wires the v0.50.2 SIEM export management surface.
// Pass nil to disable — the /api/v1/siem/* routes return 503 in that
// case (correct state when SQUADRON_SIEM_KEY is unset and the
// crypter couldn't be built).
func (s *Server) SetSiemService(svc services.SiemService) {
	s.siemService = svc
}

// SetAccessAuditMiddleware installs the per-request audit middleware
// the v0.52 build edition split moves out of the open core's hard
// wire. The wire layer in cmd/all-in-one calls this once after
// NewServer; OSS passes nil (or skips the call) so the middleware
// stays unmounted, Compliance Pack installs middleware.APIAccessAudit
// so every authenticated mutating request lands in audit_events as
// an api.request row. Safe to call with nil — the middleware is only
// mounted when this is non-nil at Run time.
func (s *Server) SetAccessAuditMiddleware(m gin.HandlerFunc) {
	s.accessAuditMiddleware = m
}

// SetActionStoreAndSigner wires the action runner dependencies
// post-construction. Called from main.go after NewServer so the
// signer can be loaded from the env without changing the server
// constructor signature. Safe to call with a nil signer: dispatch
// returns 503 in that case but read endpoints (list / get / poll)
// keep working.
func (s *Server) SetActionStoreAndSigner(store applicationstore.ApplicationStore, signer *actions.Signer) {
	s.appStore = store
	s.actionSigner = signer
}

// SetIncidentsPublishers swaps in a populated publisher registry
// after construction. main.go uses this to register the GitHub
// Issues publisher when SQUADRON_GITHUB_ISSUES_* env vars are set.
// Safe to call with nil — the publish endpoint falls back to the
// stamp only behavior.
func (s *Server) SetIncidentsPublishers(p incidents.PublisherRegistry) {
	s.incidentsPublishers = p
}

// pricingTrampoline mirrors insightsTrampoline. The /pricing/*
// routes are read-only and gracefully degrade — when the pricer
// is unwired we 503; when it's wired but disabled the handler
// returns enabled=false at 200.
func (s *Server) pricingTrampoline(fn func(*handlers.PricingHandlers, *gin.Context)) gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.pricer == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error":   "Pricing service is not wired",
				"enabled": false,
			})
			return
		}
		// PricingHandlers needs insights for the projection
		// endpoint. Reuse the one already wired via SetInsightsService.
		// When insights is nil, projection returns zero — that's fine
		// for the test_server.go path.
		h := handlers.NewPricingHandlers(s.pricer, s.insightsService, s.logger)
		fn(h, c)
	}
}

// SetAIService wires the (optional) v0.26 AI-assist service. The
// service is constructed unconditionally in main.go — it
// short-circuits with ErrDisabled when no API key is configured —
// so passing a non-nil service here is the right default. The
// nil-guard exists for the test_server.go path that doesn't wire
// AI at all.
func (s *Server) SetAIService(svc *ai.Service) {
	s.aiService = svc
}

// SetDiscoveryCredStore wires the v0.85 discovery credential
// substrate. The validate endpoint itself does NOT read or write the
// store — it constructs a transient CloudConnection from the request
// body and runs the AWS assume-role probe in memory — but the
// store's presence is what gates the discovery surface as a whole
// being available. A nil store at request time means "discovery
// isn't wired here" and the trampoline 503s rather than running a
// half-configured probe.
//
// Setter pattern mirrors the other optional services: NewServer's
// signature doesn't grow, and the test_server.go path can leave the
// surface disabled by skipping the call.
func (s *Server) SetDiscoveryCredStore(store credstore.Store) {
	s.discoveryCredStore = store
}

// SetDiscoveryCredKey wires the credstore encryption key the Save
// handler uses to seal AWSCredentials before they reach the substrate.
// Optional in the same posture as SetDiscoveryCredStore: a nil key
// leaves Save 500ing with a humanized "key not wired" error while
// Validate keeps working (Validate creates zero records).
//
// Production callers pass the key the credstore was opened with so the
// ciphertext stored at Save time decrypts cleanly when the (future)
// scan engine reads it back. Calling this without also calling
// SetDiscoveryCredStore is a no-op for the wizard.
func (s *Server) SetDiscoveryCredKey(key *credstore.Key) {
	s.discoveryCredKey = key
}

// SetDiscoveryAIService wires the v0.85 Stream 2F discovery-side AI
// proposer. The recommendations route — POST
// /api/v1/discovery/aws/connections/:id/recommendations — depends on
// it; the list / scan / validate / save routes do not. A nil service
// (the test_server.go and AI-disabled deployments path) leaves only the
// recommendations route 503ing; the rest of the discovery surface
// stays reachable.
//
// Mirrors the SetDiscoveryCredStore + SetDiscoveryCredKey setter
// pattern: NewServer's signature doesn't grow, and the wiring lives at
// main.go right beside the credstore block so the gap that prompted
// the credstore follow-up commit doesn't recur for AI.
func (s *Server) SetDiscoveryAIService(svc *ai.Service) {
	s.discoveryAIService = svc
}

// SetIaCConnStore wires the Stream 19 IaC-connection substrate onto
// the API server. Optional in the same posture as SetDiscoveryCredStore:
// a nil store leaves /api/v1/iac/github/* routes 503ing with a clear
// message; the rest of the discovery surface stays reachable.
//
// The IaC connect flow reuses the credstore.Key wired by
// SetDiscoveryCredKey to seal the GitHub PAT — Phase 2 of #609 does
// not introduce a second secrets key. Calling SetIaCConnStore
// without also calling SetDiscoveryCredKey leaves Save / Open-PR
// 500ing with a "key not wired" humanized error.
func (s *Server) SetIaCConnStore(store iacconnstore.Store) {
	s.iacConnStore = store
}

// SetGCPDiscoveryStore wires the v0.89.47 (#667 chunk 3) GCP
// connection substrate onto the API server. Optional in the same
// posture as SetDiscoveryCredStore: a nil store leaves the
// /api/v1/discovery/gcp/* routes 503ing with a clear "GCP discovery
// not configured" message; the rest of the discovery surface stays
// reachable.
//
// The GCP path reuses the credstore.Key wired by SetDiscoveryCredKey —
// chunk 1 (#667 v0.89.46) sealed the SA JSON under the same
// SQUADRON_SECRETS_KEY the AWS / IaC paths use, with a domain-tagged
// AAD ("squadron.gcp_sa.v1") preventing cross-shape unsealing.
// Calling SetGCPDiscoveryStore without also calling
// SetDiscoveryCredKey leaves Create / Validate / Scan 500ing with
// a "key not wired" humanized error.
func (s *Server) SetGCPDiscoveryStore(store gcpconnstore.Store) {
	s.discoveryGCPStore = store
}

// SetGCPDiscoveryScannerFactory wires the v0.89.47 (#667 chunk 3) GCP
// scanner factory. Production wires a factory that instantiates the
// chunk-2 *gcp.Scanner with the unsealed SA JSON; tests substitute a
// fake that returns a pre-canned scanner.Scanner. A nil factory
// leaves Validate / Scan 500ing with a humanized error; the CRUD
// routes (Create / List / Get / Update / Delete) stay unaffected.
//
// The setter pattern keeps NewServer's signature stable and mirrors
// the SetDiscoveryCredStore / SetDiscoveryAIService posture. Chunk 2
// and chunk 3 ship in parallel worktrees; main.go composes the
// concrete factory once both land in main.
func (s *Server) SetGCPDiscoveryScannerFactory(f handlers.GCPScannerFactory) {
	s.discoveryGCPScannerFactory = f
}

// SetAzureDiscoveryStore wires the v0.89.52 (#676 chunk 3) Azure
// connection substrate onto the API server. Optional in the same
// posture as SetGCPDiscoveryStore: a nil store leaves the
// /api/v1/discovery/azure/* routes 503ing with a clear "Azure
// discovery not configured" message; the rest of the discovery
// surface stays reachable.
//
// The Azure path reuses the credstore.Key wired by
// SetDiscoveryCredKey — chunk 1 (#674 v0.89.51) sealed the SP
// client_secret under the same SQUADRON_SECRETS_KEY the AWS / GCP /
// IaC paths use, with a domain-tagged AAD
// ("squadron.azure_client_secret.v1") preventing cross-shape
// unsealing. Calling SetAzureDiscoveryStore without also calling
// SetDiscoveryCredKey leaves Create / Validate / Scan 500ing with
// a "key not wired" humanized error.
func (s *Server) SetAzureDiscoveryStore(store azureconnstore.Store) {
	s.discoveryAzureStore = store
}

// SetAzureDiscoveryScannerFactory wires the v0.89.52 (#676 chunk 3)
// Azure scanner factory. Production wires a factory that
// instantiates the chunk-2 *azure.Scanner with the unsealed SP
// client_secret; tests substitute a fake that returns a pre-canned
// scanner.Scanner. A nil factory leaves Validate / Scan 500ing with
// a humanized error; the CRUD routes (Create / List / Get / Update /
// Delete) stay unaffected.
//
// The setter pattern keeps NewServer's signature stable and mirrors
// the SetGCPDiscoveryScannerFactory posture. Chunk 2 and chunk 3
// ship in parallel worktrees; main.go composes the concrete factory
// once both land in main.
func (s *Server) SetAzureDiscoveryScannerFactory(f handlers.AzureScannerFactory) {
	s.discoveryAzureScannerFactory = f
}

// SetOCIDiscoveryStore wires the v0.89.57 (#683 chunk 3) OCI
// connection substrate onto the API server. Optional in the same
// posture as SetAzureDiscoveryStore: a nil store leaves the
// /api/v1/discovery/oci/* routes 503ing with a clear "OCI discovery
// not configured" message; the rest of the discovery surface stays
// reachable.
//
// The OCI path reuses the credstore.Key wired by SetDiscoveryCredKey
// — chunk 1 (#681 v0.89.56) sealed the RSA private key under the
// same SQUADRON_SECRETS_KEY the AWS / GCP / Azure / IaC paths use,
// with a domain-tagged AAD ("squadron.oci_signing_key.v1")
// preventing cross-shape unsealing. Calling SetOCIDiscoveryStore
// without also calling SetDiscoveryCredKey leaves Create / Validate
// / Scan 500ing with a "key not wired" humanized error.
func (s *Server) SetOCIDiscoveryStore(store ociconnstore.Store) {
	s.discoveryOCIStore = store
}

// SetOCIDiscoveryScannerFactory wires the v0.89.57 (#683 chunk 3)
// OCI scanner factory. Production wires a factory that instantiates
// the chunk-2 *oci.Scanner with the unsealed RSA private key; tests
// substitute a fake that returns a pre-canned scanner.Scanner. A
// nil factory leaves Validate / Scan 500ing with a humanized error;
// the CRUD routes (Create / List / Get / Update / Delete) stay
// unaffected.
//
// The setter pattern keeps NewServer's signature stable and mirrors
// the SetAzureDiscoveryScannerFactory posture. Chunk 2 and chunk 3
// ship in parallel worktrees; main.go composes the concrete factory
// once both land in main.
func (s *Server) SetOCIDiscoveryScannerFactory(f handlers.OCIScannerFactory) {
	s.discoveryOCIScannerFactory = f
}

// SetIaCGitHubWebhookSecret wires the HMAC secret the GitHub PR-
// merged webhook receiver validates inbound signatures against.
// v0.89.23 #639 Stream 40. Slice 1 ships one shared deployment-wide
// secret (read from SQUADRON_GITHUB_WEBHOOK_SECRET in main.go); slice
// 2 will move to per-connection secrets, at which point this setter
// stays the deployment-wide fallback.
//
// An empty or nil slice is a valid input: the route still mounts but
// 503s with a humanized "set SQUADRON_GITHUB_WEBHOOK_SECRET" body so
// the operator's GitHub webhook delivery log carries the actionable
// pointer.
func (s *Server) SetIaCGitHubWebhookSecret(secret []byte) {
	if len(secret) == 0 {
		s.iacGitHubWebhookSecret = nil
		return
	}
	// Defensive copy so a caller scrubbing their env-loaded buffer
	// doesn't aliasingly scrub Squadron's cached copy.
	buf := make([]byte, len(secret))
	copy(buf, secret)
	s.iacGitHubWebhookSecret = buf
}

// SetIaCGitHubWebhookStore wires the v0.89.30 (#649) webhook delivery
// dedupe store onto the API server. The /api/v1/webhooks/github
// route consults it to short-circuit replays (captured-and-replayed
// signed deliveries) into a 200 + audit-replayed response without
// proceeding to the audit-emit path. Optional in the same posture as
// SetIaCGitHubWebhookSecret: a nil store leaves the route running
// without replay protection — the handler logs a warning on every
// inbound delivery so an operator can see the protection is unwired,
// but legitimate flows still work.
//
// In production, main.go wires this with the same applicationstore
// the rest of the server uses; the test_server.go path can leave it
// nil because the dedupe insert is best-effort.
func (s *Server) SetIaCGitHubWebhookStore(store handlers.WebhookDedupeStore) {
	s.iacGitHubWebhookStore = store
}

// SetIaCChecksClient wires the v0.89.43 (#663 Stream 61, slice 1
// chunk 2) GitHub Checks API client used by the chunk-2 PR-open
// follow-up. Nil leaves the follow-up dormant — the existing
// recommendation.pr_opened path completes normally with no
// check-run side-effects. Production wiring constructs a single
// *iacgithub.PATClient and passes it here; the PAT is supplied
// per-request at call time inside the helper.
func (s *Server) SetIaCChecksClient(c handlers.ChecksAPI) {
	s.iacChecksClient = c
}

// SetSquadronHost configures the base URL the check-run summary's
// "View in Squadron" link targets. Empty value suppresses the link
// line. Typically wired from os.Getenv("SQUADRON_PUBLIC_HOST").
func (s *Server) SetSquadronHost(host string) {
	s.squadronHost = host
}

// SetCheckRunName overrides the slice-1 default check-run name
// ("Squadron recommendation"). Operators wanting a different
// namespace can set this from os.Getenv("SQUADRON_CHECK_RUN_NAME").
// Empty value keeps the default.
func (s *Server) SetCheckRunName(name string) {
	s.checkRunName = name
}

// SetIaCWebhookChecksClient wires the v0.89.44 (#664 Stream 62,
// slice 1 chunk 3 of the GitHub Checks API back-signal arc) Checks
// API client used by the chunk-3 webhook follow-up on inbound merge
// / close events. Nil leaves the follow-up dormant — the existing
// recommendation.pr_merged / .pr_closed_not_merged path completes
// normally with no check-run side-effects. Production wires the
// SAME underlying *iacgithub.PATClient that satisfies the chunk-2
// SetIaCChecksClient surface; the two setters take different
// interface types because each chunk's interface stays deliberately
// narrow.
func (s *Server) SetIaCWebhookChecksClient(c handlers.WebhookChecksAPI) {
	s.iacWebhookChecksClient = c
}

// SetIaCWebhookChecksPAT wires the v0.89.44 (#664 Stream 62, slice 1
// chunk 3) deployment-wide PAT the webhook handler uses to
// authenticate the UpdateCheckRun PATCH. Empty leaves the chunk-3
// follow-up dormant — without a credential we cannot authenticate
// the PATCH. The PAT MUST carry the checks:write scope per design
// doc §5; missing scope surfaces as iac.check_run.failed with
// error_kind=scope_missing.
//
// Typically wired from os.Getenv("SQUADRON_IAC_GITHUB_PAT") at
// startup, but the runbook is explicit: the value is a credential
// and SHOULD live in the deployment's secrets substrate, not in
// plaintext config.
//
// Slice 2 candidate: per-connection PAT lookup off iacconnstore,
// mirroring the chunk-2 PR-open path which already unseals
// per-connection PATs.
func (s *Server) SetIaCWebhookChecksPAT(pat string) {
	s.iacWebhookChecksPAT = pat
}

// SetIaCChecksPAT wires the v0.89.44 (#665 Stream 63, slice 1 chunk
// 4) deployment-wide PAT the discovery exclusion handler uses to
// PATCH the in-flight check run to neutral on operator exclude.
// Empty value leaves the chunk-4 follow-up dormant (matches the
// nil-client posture). Production wiring sources the PAT from the
// same secret-manager pipeline as the IaC PR-open path.
func (s *Server) SetIaCChecksPAT(pat string) {
	s.iacChecksPAT = pat
}

// iacGitHubTrampoline late-binds an IaC-GitHub handler call. Mirrors
// discoveryTrampoline: 503s when the substrate is unwired so the
// test_server.go path stays unaffected.
//
// The handler is built per-request because the auditService is
// supplied via WithAuditService and the credstore Key via
// WithCredstoreKey — both are set on Server post-construction and the
// trampoline picks up whatever's there at request time.
func (s *Server) iacGitHubTrampoline(fn func(*handlers.IaCGitHubHandlers, *gin.Context)) gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.iacConnStore == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error":   "IaC connect is not configured",
				"enabled": false,
			})
			return
		}
		h := handlers.NewIaCGitHubHandlers(s.iacConnStore, s.logger)
		if s.auditService != nil {
			h.WithAuditService(s.auditService)
		}
		if s.discoveryCredKey != nil {
			h.WithCredstoreKey(s.discoveryCredKey)
		}
		// v0.89.43 (#663 Stream 61, slice 1 chunk 2 of the GitHub
		// Checks API back-signal arc). Wire the chunk-2 follow-up
		// surfaces. All four are optional — when any one is unwired
		// the helper short-circuits silently per design doc §5
		// fail-open posture. Production wires checksClient against a
		// shared *iacgithub.PATClient (token is supplied per-call so a
		// single client serves every operator); appStore satisfies the
		// slim CheckRunStore interface directly via its
		// SetCheckRunForRecommendation method.
		if s.iacChecksClient != nil {
			h.WithChecksClient(s.iacChecksClient)
		}
		if s.appStore != nil {
			h.WithCheckRunStore(s.appStore)
		}
		if strings.TrimSpace(s.squadronHost) != "" {
			h.WithSquadronHost(s.squadronHost)
		}
		if strings.TrimSpace(s.checkRunName) != "" {
			h.WithCheckRunName(s.checkRunName)
		}
		fn(h, c)
	}
}

// discoveryTrampoline late-binds a discovery handler call so the
// route table can be registered before SetDiscoveryCredStore runs.
// Mirrors the insights / recommendations / AI trampolines. 503s with
// a clear message when the credstore is still nil — that's the right
// state for the test_server.go path that doesn't wire discovery at
// all.
//
// The handler is built per-request because the auditService is
// supplied via WithAuditService and the credstore Key via
// WithCredstoreKey — both are set on Server post-construction and the
// trampoline picks up whatever's there at request time.
//
// v0.85 Stream 2F: the AI proposer is wired in too when the operator
// has configured one. The recommendations route additionally requires
// it (see discoveryAITrampoline); routes registered through the plain
// discoveryTrampoline tolerate a nil aiProposer because they never
// call it.
func (s *Server) discoveryTrampoline(fn func(*handlers.DiscoveryHandlers, *gin.Context)) gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.discoveryCredStore == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error":   "Discovery is not configured",
				"enabled": false,
			})
			return
		}
		h := handlers.NewDiscoveryHandlers(s.discoveryCredStore, s.logger)
		if s.auditService != nil {
			h.WithAuditService(s.auditService)
		}
		if s.discoveryCredKey != nil {
			h.WithCredstoreKey(s.discoveryCredKey)
		}
		if s.discoveryAIService != nil {
			h.WithAIProposer(s.discoveryAIService)
		}
		// v0.89.77 — wire the same traceindex the chunk-3 Discovery
		// dashboard reads Coverage from so the per-row last_seen_at
		// annotation flows through every AWS scan response.
		if s.traceIndexLookupForDiscovery != nil {
			h.WithTraceIndex(s.traceIndexLookupForDiscovery)
		}
		// v0.89.115 (#753 Stream 151, Cold-start latency analysis
		// slice 1 chunk 3) — wire the same cold-start observation
		// reader the chunk-2 per-resource endpoint uses so the
		// AnnotateServerlessWithColdStart pass populates the new
		// cold_start_p95_ms + cold_start_exceeds_threshold fields on
		// every AWS Lambda row of every scan response. Constants are
		// the substrate defaults (24h current / 168h baseline / 1.5x
		// ratio / 500ms floor). Nil reader short-circuits the
		// annotation; rows render "—" in the UI.
		if s.coldStartObservationReader != nil {
			h.WithColdStartObservationStore(
				s.coldStartObservationReader,
				handlers.NewStaticColdStartDetectionConstants(24, 168, 1.5, 500.0),
			)
		}
		// v0.89.37 (#656 Stream 54, #531 slice 2 chunk 4) — wire the
		// operator-set exclusion store. The application store satisfies
		// the slim DiscoveryExclusionStore interface directly so the
		// HandleAWSRecommendationExclude route lands with a real
		// upsert path when wiring is complete; otherwise the route
		// 503s with a clear "not wired" message.
		if s.appStore != nil {
			h.WithExclusionStore(s.appStore)
		}
		// v0.89.44 (#665 Stream 63, slice 1 chunk 4 of the GitHub Checks
		// API back-signal arc). Wire the chunk-4 PATCH-to-neutral
		// follow-up surfaces onto the exclusion handler. All four are
		// optional — when any one is unwired the helper short-circuits
		// silently per design doc §5 fail-open posture. The application
		// store satisfies the slim CheckRunStore interface directly
		// (it already exposes Get + Set check-run methods from chunk 1);
		// production wires iacChecksClient against a shared
		// *iacgithub.PATClient.
		if s.iacChecksClient != nil {
			h.WithChecksClient(s.iacChecksClient)
		}
		if s.appStore != nil {
			h.WithCheckRunStore(s.appStore)
		}
		if strings.TrimSpace(s.iacChecksPAT) != "" {
			h.WithChecksPAT(s.iacChecksPAT)
		}
		if strings.TrimSpace(s.squadronHost) != "" {
			h.WithSquadronHost(s.squadronHost)
		}
		fn(h, c)
	}
}

// discoveryAITrampoline is the v0.85 Stream 2F variant of
// discoveryTrampoline that additionally requires the AI proposer to be
// wired. Routes that ask the proposer to think (today: only the
// recommendations route) register through this trampoline so an
// AI-disabled deployment 503s with a clear opt-in message rather than
// running the handler and getting a generic "AI assist not configured"
// payload back from the proposer call itself.
//
// Same shape as discoveryTrampoline so the route registration above
// stays one-liner. The two trampolines share the credstore check; the
// AI check sits on top.
func (s *Server) discoveryAITrampoline(fn func(*handlers.DiscoveryHandlers, *gin.Context)) gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.discoveryCredStore == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error":   "Discovery is not configured",
				"enabled": false,
			})
			return
		}
		if s.discoveryAIService == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error":   "AI assist is not configured; discovery recommendations require it",
				"enabled": false,
			})
			return
		}
		h := handlers.NewDiscoveryHandlers(s.discoveryCredStore, s.logger)
		if s.auditService != nil {
			h.WithAuditService(s.auditService)
		}
		if s.discoveryCredKey != nil {
			h.WithCredstoreKey(s.discoveryCredKey)
		}
		h.WithAIProposer(s.discoveryAIService)
		// v0.89.28 (#643 slice 1) — wire the accepted-recommendations
		// assembler. The adapter iterates IaC connections to pick the
		// scope tuple per the spec's "same connection_id + account_id
		// + region" rule; slice 1 ships one connection per repo at
		// deployment scope so the iteration cost is trivial.
		if s.appStore != nil && s.iacConnStore != nil {
			adapter := &discoveryAcceptedAssemblerAdapter{
				appStore:    s.appStore,
				connections: s.iacConnStore,
			}
			h.WithAcceptedRecommendationsAssembler(adapter)
		}
		// v0.89.37 (#656 Stream 54, #531 slice 2 chunk 4) — wire the
		// operator-set exclusion store on this trampoline too. The
		// recommendations route doesn't consult it, but a per-handler
		// builder shares one struct so the exclude route registered
		// under discoveryTrampoline also picks it up consistently.
		if s.appStore != nil {
			h.WithExclusionStore(s.appStore)
		}
		fn(h, c)
	}
}

// discoveryGCPTrampoline late-binds a GCP discovery handler call so
// the route table can be registered before SetGCPDiscoveryStore runs.
// Mirrors discoveryTrampoline one-for-one: 503s with a clear message
// when the GCP store is still nil so the test_server.go path (which
// never wires GCP discovery) stays unaffected.
//
// The handler is built per-request because the auditService is
// supplied via SetAuditService and the credstore.Key via
// SetDiscoveryCredKey — both are set on Server post-construction and
// the trampoline picks up whatever's there at request time.
//
// v0.89.47 (#667 Stream 67, GCP discovery slice 1 chunk 3).
func (s *Server) discoveryGCPTrampoline(fn func(*handlers.DiscoveryGCPHandlers, *gin.Context)) gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.discoveryGCPStore == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error":   "GCP discovery is not configured",
				"enabled": false,
			})
			return
		}
		h := handlers.NewDiscoveryGCPHandlers(s.discoveryGCPStore, s.logger)
		if s.auditService != nil {
			h.WithGCPAuditService(s.auditService)
		}
		if s.discoveryCredKey != nil {
			h.WithGCPCredstoreKey(s.discoveryCredKey)
		}
		if s.discoveryGCPScannerFactory != nil {
			h.WithGCPScannerFactory(s.discoveryGCPScannerFactory)
		}
		// v0.89.77 — wire the traceindex lookup for per-row
		// last_seen_at annotation on GCP scan responses.
		if s.traceIndexLookupForDiscovery != nil {
			h.WithGCPTraceIndex(s.traceIndexLookupForDiscovery)
		}
		// chunk 5 (v0.89.197) — wire the AI proposer so the GCP
		// recommendations endpoint works. Unconditional: the handler
		// 503s when the proposer is nil (AI assist off).
		h.WithGCPAIProposer(s.discoveryAIService)
		// parity follow-up (v0.89.199) — wire the accepted-recommendations
		// assembler so the verdict block + discovery_proposal.created event
		// reach the proposer (cold-start empty until recs are accepted).
		if s.appStore != nil && s.iacConnStore != nil {
			h.WithGCPAcceptedAssembler(&discoveryAcceptedAssemblerAdapter{
				appStore:    s.appStore,
				connections: s.iacConnStore,
			})
		}
		fn(h, c)
	}
}

// discoveryAzureTrampoline late-binds an Azure discovery handler call
// so the route table can be registered before SetAzureDiscoveryStore
// runs. Mirrors discoveryGCPTrampoline one-for-one: 503s with a clear
// message when the Azure store is still nil so the test_server.go
// path (which never wires Azure discovery) stays unaffected.
//
// The handler is built per-request because the auditService is
// supplied via SetAuditService and the credstore.Key via
// SetDiscoveryCredKey — both are set on Server post-construction and
// the trampoline picks up whatever's there at request time.
//
// v0.89.52 (#676 Stream 74, Azure discovery slice 1 chunk 3).
func (s *Server) discoveryAzureTrampoline(fn func(*handlers.DiscoveryAzureHandlers, *gin.Context)) gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.discoveryAzureStore == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error":   "Azure discovery is not configured",
				"enabled": false,
			})
			return
		}
		h := handlers.NewDiscoveryAzureHandlers(s.discoveryAzureStore, s.logger)
		if s.auditService != nil {
			h.WithAzureAuditService(s.auditService)
		}
		if s.discoveryCredKey != nil {
			h.WithAzureCredstoreKey(s.discoveryCredKey)
		}
		if s.discoveryAzureScannerFactory != nil {
			h.WithAzureScannerFactory(s.discoveryAzureScannerFactory)
		}
		// v0.89.77 — wire the traceindex lookup for per-row
		// last_seen_at annotation on Azure scan responses.
		if s.traceIndexLookupForDiscovery != nil {
			h.WithAzureTraceIndex(s.traceIndexLookupForDiscovery)
		}
		// chunk 5 (v0.89.198) — wire the AI proposer for the Azure
		// recommendations endpoint. Unconditional; the handler 503s on nil.
		h.WithAzureAIProposer(s.discoveryAIService)
		// parity follow-up (v0.89.199) — wire the accepted-recommendations
		// assembler so the verdict block + discovery_proposal.created event
		// reach the proposer (cold-start empty until recs are accepted).
		if s.appStore != nil && s.iacConnStore != nil {
			h.WithAzureAcceptedAssembler(&discoveryAcceptedAssemblerAdapter{
				appStore:    s.appStore,
				connections: s.iacConnStore,
			})
		}
		fn(h, c)
	}
}

// discoveryOCITrampoline late-binds an OCI discovery handler call
// so the route table can be registered before SetOCIDiscoveryStore
// runs. Mirrors discoveryAzureTrampoline one-for-one: 503s with a
// clear message when the OCI store is still nil so the
// test_server.go path (which never wires OCI discovery) stays
// unaffected.
//
// The handler is built per-request because the auditService is
// supplied via SetAuditService and the credstore.Key via
// SetDiscoveryCredKey — both are set on Server post-construction
// and the trampoline picks up whatever's there at request time.
//
// v0.89.57 (#683 Stream 81, OCI discovery slice 1 chunk 3).
func (s *Server) discoveryOCITrampoline(fn func(*handlers.DiscoveryOCIHandlers, *gin.Context)) gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.discoveryOCIStore == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error":   "OCI discovery is not configured",
				"enabled": false,
			})
			return
		}
		h := handlers.NewDiscoveryOCIHandlers(s.discoveryOCIStore, s.logger)
		if s.auditService != nil {
			h.WithOCIAuditService(s.auditService)
		}
		if s.discoveryCredKey != nil {
			h.WithOCICredstoreKey(s.discoveryCredKey)
		}
		if s.discoveryOCIScannerFactory != nil {
			h.WithOCIScannerFactory(s.discoveryOCIScannerFactory)
		}
		// v0.89.77 — wire the traceindex lookup for per-row
		// last_seen_at annotation on OCI scan responses.
		if s.traceIndexLookupForDiscovery != nil {
			h.WithOCITraceIndex(s.traceIndexLookupForDiscovery)
		}
		// chunk 5 (v0.89.198) — wire the AI proposer for OCI recommendations.
		h.WithOCIAIProposer(s.discoveryAIService)
		// parity follow-up (v0.89.199) — wire the accepted-recommendations
		// assembler so the verdict block + discovery_proposal.created event
		// reach the proposer (cold-start empty until recs are accepted).
		if s.appStore != nil && s.iacConnStore != nil {
			h.WithOCIAcceptedAssembler(&discoveryAcceptedAssemblerAdapter{
				appStore:    s.appStore,
				connections: s.iacConnStore,
			})
		}
		fn(h, c)
	}
}

// discoverySummaryTrampoline late-binds the unified Discovery
// dashboard handler call so the route table can be registered before
// any of the four provider stores are wired. v0.89.61 (#688 Stream
// 86, Unified Discovery dashboard slice 1 chunk 1). Unlike the
// per-provider trampolines, this one ALWAYS serves — a deployment
// that hasn't wired any provider gets a 200 with every provider card
// in the enabled=false empty state. That's the right answer for the
// fresh-install welcome view (per design doc §6 "Empty state").
//
// Per design doc §5.1 the handler caches the SummaryResponse for 30s.
// The trampoline builds a new handler per request, so the cache lives
// on the Server struct (s.discoverySummaryHandler) rather than in the
// trampoline closure — otherwise every request would get its own
// cache and the TTL would be meaningless. Lazy construction: the
// handler is built on first call, reused thereafter; reads of
// s.discoverySummaryHandler are guarded by the handler's internal
// mutex through the cache.
func (s *Server) discoverySummaryTrampoline(fn func(*handlers.DiscoverySummaryHandlers, *gin.Context)) gin.HandlerFunc {
	return func(c *gin.Context) {
		s.summaryHandlerOnce.Do(func() {
			s.discoverySummaryHandler = handlers.NewDiscoverySummaryHandlers(
				handlers.NewAWSSummaryStore(s.discoveryCredStore),
				handlers.NewGCPSummaryStore(s.discoveryGCPStore),
				handlers.NewAzureSummaryStore(s.discoveryAzureStore),
				handlers.NewOCISummaryStore(s.discoveryOCIStore),
				s.auditService,
				handlers.NewApplicationStoreAuditQuery(s.appStore),
				handlers.DefaultSummaryCacheTTL,
				nil, // production clock
				s.logger,
			)
		})
		fn(s.discoverySummaryHandler, c)
	}
}

// SetTraceIndexForDiscovery wires the v0.89.76 (#707 Stream 105, Trace
// integration slice 1 chunk 3) traceindex.Index onto the API server so
// the new /api/v1/discovery/trace_coverage handler can project per-
// scope coverage. The traceindex.Index satisfies the
// handlers.TraceIndex interface directly through its Coverage method.
//
// Optional in the same posture as SetGCPDiscoveryStore: a nil index
// leaves the endpoint serving all-zero coverage for every provider —
// the correct cold-start posture for a deployment that hasn't observed
// any spans yet. Production wiring (cmd/all-in-one) calls this with the
// same *traceindex.Index the chunk-2 OTLP receiver dispatches Observe
// to so the dashboard and the receiver share one index.
func (s *Server) SetTraceIndexForDiscovery(idx handlers.TraceIndex) {
	s.traceIndexForDiscovery = idx
}

// SetTraceIndexLookupForDiscovery wires the v0.89.77 (#708 Stream
// 106, Trace integration slice 1 chunk 4) per-resource LastSeenAt
// lookup onto the four per-provider discovery scan handlers. The
// real *traceindex.Index satisfies the handlers.TraceIndexLookup
// interface directly through its LastSeenAt method; production
// passes the same instance SetTraceIndexForDiscovery already
// received so the receiver, the dashboard, and the inventory
// annotation all share one index.
//
// Optional; nil leaves the scan response un-annotated (every row
// surfaces "never" in the UI), matching the cold-start posture.
func (s *Server) SetTraceIndexLookupForDiscovery(idx handlers.TraceIndexLookup) {
	s.traceIndexLookupForDiscovery = idx
}

// discoveryTraceCoverageTrampoline late-binds the v0.89.76 (#707 Stream
// 105) trace coverage handler call so the route table can be registered
// before SetTraceIndexForDiscovery (or any of the four provider stores)
// runs. Same posture as discoverySummaryTrampoline: ALWAYS serves —
// a deployment that hasn't wired any provider gets a 200 with every
// provider key populated as zero-count. This is by design per the
// design doc §7 cold-start contract; the dashboard renders an empty
// state inside the panel rather than hiding the panel entirely.
//
// Per design doc §7 the handler caches the TraceCoverageResponse for
// 30s. The trampoline builds the handler once on first call and reuses
// it thereafter so the cache lives on the Server struct.
func (s *Server) discoveryTraceCoverageTrampoline(fn func(*handlers.DiscoveryTraceCoverageHandlers, *gin.Context)) gin.HandlerFunc {
	return func(c *gin.Context) {
		s.traceCoverageHandlerOnce.Do(func() {
			audit := handlers.NewApplicationStoreAuditQuery(s.appStore)
			s.discoveryTraceCoverageHandler = handlers.NewDiscoveryTraceCoverageHandlers(
				handlers.NewAWSSummaryStore(s.discoveryCredStore),
				handlers.NewGCPSummaryStore(s.discoveryGCPStore),
				handlers.NewAzureSummaryStore(s.discoveryAzureStore),
				handlers.NewOCISummaryStore(s.discoveryOCIStore),
				s.traceIndexForDiscovery,
				handlers.NewAuditInventoryCountQuery(audit),
				// v0.89.82 (#713 Stream 111) — slice-2 pending-emission
				// projection. nil today: the inventory store wiring
				// that exposes primitive_enabled + last_seen_at lands
				// in a follow-on chunk. The handler treats nil as
				// "everything is zero" so the sub-indicator stays
				// hidden until the projection is wired.
				nil,
				s.auditService,
				handlers.DefaultTraceCoverageCacheTTL,
				nil, // production clock
				s.logger,
			)
		})
		fn(s.discoveryTraceCoverageHandler, c)
	}
}

// SetQualitySnapshotIndexForDiscovery wires the v0.89.86 (#717 Stream
// 115, Span quality slice 1 chunk 2) Quality snapshot index onto the
// API server so the new /api/v1/discovery/span_quality handler can
// project per-provider aggregates. The same *traceindex.Quality the
// chunk-1 OTLP receiver dispatches Observe to satisfies the
// handlers.QualitySnapshotIndex interface directly via SnapshotAll +
// SnapshotKey.
//
// Optional in the same posture as SetTraceIndexForDiscovery: a nil
// index leaves the aggregate endpoint serving all-zero counts for
// every provider and the per-resource detail endpoint 404ing every
// lookup — the correct cold-start posture for a deployment that
// hasn't observed any spans yet.
func (s *Server) SetQualitySnapshotIndexForDiscovery(idx handlers.QualitySnapshotIndex) {
	s.qualitySnapshotIndexForDiscovery = idx
}

// SetResourceKeyProjectorForDiscovery wires the v0.89.86 (#717 Stream
// 115) per-resource key projector onto the API server so the
// /api/v1/discovery/:provider/inventory/:kind/:id/span_quality
// handler can resolve path params to a quality key. Production wires
// an inventory-store-aware projector that runs the same
// ComputeResourceKey projection the OTLP receiver does, against the
// stored inventory row.
//
// Optional; nil leaves the per-resource endpoint 404ing every lookup
// (the cold-start posture for a deployment that hasn't wired the
// inventory projection yet).
func (s *Server) SetResourceKeyProjectorForDiscovery(p handlers.ResourceKeyProjector) {
	s.resourceKeyProjectorForDiscovery = p
}

// discoverySpanQualityTrampoline late-binds the v0.89.86 (#717
// Stream 115) span-quality handler call so the route table can be
// registered before SetQualitySnapshotIndexForDiscovery /
// SetResourceKeyProjectorForDiscovery run. Same posture as
// discoveryTraceCoverageTrampoline: ALWAYS serves — a deployment
// that hasn't wired the Quality index or the key projector gets
// either zero-counts (aggregate) or 404 (detail), never 503.
//
// The trampoline builds the handler once on first call and reuses it
// thereafter so the cache lives on the Server struct.
func (s *Server) discoverySpanQualityTrampoline(fn func(*handlers.DiscoverySpanQualityHandlers, *gin.Context)) gin.HandlerFunc {
	return func(c *gin.Context) {
		s.spanQualityHandlerOnce.Do(func() {
			s.discoverySpanQualityHandler = handlers.NewDiscoverySpanQualityHandlers(
				s.qualitySnapshotIndexForDiscovery,
				s.resourceKeyProjectorForDiscovery,
				s.auditService,
				handlers.DefaultSpanQualityCacheTTL,
				nil, // production clock
				s.logger,
			)
		})
		fn(s.discoverySpanQualityHandler, c)
	}
}

// SetWorkloadHealthInventoryReader wires the v0.89.132 (#772 Stream
// 170, Workload Health dashboard panel slice 1 chunk 1) reader onto
// the API server so the new /api/v1/discovery/workload_health handler
// can project per-provider serverless health counts.
//
// Optional in the same posture as SetQualitySnapshotIndexForDiscovery:
// a nil reader leaves the endpoint serving zero counts for every
// provider — the correct cold-start posture for a deployment that
// hasn't wired the serverless inventory annotators (the UI then hides
// the panel per design doc §5.3). Production wiring (cmd/all-in-one)
// calls this with the in-memory inventory reader that observes the
// same per-scan annotations the cold-start / sampling / error-rate
// annotators write.
func (s *Server) SetWorkloadHealthInventoryReader(r handlers.ServerlessHealthInventoryReader) {
	s.workloadHealthInventoryReader = r
}

// discoveryWorkloadHealthTrampoline late-binds the v0.89.132 (#772
// Stream 170) workload health handler call so the route table can be
// registered before SetWorkloadHealthInventoryReader runs. Same
// posture as discoverySpanQualityTrampoline: ALWAYS serves — a
// deployment that hasn't wired the reader gets zero counts for every
// provider (the dashboard panel hides itself on the UI side), never
// 503.
//
// The trampoline builds the handler once on first call and reuses it
// thereafter so the 30s cache lives on the Server struct.
func (s *Server) discoveryWorkloadHealthTrampoline(fn func(*handlers.DiscoveryWorkloadHealthHandlers, *gin.Context)) gin.HandlerFunc {
	return func(c *gin.Context) {
		s.workloadHealthHandlerOnce.Do(func() {
			s.discoveryWorkloadHealthHandler = handlers.NewDiscoveryWorkloadHealthHandlers(
				s.workloadHealthInventoryReader,
				s.auditService,
				handlers.DefaultWorkloadHealthCacheTTL,
				nil, // production clock
				s.logger,
			)
		})
		fn(s.discoveryWorkloadHealthHandler, c)
	}
}

// discoveryServerlessColdStartTrampoline late-binds the v0.89.114
// (#752 Stream 150) per-resource cold-start endpoint handler so the
// caller composes the deferred wire-through against the optional
// SetColdStartObservationReader call. Same lazy-construction pattern
// as discoverySpanQualityTrampoline — the handler builds once on
// first call and reuses thereafter so the wired store reference
// stays stable across requests.
//
// Wiring posture: nil store at construction time means the
// per-resource endpoint returns 404 unconditionally — matching the
// "no cold-start observations recorded for resource" 404 the
// handler would emit for any specific resource that hasn't been
// observed yet. Deployments that haven't wired chunk 1 see the
// chunk-1 storage migration land but the endpoint return 404 until
// chunk 2's persistence path runs at least once.
func (s *Server) discoveryServerlessColdStartTrampoline(fn func(*handlers.DiscoveryServerlessColdStartHandlers, *gin.Context)) gin.HandlerFunc {
	return func(c *gin.Context) {
		s.coldStartHandlerOnce.Do(func() {
			s.discoveryServerlessColdStartHandler = handlers.NewDiscoveryServerlessColdStartHandlers(
				s.coldStartObservationReader,
				nil, // default constants — 24h / 168h / 1.5x / 500ms
				s.logger,
			)
		})
		fn(s.discoveryServerlessColdStartHandler, c)
	}
}

// SetColdStartObservationReader — v0.89.114 (#752 Stream 150). Wires
// the cold-start observation store into the Server. The setter
// posture mirrors SetActionStoreAndSigner — production calls this
// after the sqlite store is constructed; tests pass nil to exercise
// the unwired 404 path. Safe to call before or after the trampoline
// builds the handler so long as the call happens before the first
// route hit (the once.Do captures whatever value is set at that
// point).
func (s *Server) SetColdStartObservationReader(reader handlers.ColdStartObservationReader) {
	s.coldStartObservationReader = reader
}

// discoveryServerlessSamplingTrampoline — v0.89.123 (#763 Stream 161,
// Sampling rate analysis slice 1 chunk 2). Mirrors the cold-start
// trampoline from v0.89.114: builds the per-resource sampling
// handler once on first request and reuses thereafter. Nil lookup
// OR nil detector at construction time means the handler returns
// 404 unconditionally — matching the "no sampling observations
// recorded for resource" 404 the handler would emit for any
// specific resource that hasn't been observed yet.
func (s *Server) discoveryServerlessSamplingTrampoline(fn func(*handlers.DiscoveryServerlessSamplingHandlers, *gin.Context)) gin.HandlerFunc {
	return func(c *gin.Context) {
		s.samplingHandlerOnce.Do(func() {
			s.discoveryServerlessSamplingHandler = handlers.NewDiscoveryServerlessSamplingHandlers(
				s.samplingResourceLookup,
				s.samplingDetector,
				s.logger,
			)
		})
		fn(s.discoveryServerlessSamplingHandler, c)
	}
}

// SetSamplingResourceLookup — v0.89.123 (#763 Stream 161). Wires
// the per-cloud serverless inventory lookup into the Server.
// Mirrors SetColdStartObservationReader posture — production
// calls this after the inventory adapter is constructed; tests
// pass nil to exercise the unwired 404 path.
func (s *Server) SetSamplingResourceLookup(lookup handlers.SamplingResourceLookup) {
	s.samplingResourceLookup = lookup
}

// SetSamplingDetector — v0.89.123. Wires the per-cloud sampling
// detector (closure holding MetricQuerier + traceindex Quality)
// into the Server. Same posture as SetSamplingResourceLookup.
func (s *Server) SetSamplingDetector(detector handlers.SamplingDetector) {
	s.samplingDetector = detector
}

// discoveryServerlessErrorRateTrampoline — v0.89.128 (#768 Stream
// 166, Error rate correlation slice 1 chunk 2). Mirrors the
// cold-start trampoline from v0.89.114: builds the per-resource
// error-rate handler once on first request and reuses thereafter.
// Nil reader at construction time means the handler returns 404
// unconditionally — matching the "no error-rate observations
// recorded for resource" 404 the handler would emit for any
// specific resource that hasn't been observed yet.
func (s *Server) discoveryServerlessErrorRateTrampoline(fn func(*handlers.DiscoveryServerlessErrorRateHandlers, *gin.Context)) gin.HandlerFunc {
	return func(c *gin.Context) {
		s.errorRateHandlerOnce.Do(func() {
			s.discoveryServerlessErrorRateHandler = handlers.NewDiscoveryServerlessErrorRateHandlers(
				s.errorRateObservationReader,
				s.logger,
			)
		})
		fn(s.discoveryServerlessErrorRateHandler, c)
	}
}

// SetErrorRateObservationReader — v0.89.128 (#768 Stream 166).
// Wires the error-rate observation store into the Server. Mirrors
// SetColdStartObservationReader posture — production calls this
// after the sqlite store is constructed; tests pass nil to
// exercise the unwired 404 path. Safe to call before or after the
// trampoline builds the handler so long as the call happens before
// the first route hit (the once.Do captures whatever value is set
// at that point).
func (s *Server) SetErrorRateObservationReader(reader handlers.ErrorRateObservationReader) {
	s.errorRateObservationReader = reader
}

// discoveryAcceptedAssemblerAdapter — v0.89.28 (#643 slice 1) —
// resolves the (account_id, region) scope to a per-connection lookup
// by iterating the iacconnstore.Store. The discovery handler doesn't
// see IaC connection IDs directly; the adapter walks every connection
// the deployment has and unions accepted-PR rows from each
// (connection_id, account_id, region) scope. Slice 1 ships one
// connection per deployment so the union is degenerate; slice 2's
// multi-tenant question (multiple connections to the same repo) lands
// in the cross-connection-scope question from §10 Q4.
type discoveryAcceptedAssemblerAdapter struct {
	appStore    applicationstore.ApplicationStore
	connections iacconnstore.Store
}

// AssembleForDiscoveryScope walks every IaC connection, builds a
// DiscoveryBridge per connection, and returns the union of the
// accepted-PR examples and URLs. Errors on any single connection are
// logged via the higher-level Warn in the handler and skipped — one
// bad connection shouldn't sink the proposer call.
//
// v0.89.36 (#655 Stream 53): retained as the v0.89.28 compat surface;
// HandleAWSGenerateRecommendations now calls AssembleVerdictBlock
// below instead. Kept so existing stubs and SIEM-facing helpers
// continue to compile.
func (a *discoveryAcceptedAssemblerAdapter) AssembleForDiscoveryScope(
	ctx context.Context, accountID, region string,
) ([]ai.AcceptedRecommendationExample, []string, error) {
	conns, err := a.connections.List(ctx)
	if err != nil {
		return nil, nil, err
	}
	var examples []ai.AcceptedRecommendationExample
	var urls []string
	for _, conn := range conns {
		if conn == nil {
			continue
		}
		b := proposer.NewDiscoveryBridge(a.appStore, a.connections)
		exs, prURLs, err := b.AssembleAcceptedRecommendations(ctx, conn.ConnectionID, accountID, region)
		if err != nil {
			continue
		}
		examples = append(examples, exs...)
		urls = append(urls, prURLs...)
	}
	return examples, urls, nil
}

// AssembleVerdictBlock is the v0.89.36 (#655 Stream 53, #531 slice 2
// chunk 3) wiring path. Walks every IaC connection, builds a
// DiscoveryBridge per connection, calls AssembleDiscoveryVerdicts,
// unions the per-connection approved + rejected slices, and renders
// the combined pool through verdictprompt with the discovery-surface
// copy. Returns ("", nil, nil) on the cold-start path so the prompt
// stays byte-for-byte identical to the slice 1 (v0.89.28) output.
//
// Per-connection errors are skipped (matching AssembleForDiscoveryScope).
// A connection-list error surfaces to the caller, which logs and
// proceeds with cold-start.
func (a *discoveryAcceptedAssemblerAdapter) AssembleVerdictBlock(
	ctx context.Context, accountID, region string,
) (string, []string, error) {
	block, urls, _, err := a.AssembleVerdictBlockWithByState(ctx, accountID, region)
	return block, urls, err
}

// AssembleVerdictBlockWithByState is the v0.89.37 (#657 Stream 55,
// #531 slice 2 chunk 6) extension. Mirrors AssembleVerdictBlock but
// also returns the per-state PR URL bucket map for the audit
// payload's verdict_examples_used_by_state field. The bucket map
// projects each selected verdict's State to a discovery-surface
// bucket key (merged → "merged"; closed_not_merged →
// "closed_not_merged"; operator_excluded → "operator_excluded";
// other states defensively skipped). On cold start / opt-out /
// recency-window empty the bucket map is nil so the caller's
// hasAnyDiscoveryByState gate omits the field from the audit row.
func (a *discoveryAcceptedAssemblerAdapter) AssembleVerdictBlockWithByState(
	ctx context.Context, accountID, region string,
) (string, []string, map[string][]string, error) {
	conns, err := a.connections.List(ctx)
	if err != nil {
		return "", nil, nil, err
	}
	var allApproved, allRejected []verdictsel.Verdict
	var urls []string
	urlsByState := map[string][]string{}
	var lastBridge *proposer.DiscoveryBridge
	for _, conn := range conns {
		if conn == nil {
			continue
		}
		b := proposer.NewDiscoveryBridge(a.appStore, a.connections)
		lastBridge = b
		approved, rejected, prURLs, err := b.AssembleDiscoveryVerdicts(ctx, conn.ConnectionID, accountID, region)
		if err != nil {
			continue
		}
		allApproved = append(allApproved, approved...)
		allRejected = append(allRejected, rejected...)
		urls = append(urls, prURLs...)
		// Walk the rejected + approved slices to populate the by-state
		// map. The bucket keys are the discovery-surface state
		// strings; the order within each bucket mirrors the
		// per-connection AssembleDiscoveryVerdicts walk, which already
		// orders by selection (rejected first, then approved).
		for _, v := range rejected {
			switch v.State {
			case verdictsel.StateClosedNotMerged:
				urlsByState["closed_not_merged"] = append(urlsByState["closed_not_merged"], v.ID)
			case verdictsel.StateOperatorExcluded:
				urlsByState["operator_excluded"] = append(urlsByState["operator_excluded"], v.ID)
			}
		}
		for _, v := range approved {
			if v.State == verdictsel.StateMerged {
				urlsByState["merged"] = append(urlsByState["merged"], v.ID)
			}
		}
	}
	if lastBridge == nil || (len(allApproved) == 0 && len(allRejected) == 0) {
		return "", nil, nil, nil
	}
	return lastBridge.RenderDiscoveryVerdictBlock(allApproved, allRejected), urls, urlsByState, nil
}

// aiTrampoline late-binds an AI handler call. Same shape as the
// other trampolines, but with a softer 503 — the /status route in
// particular needs to remain responsive even when the service is
// unwired so the UI's capability probe doesn't fail at app load.
// We let the /status handler decide its own response shape; other
// handlers go through the standard nil-check.
func (s *Server) aiTrampoline(fn func(*handlers.AIHandlers, *gin.Context)) gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.aiService == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error":   "AI assist is not configured",
				"enabled": false,
			})
			return
		}
		h := handlers.NewAIHandlers(s.aiService, s.logger)
		fn(h, c)
	}
}

// askTrampoline late binds the Ask handler so the route table can
// be wired before the AI service is set. The handler needs the
// rollout + audit services to build its context bag; both are
// already on the Server and required at construction, so no nil
// guards beyond the AI check are needed.
//
// Same 503 semantics as the rest of the AI surface: if AI isn't
// configured the route returns a clear opt in message rather than
// 500ing or pretending the surface exists.
func (s *Server) askTrampoline() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.aiService == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error":   "AI assist is not configured",
				"enabled": false,
			})
			return
		}
		// v0.66 — costSpikes + recs lazily adapted into the Ask
		// listers. Either may be nil (cost insights or recs engine
		// not wired); the handler skips a source when its lister is
		// nil, so the resulting bag just narrows accordingly.
		var costSpikes handlers.AskCostSpikeLister
		if s.costSpikes != nil {
			costSpikes = s.costSpikes
		}
		var recs handlers.AskRecLister
		if s.recsEngine != nil {
			recs = newAskRecsAdapter(s.recsEngine)
		}
		// v0.68 — agents adapter. agentService is required at
		// Server construction so it's never nil here, but guard
		// anyway in case a future code path makes it optional.
		var agents handlers.AskAgentLister
		if s.agentService != nil {
			agents = newAskAgentsAdapter(s.agentService)
		}
		h := handlers.NewAskHandler(s.aiService, s.rolloutService, s.auditService, costSpikes, recs, agents, s.logger)
		h.HandleAsk(c)
	}
}

// askAgentsAdapter wraps services.AgentService so it satisfies
// handlers.AskAgentLister. Walks ListAgents, prioritizes the
// operator interesting subset (offline first, then drifted, then
// whatever fills the remaining slots), trims to limit. A 500
// agent healthy fleet returns zero entries, which is correct — the
// operator asking "anything wrong?" gets a JARVIS "no" rather than
// a wall of agent rows that say "online, synced."
type askAgentsAdapter struct {
	svc services.AgentService
}

func newAskAgentsAdapter(svc services.AgentService) *askAgentsAdapter {
	return &askAgentsAdapter{svc: svc}
}

func (a *askAgentsAdapter) ListForAsk(ctx context.Context, limit int) ([]handlers.AskAgent, error) {
	if a == nil || a.svc == nil {
		return nil, nil
	}
	all, err := a.svc.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	// Walk twice — first pass collects interesting agents, second
	// pass fills with whatever's left up to limit. Cheap on a
	// 500-agent fleet; large fleets would benefit from a service
	// layer query if this ever becomes a hot path.
	var offline, drifted, rest []handlers.AskAgent
	for _, ag := range all {
		if ag == nil {
			continue
		}
		slim := handlers.AskAgent{
			ID:          ag.ID.String(),
			Name:        ag.Name,
			Status:      string(ag.Status),
			DriftStatus: string(ag.DriftStatus),
			LastSeen:    ag.LastSeen,
		}
		if ag.GroupName != nil {
			slim.GroupName = *ag.GroupName
		}
		switch {
		case ag.Status == services.AgentStatusOffline:
			offline = append(offline, slim)
		case ag.DriftStatus == services.ConfigDriftStatusDrifted:
			drifted = append(drifted, slim)
		default:
			rest = append(rest, slim)
		}
	}
	// Concatenate in priority order, then trim. The "interesting
	// first" ordering means that on a small bag, the first slot
	// is the most likely answer to the operator's question.
	out := append([]handlers.AskAgent{}, offline...)
	out = append(out, drifted...)
	// Skip the rest bucket entirely: healthy synced agents do not
	// belong in the bag. An operator asking about the fleet by
	// name will hit the next bag widening (agent name search) in
	// a follow on release; for v0.68 we keep the bag focused on
	// what's actually wrong.
	_ = rest
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// askRecsAdapter wraps a recommendations.Engine so it satisfies the
// handlers.AskRecLister interface. The Ask handler calls
// ListForAsk(ctx, limit); we map that to Engine.Evaluate with a
// fixed 1h window (the same default the /recommendations endpoint
// uses) and trim to the requested limit. Severity ordering and
// dismissal filtering are already handled by the engine.
type askRecsAdapter struct {
	engine *recommendations.Engine
}

func newAskRecsAdapter(engine *recommendations.Engine) *askRecsAdapter {
	return &askRecsAdapter{engine: engine}
}

func (a *askRecsAdapter) ListForAsk(ctx context.Context, limit int) ([]handlers.AskRec, error) {
	if a == nil || a.engine == nil {
		return nil, nil
	}
	recs, err := a.engine.Evaluate(ctx, insights.Window1h)
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(recs) > limit {
		recs = recs[:limit]
	}
	out := make([]handlers.AskRec, 0, len(recs))
	for _, r := range recs {
		out = append(out, handlers.AskRec{
			ID:      r.ID,
			Title:   r.Title,
			Detail:  r.Detail,
			AgentID: r.AgentID,
		})
	}
	return out, nil
}

// aiStatusTrampoline is the special case for /api/v1/ai/status —
// when the service is nil, return enabled=false rather than 503
// so the UI's capability probe stays simple.
func (s *Server) aiStatusTrampoline() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.aiService == nil {
			c.JSON(http.StatusOK, ai.Capabilities{Enabled: false})
			return
		}
		h := handlers.NewAIHandlers(s.aiService, s.logger)
		h.HandleStatus(c)
	}
}

// recommendationsTrampoline late-binds a recs handler call so the
// route table can be registered before SetRecommendationsEngine
// runs. Mirrors insightsTrampoline; 503s with a clear error
// message when the engine is still nil.
func (s *Server) recommendationsTrampoline(fn func(*handlers.RecommendationsHandlers, *gin.Context)) gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.recsEngine == nil || s.recsDismissals == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": "Cost Recommendations are not available — engine not wired",
			})
			return
		}
		h := handlers.NewRecommendationsHandlers(s.recsEngine, s.recsDismissals, s.logger)
		fn(h, c)
	}
}

// Start starts the HTTP server
func (s *Server) Start(port string) error {
	s.httpServer = &http.Server{
		Addr:    ":" + port,
		Handler: s.router,
	}

	s.logger.Info("Starting HTTP API server", zap.String("port", port))
	return s.httpServer.ListenAndServe()
}

// Stop gracefully stops the HTTP server
func (s *Server) Stop(ctx context.Context) error {
	s.logger.Info("Stopping HTTP API server")

	// Create a context with timeout for graceful shutdown
	shutdownCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	return s.httpServer.Shutdown(shutdownCtx)
}

// registerRoutes registers all API routes.
//
// WIRING-ORDER GOTCHA (see v0.89.211/212): registerRoutes runs inside
// NewServer(), BEFORE main.go wires the post-construction stores via the
// Set* methods (e.g. SetActionStoreAndSigner sets s.appStore;
// SetDiscoveryCredStore, SetAIService, ...). A handler built EAGERLY here
// that reads one of those still-nil fields captures the nil and panics at
// request time (-> 500). Two real bugs came from exactly this: the
// incidents and actions handlers. Rule: a handler that depends on a
// post-construction store MUST be built lazily — per request, via a
// closure/trampoline that reads s.<field> when the request arrives (see
// the discovery trampolines and the incidents/actions route closures
// below), and should nil-guard the store as defense-in-depth. Handlers
// that use only NewServer constructor params (agentService,
// telemetryService, alertService, broker, ...) are safe to build eagerly.
func (s *Server) registerRoutes() {
	// Initialize handlers
	agentHandlers := handlers.NewAgentHandlersWithTracer(s.agentService, s.commander, s.configsTracer, s.logger)
	configHandlers := handlers.NewConfigHandlers(s.agentService, s.commander, s.logger)
	// v0.51 — wire the audit service so HandleLintConfig can persist
	// config.lint_evaluated events when findings show up. nil-safe.
	configHandlers.SetAuditService(s.auditService)
	telemetryHandlers := handlers.NewTelemetryHandlers(s.telemetryService, s.logger)
	squadronQLHandlers := handlers.NewSquadronQLHandlers(s.telemetryService, s.logger)
	groupHandlers := handlers.NewGroupHandlers(s.agentService, s.commander, s.logger)
	savedQueryHandlers := handlers.NewSavedQueryHandlers(s.savedQueryService, s.logger)
	topologyHandlers := handlers.NewTopologyHandlers(s.agentService, s.telemetryService, s.logger)
	healthHandlers := handlers.NewHealthHandlers(s.agentService, s.telemetryService, s.logger)
	alertHandlers := handlers.NewAlertHandlers(s.alertService, s.logger)
	auditHandlers := handlers.NewAuditHandlers(s.auditService, s.aiService, s.appStore, s.logger)
	rolloutHandlers := handlers.NewRolloutHandlers(s.rolloutService, s.logger)
	eventsHandlers := handlers.NewEventsHandlers(s.broker, s.logger)
	authHandlers := handlers.NewAuthHandlers(s.authService, s.logger)

	// Metrics endpoint — public so scrapers don't need a token.
	s.router.GET("/metrics", gin.WrapH(promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{})))

	// Health check — public so load balancers can probe.
	s.router.GET("/health", healthHandlers.HandleHealth)

	// PUBLIC route: GitHub doesn't authenticate to Squadron's API; the
	// HMAC signature IS the auth. Do NOT add auth middleware here.
	// v0.89.23 #639 Stream 40 — receives pull_request events from the
	// connected IaC repo, validates X-Hub-Signature-256, and emits
	// recommendation.pr_merged audit rows when action=="closed" &&
	// merged==true. The receiver is registered above the v1 auth group
	// so RequireBearer does NOT intercept; that is the design, not an
	// oversight. The webhook secret is read at startup from
	// SQUADRON_GITHUB_WEBHOOK_SECRET — an empty value mounts the route
	// in a 503-on-every-call state so the GitHub webhook delivery log
	// surfaces the misconfiguration cleanly.
	s.router.POST("/api/v1/webhooks/github", func(c *gin.Context) {
		h := handlers.NewIaCGitHubWebhookHandler(
			s.auditService,
			s.iacConnStore,
			s.iacGitHubWebhookSecret,
			s.logger,
		)
		// v0.89.30 (#649) — wire the dedupe store onto the per-
		// request handler so the replay-protection check is live.
		// Nil-safe; the handler logs and proceeds when unwired.
		if s.iacGitHubWebhookStore != nil {
			h = h.WithDedupeStore(s.iacGitHubWebhookStore)
		}
		// v0.89.31 (#650) — wire the credstore Key so the receiver
		// can unseal per-connection webhook secrets. Nil-safe; when
		// unwired, the per-connection lookup short-circuits and the
		// receiver falls back to the env-var global. The Key is the
		// same one the discovery substrate uses to seal PATs.
		if s.discoveryCredKey != nil {
			h = h.WithCredstoreKey(s.discoveryCredKey)
		}
		// v0.89.44 (#664 Stream 62, slice 1 chunk 3 of the GitHub
		// Checks API back-signal arc). Wire the chunk-3 follow-up
		// surfaces. All four are optional — when any one is unwired
		// the helper short-circuits silently per design doc §5
		// fail-open posture. Production wires iacWebhookChecksClient
		// against the same shared *iacgithub.PATClient chunk 2's
		// SetIaCChecksClient uses; appStore satisfies the slim
		// WebhookCheckRunStore interface directly via its
		// GetCheckRunForRecommendation + SetCheckRunForRecommendation
		// methods.
		if s.iacWebhookChecksClient != nil {
			h = h.WithChecksAPI(s.iacWebhookChecksClient)
		}
		if s.appStore != nil {
			h = h.WithCheckRunStore(s.appStore)
		}
		if strings.TrimSpace(s.iacWebhookChecksPAT) != "" {
			h = h.WithPAT(s.iacWebhookChecksPAT)
		}
		if strings.TrimSpace(s.squadronHost) != "" {
			h = h.WithSquadronHost(s.squadronHost)
		}
		h.HandleWebhook(c)
	})

	// API v1 routes
	v1 := s.router.Group("/api/v1")
	if s.authConfig.Enabled {
		// When auth is enabled, every /api/v1/* request must carry a
		// valid Bearer token. /metrics and /health above stay public.
		v1.Use(middleware.RequireBearer(s.authService, s.logger))
		// v0.52 — record an api.request audit event for every
		// authenticated mutating request, but only if the
		// build-edition wire layer installed the middleware.
		// OSS leaves accessAuditMiddleware nil so the per-call
		// evidence path is unmounted; service-layer events still
		// own the substantive state-change record. The Compliance
		// Pack build installs middleware.APIAccessAudit, which
		// produces the per-call evidence trail compliance
		// frameworks ask for.
		if s.accessAuditMiddleware != nil {
			v1.Use(s.accessAuditMiddleware)
		}
	}
	{
		// Auth token management lives under /api/v1/auth/tokens.
		// Bootstrap problem: the first token has to be created without
		// a token. That's handled by the bootstrap-token-on-first-start
		// flow in main.go — by the time operators reach this endpoint
		// they should already have a token to authenticate with.
		auth := v1.Group("/auth/tokens")
		{
			auth.GET("", middleware.RequireScope(services.ScopeAuthRead), authHandlers.HandleListTokens)
			auth.POST("", middleware.RequireScope(services.ScopeAuthWrite), authHandlers.HandleCreateToken)
			auth.POST("/:id/revoke", middleware.RequireScope(services.ScopeAuthWrite), authHandlers.HandleRevokeToken)
		}

		// Agent routes. GET is read; PATCH/POST modify the agent (group
		// assignment, config push, restart) and require write.
		agents := v1.Group("/agents")
		{
			agents.GET("", middleware.RequireScope(services.ScopeAgentsRead), agentHandlers.HandleGetAgents)
			agents.GET("/stats", middleware.RequireScope(services.ScopeAgentsRead), agentHandlers.HandleGetAgentStats)
			agents.GET("/:id", middleware.RequireScope(services.ScopeAgentsRead), agentHandlers.HandleGetAgent)
			agents.PATCH("/:id/group", middleware.RequireScope(services.ScopeAgentsWrite), agentHandlers.HandleUpdateAgentGroup)
			agents.POST("/:id/config", middleware.RequireScope(services.ScopeAgentsWrite), agentHandlers.HandleSendConfigToAgent)
			agents.POST("/:id/restart", middleware.RequireScope(services.ScopeAgentsWrite), agentHandlers.HandleRestartAgent)
			// v0.35: hard-delete the agent record for hosts that
			// have been retired from the fleet. Audit-logged via
			// the agent service's existing event publish.
			agents.DELETE("/:id", middleware.RequireScope(services.ScopeAgentsWrite), agentHandlers.HandleDecommissionAgent)
		}

		// Config routes. validate/lint/templates are read-shaped (they
		// don't mutate state even though they're POSTs by API design),
		// so they require configs:read. Create/update/delete need write.
		configs := v1.Group("/configs")
		{
			configs.GET("", middleware.RequireScope(services.ScopeConfigsRead), configHandlers.HandleGetConfigs)
			configs.POST("", middleware.RequireScope(services.ScopeConfigsWrite), configHandlers.HandleCreateConfig)
			configs.POST("/validate", middleware.RequireScope(services.ScopeConfigsRead), configHandlers.HandleValidateConfig)
			configs.POST("/lint", middleware.RequireScope(services.ScopeConfigsRead), configHandlers.HandleLintConfig)
			configs.GET("/templates", middleware.RequireScope(services.ScopeConfigsRead), configHandlers.HandleGetConfigTemplates)
			configs.GET("/templates/:id", middleware.RequireScope(services.ScopeConfigsRead), configHandlers.HandleGetConfigTemplate)
			configs.GET("/versions", middleware.RequireScope(services.ScopeConfigsRead), configHandlers.HandleGetConfigVersions)
			configs.GET("/:id", middleware.RequireScope(services.ScopeConfigsRead), configHandlers.HandleGetConfig)
			configs.PUT("/:id", middleware.RequireScope(services.ScopeConfigsWrite), configHandlers.HandleUpdateConfig)
			configs.DELETE("/:id", middleware.RequireScope(services.ScopeConfigsWrite), configHandlers.HandleDeleteConfig)
		}

		// Telemetry routes are all read-shaped (POSTs are queries, not
		// mutations). Saved queries are a CRUD library that piggybacks
		// on the same scope — there's no separate "saved query write"
		// scope for v0.10; if operators want stricter isolation, that's
		// a future scope subdivision.
		telemetry := v1.Group("/telemetry")
		telemetry.Use(middleware.RequireScope(services.ScopeTelemetryRead))
		{
			telemetry.POST("/metrics/query", telemetryHandlers.HandleQueryMetrics)
			savedQueries := telemetry.Group("/saved-queries")
			{
				savedQueries.GET("", savedQueryHandlers.HandleListSavedQueries)
				savedQueries.POST("", savedQueryHandlers.HandleCreateSavedQuery)
				savedQueries.PUT("/:id", savedQueryHandlers.HandleUpdateSavedQuery)
				savedQueries.DELETE("/:id", savedQueryHandlers.HandleDeleteSavedQuery)
			}

			telemetry.POST("/logs/query", telemetryHandlers.HandleQueryLogs)
			telemetry.POST("/traces/query", telemetryHandlers.HandleQueryTraces)
			telemetry.GET("/overview", telemetryHandlers.HandleGetTelemetryOverview)
			telemetry.GET("/services", telemetryHandlers.HandleGetServices)

			telemetry.POST("/query", squadronQLHandlers.HandleSquadronQLQuery)
			telemetry.POST("/query/validate", squadronQLHandlers.HandleValidateQuery)
			telemetry.POST("/query/suggestions", squadronQLHandlers.HandleGetSuggestions)
			telemetry.GET("/query/templates", squadronQLHandlers.HandleGetTemplates)
			telemetry.GET("/query/functions", squadronQLHandlers.HandleGetFunctions)
		}

		// Group routes. Restart is a write because it triggers an
		// operational change on every agent in the group.
		groups := v1.Group("/groups")
		{
			groups.GET("", middleware.RequireScope(services.ScopeGroupsRead), groupHandlers.HandleGetGroups)
			groups.POST("", middleware.RequireScope(services.ScopeGroupsWrite), groupHandlers.HandleCreateGroup)
			groups.GET("/:id", middleware.RequireScope(services.ScopeGroupsRead), groupHandlers.HandleGetGroup)
			groups.PUT("/:id", middleware.RequireScope(services.ScopeGroupsWrite), groupHandlers.HandleUpdateGroup)
			groups.DELETE("/:id", middleware.RequireScope(services.ScopeGroupsWrite), groupHandlers.HandleDeleteGroup)
			groups.POST("/:id/config", middleware.RequireScope(services.ScopeGroupsWrite), groupHandlers.HandleAssignConfig)
			groups.GET("/:id/config", middleware.RequireScope(services.ScopeGroupsRead), groupHandlers.HandleGetGroupConfig)
			groups.GET("/:id/agents", middleware.RequireScope(services.ScopeAgentsRead), groupHandlers.HandleGetGroupAgents)
			groups.POST("/:id/restart", middleware.RequireScope(services.ScopeAgentsWrite), groupHandlers.HandleRestartGroup)
		}

		// Topology routes are read-only views over agents + telemetry.
		topology := v1.Group("/topology")
		topology.Use(middleware.RequireScope(services.ScopeAgentsRead))
		{
			topology.GET("", topologyHandlers.HandleGetTopology)
			topology.GET("/agent/:id", topologyHandlers.HandleGetAgentTopology)
			topology.GET("/group/:id", topologyHandlers.HandleGetGroupTopology)
		}

		// Alert rule routes
		alerts := v1.Group("/alerts/rules")
		{
			alerts.GET("", middleware.RequireScope(services.ScopeAlertsRead), alertHandlers.HandleListAlertRules)
			alerts.POST("", middleware.RequireScope(services.ScopeAlertsWrite), alertHandlers.HandleCreateAlertRule)
			alerts.GET("/:id", middleware.RequireScope(services.ScopeAlertsRead), alertHandlers.HandleGetAlertRule)
			alerts.PUT("/:id", middleware.RequireScope(services.ScopeAlertsWrite), alertHandlers.HandleUpdateAlertRule)
			alerts.DELETE("/:id", middleware.RequireScope(services.ScopeAlertsWrite), alertHandlers.HandleDeleteAlertRule)
		}

		// Real-time event stream (Server-Sent Events). Stream carries
		// state-change events across every domain; the audit:read scope
		// is the closest match since the events are largely the same
		// shapes the audit log records.
		v1.GET("/events/stream", middleware.RequireScope(services.ScopeAuditRead), eventsHandlers.HandleStream)

		// Audit log — read-only, with the v0.57 explain side door.
		// The explain endpoint mutates exactly one row (the requested
		// one, to cache the AI explanation) but the operator is only
		// allowed to do that for rows they can already read, so we
		// keep both routes behind ScopeAuditRead.
		audit := v1.Group("/audit")
		audit.Use(middleware.RequireScope(services.ScopeAuditRead))
		{
			audit.GET("/events", auditHandlers.HandleListAuditEvents)
			audit.POST("/:id/explain", auditHandlers.HandleExplainAuditEvent)
		}

		// v0.40.0 Timeline — postmortem view that merges audit,
		// deploy, and cost-spike events into one chronologically
		// sorted stream. Read-only; gated by audit-read since the
		// merged data is a strict subset of what the audit log
		// already exposes.
		v1.GET("/timeline",
			middleware.RequireScope(services.ScopeAuditRead),
			func(c *gin.Context) {
				handlers.NewTimelineHandlers(
					s.auditService,
					s.deploy,
					s.costSpikes,
					s.logger,
				).HandleList(c)
			})

		// Rollouts — safe staged config deployment with automatic rollback.
		rollouts := v1.Group("/rollouts")
		{
			rollouts.GET("", middleware.RequireScope(services.ScopeRolloutsRead), rolloutHandlers.HandleListRollouts)
			rollouts.POST("", middleware.RequireScope(services.ScopeRolloutsWrite), rolloutHandlers.HandleCreateRollout)
			// v0.73 — plan create. N rollout inputs become a single
			// plan with shared PlanID assigned server side. The
			// engine support for plans landed in v0.70-72; this
			// endpoint is the surface clients use to actually
			// produce one. Same scope as regular Create because
			// creating a plan is conceptually N rollout creates.
			// Read/list endpoint for plans lands in v0.74.
			rollouts.POST("/plans", middleware.RequireScope(services.ScopeRolloutsWrite), rolloutHandlers.HandleCreatePlan)
			// v0.74 — plan read. Returns the envelope (forward steps
			// + rollback steps + derived state). Same scope as
			// /rollouts/:id since the data is a view over rollouts.
			rollouts.GET("/plans/:id", middleware.RequireScope(services.ScopeRolloutsRead), rolloutHandlers.HandleGetPlan)
			// v0.89.2 — plan list backfill (#554). The v0.77
			// squadronctl plans subcommand shipped get/create only
			// and deferred list as a backfill; this is the
			// matching server endpoint. Same scope as plans/:id —
			// list is a read over rollouts the same token already
			// reaches via GET /rollouts.
			rollouts.GET("/plans", middleware.RequireScope(services.ScopeRolloutsRead), rolloutHandlers.HandleListPlans)
			rollouts.GET("/:id", middleware.RequireScope(services.ScopeRolloutsRead), rolloutHandlers.HandleGetRollout)
			rollouts.POST("/:id/abort", middleware.RequireScope(services.ScopeRolloutsWrite), rolloutHandlers.HandleAbortRollout)
			rollouts.POST("/:id/pause", middleware.RequireScope(services.ScopeRolloutsWrite), rolloutHandlers.HandlePauseRollout)
			rollouts.POST("/:id/resume", middleware.RequireScope(services.ScopeRolloutsWrite), rolloutHandlers.HandleResumeRollout)
			// v0.60 — one click rollback. Creates a new rollout that
			// targets the source's previous config. Routed under
			// rollouts:write because the operator is creating a new
			// rollout (just one with a predetermined target).
			rollouts.POST("/:id/rollback", middleware.RequireScope(services.ScopeRolloutsWrite), rolloutHandlers.HandleRollBackRollout)
			// v0.47 — approval workflow.
			// v0.48 — separated scope: approval requires the
			// dedicated rollouts:approve grant, not rollouts:write.
			// This is the runtime separation of duties — an
			// operator with only rollouts:write can fire a
			// rollout but can't approve it; an approver needs an
			// explicit grant. The two-person rule (requester ≠
			// approver) is still enforced inside the service for
			// the case where someone legitimately holds both
			// scopes.
			rollouts.POST("/:id/approve", middleware.RequireScope(services.ScopeRolloutsApprove), rolloutHandlers.HandleApproveRollout)
			rollouts.POST("/:id/reject", middleware.RequireScope(services.ScopeRolloutsApprove), rolloutHandlers.HandleRejectRollout)
			// v0.89.26 (#642 Stream 43) — per-rollout
			// exclude-from-learning toggle for the #531 slice 2
			// feedback loop (§10 Q3). Routed under rollouts:write
			// because the operator is mutating a single rollout's
			// metadata (the flag), not approving or creating one.
			// The rollouts:approve scope is reserved for the
			// state-changing approve/reject pair.
			rollouts.POST("/:id/exclude-from-learning", middleware.RequireScope(services.ScopeRolloutsWrite), rolloutHandlers.HandleExcludeFromLearning)
		}

		// Rollout recipe cookbook. Sibling of /rollouts (not nested)
		// to avoid Gin's static-vs-parametric route conflict with
		// /rollouts/:id. Both endpoints are cache-friendly — they
		// change only on Squadron upgrade. rollouts:read gates them so
		// a read-only operator can see what shapes are available even
		// though they can't create rollouts.
		v1.GET("/rollout-recipes/abort-criteria",
			middleware.RequireScope(services.ScopeRolloutsRead),
			rolloutHandlers.HandleListAbortCriteriaRecipes)
		v1.GET("/rollout-recipes/templates",
			middleware.RequireScope(services.ScopeRolloutsRead),
			rolloutHandlers.HandleListRolloutTemplates)

		// Rollout preview — diff + lint between a group's current
		// effective config and a target config, for the create-form
		// "are you sure?" pane. Sibling of /rollouts for the same
		// routing-conflict reason.
		v1.GET("/rollout-preview",
			middleware.RequireScope(services.ScopeRolloutsRead),
			rolloutHandlers.HandlePreviewRollout)

		// Telemetry Volume Insights (v0.24+). Read-only data
		// surface for "where are my telemetry bytes going". The
		// v0.25 cost-recommendation engine reads from these
		// endpoints; keep the response shapes stable. Mounted
		// behind ScopeAgentsRead — same scope that gates the
		// agents list, since the insights are an aggregation of
		// the same underlying telemetry.
		//
		// Routes are registered unconditionally; each handler
		// re-checks s.insightsService at request time and 503s
		// if it's still nil. This lets main.go wire the service
		// via SetInsightsService AFTER NewServer constructs the
		// route table (the alternative — make every existing
		// caller of NewServer take another argument — has more
		// blast radius than this trampoline).
		v1.GET("/insights/volume",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.insightsTrampoline(func(h *handlers.InsightsHandlers, c *gin.Context) { h.HandleFleetVolume(c) }))
		v1.GET("/insights/volume/agents",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.insightsTrampoline(func(h *handlers.InsightsHandlers, c *gin.Context) { h.HandleTopAgents(c) }))
		v1.GET("/insights/volume/agents/:id",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.insightsTrampoline(func(h *handlers.InsightsHandlers, c *gin.Context) { h.HandleAgentVolume(c) }))
		v1.GET("/insights/volume/attributes",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.insightsTrampoline(func(h *handlers.InsightsHandlers, c *gin.Context) { h.HandleTopAttributes(c) }))
		v1.GET("/insights/volume/drops",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.insightsTrampoline(func(h *handlers.InsightsHandlers, c *gin.Context) { h.HandleDrops(c) }))

		// Cost Recommendations (v0.25). Heuristic advice layered on
		// top of the v0.24 insights surface. Reads are
		// ScopeAgentsRead (same gating as the underlying volume
		// data); dismiss/restore mutations require ScopeAgentsWrite
		// because they shape what other operators see.
		v1.GET("/recommendations",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.recommendationsTrampoline(func(h *handlers.RecommendationsHandlers, c *gin.Context) { h.HandleList(c) }))
		v1.GET("/recommendations/agents/:id",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.recommendationsTrampoline(func(h *handlers.RecommendationsHandlers, c *gin.Context) { h.HandleListForAgent(c) }))
		v1.GET("/recommendations/dismissals",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.recommendationsTrampoline(func(h *handlers.RecommendationsHandlers, c *gin.Context) { h.HandleListDismissals(c) }))
		v1.POST("/recommendations/:id/dismiss",
			middleware.RequireScope(services.ScopeAgentsWrite),
			s.recommendationsTrampoline(func(h *handlers.RecommendationsHandlers, c *gin.Context) { h.HandleDismiss(c) }))
		v1.POST("/recommendations/:id/restore",
			middleware.RequireScope(services.ScopeAgentsWrite),
			s.recommendationsTrampoline(func(h *handlers.RecommendationsHandlers, c *gin.Context) { h.HandleRestore(c) }))

		// v0.26 AI assist. Wraps the Anthropic Messages API; off
		// by default. All routes behind ScopeAgentsRead since
		// they're read-only assistive surfaces (no state changes,
		// no fan-out, no agent commands). The /status route stays
		// responsive even when AI is unwired so the UI's
		// capability probe is a single round-trip; the other
		// routes 503 with an opt-in message.
		v1.GET("/ai/status",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.aiStatusTrampoline())
		v1.POST("/ai/explain",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.aiTrampoline(func(h *handlers.AIHandlers, c *gin.Context) { h.HandleExplainSnippet(c) }))
		v1.POST("/ai/merge",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.aiTrampoline(func(h *handlers.AIHandlers, c *gin.Context) { h.HandleMergeIntoConfig(c) }))
		v1.POST("/ai/explain-config",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.aiTrampoline(func(h *handlers.AIHandlers, c *gin.Context) { h.HandleExplainConfig(c) }))
		// v0.44 — natural-language fleet query. Same agents-read
		// scope since the result is just filter params for /agents.
		v1.POST("/ai/fleet-query",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.aiTrampoline(func(h *handlers.AIHandlers, c *gin.Context) { h.HandleFleetQuery(c) }))
		// v0.84 — proposer playground preview. Non-persisting preview
		// of the proposer's response for the operator-facing playground
		// UI. Same agents-read scope: no rollouts / plans / audit
		// events are created; the operator is just dogfooding the
		// proposer's reasoning. Anyone who can read the fleet can
		// preview a proposal against it.
		v1.POST("/ai/proposer/preview",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.aiTrampoline(func(h *handlers.AIHandlers, c *gin.Context) { h.HandleProposerPreview(c) }))
		// v0.44 — auto-remediate lint warnings. The remediated YAML
		// flows through the normal save / rollout path, so no new
		// scope is needed — config-write happens at save time.
		v1.POST("/ai/remediate-lint",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.aiTrampoline(func(h *handlers.AIHandlers, c *gin.Context) { h.HandleRemediateLint(c) }))
		// v0.63 — conversational Ask Squadron. The handler walks a
		// small read context (recent rollouts + audit events) and
		// hands the question to the AI service. Same agents-read
		// scope: the answer's content is bounded by what the same
		// token could read by hitting /rollouts and /audit/events
		// directly. Coining a new scope would be vocabulary noise.
		v1.POST("/ai/ask",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.askTrampoline())

		// v0.85 Stream 2C — connector wizard pre-commit validation.
		// POST a transient role-ARN + external-ID + region triple;
		// the handler runs sts:AssumeRole + a tiny EC2/Lambda probe
		// in-memory and returns the typed ValidationResult the
		// wizard renders as its "what just happened" panel. ZERO
		// records created — neither the credstore nor the audit log
		// is touched. Save lands in a separate later endpoint.
		//
		// agents:read is the right scope: the operator is reading
		// the connect-account capability surface, not yet mutating
		// any persisted Squadron state. A future connect-write scope
		// gates the Save endpoint.
		v1.POST("/discovery/aws/validate",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.discoveryTrampoline(func(h *handlers.DiscoveryHandlers, c *gin.Context) { h.HandleAWSValidate(c) }))

		// v0.85 Stream 2D — connector wizard Save endpoint.
		// Persists the trust-policy metadata via credstore, emits a
		// discovery.aws.connection_created audit event, and returns
		// {connection_id, status:"connected"} on success. ZERO writes
		// happen when validation fails — the handler re-runs
		// scanner.Validate one last time before persisting to catch a
		// role edit between the wizard's Validate step and Save.
		//
		// agents:write is the right scope: this is the first real
		// substrate write on the discovery path. Validate stays on
		// agents:read because it's a probe; Save is the mutation.
		v1.POST("/discovery/aws/connections",
			middleware.RequireScope(services.ScopeAgentsWrite),
			s.discoveryTrampoline(func(h *handlers.DiscoveryHandlers, c *gin.Context) { h.HandleAWSSaveConnection(c) }))

		// v0.85 Stream 2E — connector list endpoint. Returns the
		// display fields of every stored AWS connection so the
		// /discovery/aws page's Account tab can render its connection
		// cards. NEVER returns the role ARN, the ExternalId, or any
		// encrypted credential bytes — operators see "this account is
		// connected" but cannot read back trust-policy material.
		//
		// agents:read is the right scope: this is a read-only view of
		// the substrate that mirrors what the Save endpoint accepted.
		v1.GET("/discovery/aws/connections",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.discoveryTrampoline(func(h *handlers.DiscoveryHandlers, c *gin.Context) { h.HandleAWSListConnections(c) }))

		// v0.85 Stream 2E — on-demand scan endpoint. Looks up the
		// connection, emits discovery.aws.scan_started, runs the
		// scanner synchronously, emits discovery.aws.scan_completed
		// with per-category counts + the partial flag, and returns
		// the typed scanner.Result as JSON.
		//
		// Known trade-off: slice 1 scans block the HTTP request. A
		// large account could hang the request for minutes — slice 3's
		// scheduled-scan engine will move to async with persisted
		// results. The route stays stable.
		//
		// agents:read is the right scope: the scan creates audit events
		// but no persisted Squadron state (no inventory_aws table in
		// slice 1) — the operator is reading the cloud's current
		// state, mediated by Squadron's read-only role.
		v1.POST("/discovery/aws/connections/:id/scan",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.discoveryTrampoline(func(h *handlers.DiscoveryHandlers, c *gin.Context) { h.HandleAWSRunScan(c) }))

		// v0.89.7a Stream 21 (#616) — multi-account AWS scan-all.
		// Fans out per-account scans across every stored AWS
		// connection in the credstore, with bounded concurrency
		// (default 3, max 8), aggregates the result, and emits one
		// discovery.aws.scan_all_completed audit event. The
		// per-account discovery.aws.scan_completed events still
		// fire — the aggregate is in addition, not a replacement;
		// the two link via the scan_all_id payload field. The
		// per-account endpoint above is completely unchanged.
		//
		// agents:read for the same reason the per-account scan uses
		// it: the orchestrator creates audit events but no
		// persisted Squadron state — the operator is reading every
		// connected cloud's current state, mediated by Squadron's
		// read-only roles. The aggregate "writes" only audit rows,
		// which all read endpoints already do; agents:write is
		// reserved for state-mutating endpoints (Save, Delete).
		//
		// Curl-friendly contract: optional regions + concurrency
		// query parameters; no request body needed. Operators
		// driving this from ops scripts pre-UI (v0.89.7b ships the
		// UI surface) get a fully self-describing JSON response
		// with the aggregate counts + per-account roll-up.
		v1.POST("/discovery/aws/scan-all",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.discoveryTrampoline(func(h *handlers.DiscoveryHandlers, c *gin.Context) { h.HandleAWSScanAll(c) }))

		// v0.85 Stream 2F — discovery-side AI recommendations route.
		// The Inventory tab POSTs the scan result it just rendered; the
		// handler converts it into an ai.DiscoveryScanContext, asks the
		// proposer for an instrumentation plan, and walks the plan-kind
		// result into a slice of typed Recommendations (one per plan
		// step) whose IaC field carries the Terraform the operator
		// runs through their existing IaC pipeline. Squadron never
		// executes the Terraform — the design doc's thesis line stands.
		//
		// Registered through discoveryAITrampoline so AI-disabled
		// deployments 503 here without breaking the list/scan/validate
		// routes (which stay reachable via discoveryTrampoline).
		//
		// agents:read is the right scope: the recommendation generation
		// emits an audit event but creates no persisted Squadron state
		// in slice 1 (recommendations themselves are not persisted in
		// the OSS surface — they're returned per request and rendered
		// in the UI; persistence lands as part of the dismissals path).
		v1.POST("/discovery/aws/connections/:id/recommendations",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.discoveryAITrampoline(func(h *handlers.DiscoveryHandlers, c *gin.Context) { h.HandleAWSGenerateRecommendations(c) }))

		// v0.89.209 — async recommendations poll. The kick-off (the POST
		// above) returns 202 + a job_id; the UI polls this until the job
		// is succeeded (carries the recommendations) or failed. Provider-
		// agnostic: the job id is globally unique, so one route serves
		// every cloud. Pure read of the in-process job store (agents:read);
		// runs on discoveryTrampoline, which shares the process-wide
		// default job store with the kick-off handler.
		v1.GET("/discovery/recommendations/jobs/:jobID",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.discoveryTrampoline(func(h *handlers.DiscoveryHandlers, c *gin.Context) { h.HandleRecommendationJobStatus(c) }))

		// v0.89.37 (#656 Stream 54, #531 slice 2 chunk 4) — operator-
		// set exclusion endpoint. The Recommendations tab POSTs here
		// when the operator clicks the "Don't propose this again"
		// button. Persists the verdict in iac_recommendation_verdicts
		// and emits a discovery_recommendation.excluded / .exclude_
		// cleared audit event on state transitions (no-op toggles
		// produce no audit row).
		//
		// agents:write because the route mutates substrate state. The
		// affordance is per-recommendation; the next discovery proposal
		// in the same scope will see the row via the discovery
		// bridge's ListExcludedRecommendations sweep and drop the
		// kind from its prompt block.
		v1.POST("/discovery/aws/recommendations/exclude",
			middleware.RequireScope(services.ScopeAgentsWrite),
			s.discoveryTrampoline(func(h *handlers.DiscoveryHandlers, c *gin.Context) { h.HandleAWSRecommendationExclude(c) }))

		// v0.89.40 (#660 Stream 58, #531 slice 2 chunk 5 follow-on) —
		// read-side of the operator-set exclusion table. The
		// Recommendations tab GETs this on mount to hydrate its
		// excludedSet from the persisted iac_recommendation_verdicts
		// rows, so the Excluded badges survive a page refresh. Chunk 5
		// shipped the POST half with a TODO acknowledging the
		// refresh-loses-state gap; this closes that gap without
		// changing chunk 4's schema or chunk 5's toggle behavior.
		//
		// agents:read because this is a pure read endpoint — no
		// substrate mutation, no audit emission. The same auth posture
		// as the other list/scan endpoints on this surface.
		v1.GET("/discovery/aws/recommendations/excluded",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.discoveryTrampoline(func(h *handlers.DiscoveryHandlers, c *gin.Context) { h.HandleAWSRecommendationListExcluded(c) }))

		// Recs-UI parity (v0.89.201) — provider-agnostic aliases for the
		// exclusion endpoints. HandleAWSRecommendationExclude /
		// ListExcluded key off the request-body scope (account_id /
		// region / kind), not the URL, so the GCP / Azure / OCI
		// Recommendations tabs reuse them with their own scope. The
		// /aws/ routes above stay for backward compat.
		v1.POST("/discovery/recommendations/exclude",
			middleware.RequireScope(services.ScopeAgentsWrite),
			s.discoveryTrampoline(func(h *handlers.DiscoveryHandlers, c *gin.Context) { h.HandleAWSRecommendationExclude(c) }))
		v1.GET("/discovery/recommendations/excluded",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.discoveryTrampoline(func(h *handlers.DiscoveryHandlers, c *gin.Context) { h.HandleAWSRecommendationListExcluded(c) }))

		// v0.89.47 (#667 Stream 67, GCP discovery slice 1 chunk 3) —
		// GCP-side mirror of the /discovery/aws/* surface. Per design
		// doc §6 the route shapes mirror the AWS counterparts so the
		// wizard, the API consumers, and the SIEM forwarders see one
		// consistent pattern across providers. The handler is built
		// per-request through discoveryGCPTrampoline; the trampoline
		// 503s when SetGCPDiscoveryStore is unwired so test_server.go
		// stays unaffected.
		//
		// Auth posture mirrors the AWS surface: agents:read on the
		// list / get / validate / scan / recommendations routes
		// (those create audit events but no Squadron-persisted state
		// in slice 1); agents:write on Create / Update / Delete (all
		// three mutate the gcp_connections substrate). The scan route
		// uses agents:read for the same reason its AWS counterpart
		// does: the scan creates audit rows but no persisted scan
		// result; the operator is reading the cloud's state mediated
		// by Squadron's read-only SA.
		v1.POST("/discovery/gcp/connections",
			middleware.RequireScope(services.ScopeAgentsWrite),
			s.discoveryGCPTrampoline(func(h *handlers.DiscoveryGCPHandlers, c *gin.Context) { h.HandleCreateGCPConnection(c) }))
		v1.GET("/discovery/gcp/connections",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.discoveryGCPTrampoline(func(h *handlers.DiscoveryGCPHandlers, c *gin.Context) { h.HandleListGCPConnections(c) }))
		v1.GET("/discovery/gcp/connections/:id",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.discoveryGCPTrampoline(func(h *handlers.DiscoveryGCPHandlers, c *gin.Context) { h.HandleGetGCPConnection(c) }))
		v1.PATCH("/discovery/gcp/connections/:id",
			middleware.RequireScope(services.ScopeAgentsWrite),
			s.discoveryGCPTrampoline(func(h *handlers.DiscoveryGCPHandlers, c *gin.Context) { h.HandleUpdateGCPConnection(c) }))
		v1.DELETE("/discovery/gcp/connections/:id",
			middleware.RequireScope(services.ScopeAgentsWrite),
			s.discoveryGCPTrampoline(func(h *handlers.DiscoveryGCPHandlers, c *gin.Context) { h.HandleDeleteGCPConnection(c) }))
		v1.POST("/discovery/gcp/connections/:id/validate",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.discoveryGCPTrampoline(func(h *handlers.DiscoveryGCPHandlers, c *gin.Context) { h.HandleValidateGCPConnection(c) }))
		v1.POST("/discovery/gcp/connections/:id/scan",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.discoveryGCPTrampoline(func(h *handlers.DiscoveryGCPHandlers, c *gin.Context) { h.HandleScanGCPConnection(c) }))
		v1.POST("/discovery/gcp/connections/:id/recommendations",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.discoveryGCPTrampoline(func(h *handlers.DiscoveryGCPHandlers, c *gin.Context) { h.HandleRecommendationsForGCPScan(c) }))

		// v0.89.52 (#676 Stream 74, Azure discovery slice 1 chunk 3) —
		// Azure-side mirror of the /discovery/aws/* and /discovery/gcp/*
		// surfaces. Per design doc §7 the route shapes mirror the AWS /
		// GCP counterparts so the wizard, the API consumers, and the
		// SIEM forwarders see one consistent pattern across providers.
		// The handler is built per-request through
		// discoveryAzureTrampoline; the trampoline 503s when
		// SetAzureDiscoveryStore is unwired so test_server.go stays
		// unaffected.
		//
		// Auth posture mirrors the AWS / GCP surfaces: agents:read on
		// the list / get / validate / scan / recommendations routes
		// (those create audit events but no Squadron-persisted state
		// in slice 1); agents:write on Create / Update / Delete (all
		// three mutate the azure_connections substrate).
		v1.POST("/discovery/azure/connections",
			middleware.RequireScope(services.ScopeAgentsWrite),
			s.discoveryAzureTrampoline(func(h *handlers.DiscoveryAzureHandlers, c *gin.Context) { h.HandleCreateAzureConnection(c) }))
		v1.GET("/discovery/azure/connections",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.discoveryAzureTrampoline(func(h *handlers.DiscoveryAzureHandlers, c *gin.Context) { h.HandleListAzureConnections(c) }))
		v1.GET("/discovery/azure/connections/:id",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.discoveryAzureTrampoline(func(h *handlers.DiscoveryAzureHandlers, c *gin.Context) { h.HandleGetAzureConnection(c) }))
		v1.PATCH("/discovery/azure/connections/:id",
			middleware.RequireScope(services.ScopeAgentsWrite),
			s.discoveryAzureTrampoline(func(h *handlers.DiscoveryAzureHandlers, c *gin.Context) { h.HandleUpdateAzureConnection(c) }))
		v1.DELETE("/discovery/azure/connections/:id",
			middleware.RequireScope(services.ScopeAgentsWrite),
			s.discoveryAzureTrampoline(func(h *handlers.DiscoveryAzureHandlers, c *gin.Context) { h.HandleDeleteAzureConnection(c) }))
		v1.POST("/discovery/azure/connections/:id/validate",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.discoveryAzureTrampoline(func(h *handlers.DiscoveryAzureHandlers, c *gin.Context) { h.HandleValidateAzureConnection(c) }))
		v1.POST("/discovery/azure/connections/:id/scan",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.discoveryAzureTrampoline(func(h *handlers.DiscoveryAzureHandlers, c *gin.Context) { h.HandleScanAzureConnection(c) }))
		v1.POST("/discovery/azure/connections/:id/recommendations",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.discoveryAzureTrampoline(func(h *handlers.DiscoveryAzureHandlers, c *gin.Context) { h.HandleRecommendationsForAzureScan(c) }))

		// v0.89.57 (#683 Stream 81, OCI discovery slice 1 chunk 3) —
		// OCI-side mirror of the /discovery/aws/*, /discovery/gcp/*,
		// and /discovery/azure/* surfaces. Per design doc §7 the route
		// shapes mirror the AWS / GCP / Azure counterparts so the
		// wizard, the API consumers, and the SIEM forwarders see one
		// consistent pattern across all four providers. The handler is
		// built per-request through discoveryOCITrampoline; the
		// trampoline 503s when SetOCIDiscoveryStore is unwired so
		// test_server.go stays unaffected.
		//
		// Auth posture mirrors the AWS / GCP / Azure surfaces:
		// agents:read on the list / get / validate / scan /
		// recommendations routes (those create audit events but no
		// Squadron-persisted state in slice 1); agents:write on
		// Create / Update / Delete (all three mutate the
		// oci_connections substrate).
		v1.POST("/discovery/oci/connections",
			middleware.RequireScope(services.ScopeAgentsWrite),
			s.discoveryOCITrampoline(func(h *handlers.DiscoveryOCIHandlers, c *gin.Context) { h.HandleCreateOCIConnection(c) }))
		v1.GET("/discovery/oci/connections",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.discoveryOCITrampoline(func(h *handlers.DiscoveryOCIHandlers, c *gin.Context) { h.HandleListOCIConnections(c) }))
		v1.GET("/discovery/oci/connections/:id",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.discoveryOCITrampoline(func(h *handlers.DiscoveryOCIHandlers, c *gin.Context) { h.HandleGetOCIConnection(c) }))
		v1.PATCH("/discovery/oci/connections/:id",
			middleware.RequireScope(services.ScopeAgentsWrite),
			s.discoveryOCITrampoline(func(h *handlers.DiscoveryOCIHandlers, c *gin.Context) { h.HandleUpdateOCIConnection(c) }))
		v1.DELETE("/discovery/oci/connections/:id",
			middleware.RequireScope(services.ScopeAgentsWrite),
			s.discoveryOCITrampoline(func(h *handlers.DiscoveryOCIHandlers, c *gin.Context) { h.HandleDeleteOCIConnection(c) }))
		v1.POST("/discovery/oci/connections/:id/validate",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.discoveryOCITrampoline(func(h *handlers.DiscoveryOCIHandlers, c *gin.Context) { h.HandleValidateOCIConnection(c) }))
		v1.POST("/discovery/oci/connections/:id/scan",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.discoveryOCITrampoline(func(h *handlers.DiscoveryOCIHandlers, c *gin.Context) { h.HandleScanOCIConnection(c) }))
		v1.POST("/discovery/oci/connections/:id/recommendations",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.discoveryOCITrampoline(func(h *handlers.DiscoveryOCIHandlers, c *gin.Context) { h.HandleRecommendationsForOCIScan(c) }))

		// v0.89.61 (#688 Stream 86, Unified Discovery dashboard slice 1
		// chunk 1) — cross-provider summary endpoint. Aggregates the
		// per-provider connection counts, last-scan timestamps, instance
		// inventory, and recent recommendations into one JSON shape so
		// the unified /discovery dashboard renders in a single round-
		// trip. Per design doc §7 contract item 5 the route sits under
		// the existing auth middleware; agents:read is the right scope
		// because the endpoint mutates nothing (audit cache-miss emit
		// aside, which mirrors the read-side audit posture every other
		// list endpoint already has).
		//
		// The summary trampoline tolerates ALL FOUR provider stores
		// being nil — a fresh-install Squadron with no clouds connected
		// still serves a 200 with the welcome empty state (every
		// provider card enabled=false). This is by design per §6
		// "Empty state". The per-provider trampolines 503 in the same
		// posture because their endpoints REQUIRE the per-provider
		// store to do anything useful; the summary handler can
		// legitimately return zero-counts.
		v1.GET("/discovery/summary",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.discoverySummaryTrampoline(func(h *handlers.DiscoverySummaryHandlers, c *gin.Context) { h.HandleSummary(c) }))

		// v0.89.76 (#707 Stream 105, Trace integration slice 1 chunk 3) —
		// trace coverage endpoint. Joins discovery inventory (sourced
		// from the same scan_completed audit projection /discovery/summary
		// reads) against the in-process traceindex shipped by chunks 1+2
		// to answer "of the resources Squadron has inventoried, how many
		// have actually emitted spans recently." Per design doc §7 the
		// route mounts under the existing auth middleware; agents:read
		// matches the rest of the read-shaped discovery surface and the
		// /discovery/summary precedent.
		//
		// Same nil-tolerant posture as the summary trampoline: an
		// operator on a fresh install with no clouds connected AND no
		// spans observed still sees a 200 with every provider key
		// populated as zero-counts. The Discovery dashboard panel
		// renders the cold-start "Run a discovery scan to populate the
		// trace coverage view" message inside the panel rather than
		// hiding it.
		v1.GET("/discovery/trace_coverage",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.discoveryTraceCoverageTrampoline(func(h *handlers.DiscoveryTraceCoverageHandlers, c *gin.Context) { h.HandleTraceCoverage(c) }))

		// v0.89.86 (#717 Stream 115, Span quality slice 1 chunk 2) —
		// span quality aggregate + per-resource detail endpoints.
		// Mirrors trace_coverage's nil-tolerant posture: a deployment
		// that hasn't observed any spans yet sees a 200 with every
		// provider key populated as zero-counts on the aggregate, and
		// 404 on the per-resource detail. Same ScopeAgentsRead gate as
		// the rest of the discovery read surface.
		//
		// HandleSpanQuality emits AuditEventSpanQualityRequested on
		// cache MISS only (the 30s cache window short-circuits the
		// emission to avoid drowning the timeline in dashboard polls);
		// HandleResourceSpanQuality does NOT emit (operator-clicked
		// drill-down, low timeline value).
		v1.GET("/discovery/span_quality",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.discoverySpanQualityTrampoline(func(h *handlers.DiscoverySpanQualityHandlers, c *gin.Context) { h.HandleSpanQuality(c) }))
		v1.GET("/discovery/:provider/inventory/:kind/:id/span_quality",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.discoverySpanQualityTrampoline(func(h *handlers.DiscoverySpanQualityHandlers, c *gin.Context) { h.HandleResourceSpanQuality(c) }))

		// v0.89.132 (#772 Stream 170, Workload Health dashboard panel
		// slice 1 chunk 1) — workload health endpoint. Aggregates the
		// per-provider serverless cold-start + sampling + error-rate
		// detection counts into one JSON shape so the new WORKLOAD
		// HEALTH dashboard panel renders in a single round-trip. Per
		// design doc §6 the route sits under the existing auth
		// middleware; agents:read matches the rest of the read-side
		// discovery surface.
		//
		// Same nil-tolerant posture as the trace_coverage trampoline:
		// a deployment that hasn't wired the per-provider serverless
		// inventory annotators sees every provider populated as zero
		// counts and the UI panel hides itself per design doc §5.3.
		//
		// HandleWorkloadHealth emits
		// AuditEventDiscoveryWorkloadHealthRequested on cache MISS
		// only — the 30s cache window short-circuits the emission so
		// the timeline doesn't drown in dashboard polls.
		v1.GET("/discovery/workload_health",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.discoveryWorkloadHealthTrampoline(func(h *handlers.DiscoveryWorkloadHealthHandlers, c *gin.Context) { h.HandleWorkloadHealth(c) }))

		// v0.89.114 (#752 Stream 150, Cold-start latency slice 1 chunk
		// 2) — per-resource cold-start observation endpoint. Returns the
		// composed current-window + baseline-window + ratio shape per
		// design doc §6.1. Same ScopeAgentsRead gate as the rest of the
		// discovery read surface. 404 when no observations exist yet for
		// the resource (matching the cold-start posture of the trace
		// coverage and span quality per-resource detail endpoints).
		// The kind segment is fixed at "serverless" in slice 1 — the
		// route shape preserves the per-provider /:provider/inventory/
		// :kind/:id/... convention so chunk 3 can extend other kinds
		// (compute / database / orchestration / event_source) without a
		// breaking URL change.
		v1.GET("/discovery/:provider/inventory/serverless/:id/cold_start",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.discoveryServerlessColdStartTrampoline(func(h *handlers.DiscoveryServerlessColdStartHandlers, c *gin.Context) { h.HandleColdStart(c) }))

		// v0.89.123 (#763 Stream 161, Sampling rate analysis slice 1
		// chunk 2) — per-resource sampling endpoint. Returns the
		// composed observed_span_count / expected_invocation_count
		// ratio + would_fire shape per design doc §6.1. Same
		// ScopeAgentsRead gate as the cold-start sibling. 404 when no
		// observations exist yet for the resource (matching the
		// cold-start posture). The kind segment is fixed at
		// "serverless" — chunk 3 may extend to other kinds in future
		// slices without a breaking URL change.
		v1.GET("/discovery/:provider/inventory/serverless/:id/sampling",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.discoveryServerlessSamplingTrampoline(func(h *handlers.DiscoveryServerlessSamplingHandlers, c *gin.Context) { h.HandleSampling(c) }))

		// v0.89.128 (#768 Stream 166, Error rate correlation slice 1
		// chunk 2) — per-resource error rate endpoint. Returns the
		// composed current/baseline error rate ratio + would_fire
		// shape per design doc §6.1. Same ScopeAgentsRead gate as
		// the cold-start sibling. 404 when no observations exist yet
		// for the resource. The kind segment is fixed at "serverless"
		// — chunk 3 may extend to other kinds in future slices
		// without a breaking URL change.
		v1.GET("/discovery/:provider/inventory/serverless/:id/error_rate",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.discoveryServerlessErrorRateTrampoline(func(h *handlers.DiscoveryServerlessErrorRateHandlers, c *gin.Context) { h.HandleErrorRate(c) }))

		// v0.89.3 Stream 19 (#603) — Connect IaC repo, slice 1
		// (GitHub PAT). Validate is a test-before-commit preflight
		// that reads the repo + each placement-map file; it emits
		// iac.github.connection_validated for the audit timeline but
		// creates ZERO records on the substrate side. Save persists
		// the connection (with the PAT sealed via the credstore
		// Key) and emits iac.github.connection_created. List returns
		// the redacted display rows. Delete is idempotent and
		// un-audited in slice 1 (design doc §8 enumerates four
		// events; delete is not among them — webhook-driven
		// pr_merged / pr_closed land in slice 1.5).
		//
		// Open-PR is the recommendation-tab integration point: it
		// loads the connection, resolves the placement-map row,
		// appends the snippet (one trailing newline, no parse) to
		// the declared file on a new branch, opens the PR with
		// labels per design doc §7, and emits
		// recommendation.pr_opened / pr_open_failed. Squadron NEVER
		// pushes the default branch — invariant enforced at both
		// wrapper and handler layers.
		//
		// agents:read on Validate (same as AWS validate); agents:write
		// on save / delete / open-pr because those mutate substrate
		// state or write to the operator's GitHub.
		v1.POST("/iac/github/validate",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.iacGitHubTrampoline(func(h *handlers.IaCGitHubHandlers, c *gin.Context) { h.HandleIaCGitHubValidate(c) }))
		v1.POST("/iac/github/connections",
			middleware.RequireScope(services.ScopeAgentsWrite),
			s.iacGitHubTrampoline(func(h *handlers.IaCGitHubHandlers, c *gin.Context) { h.HandleIaCGitHubSaveConnection(c) }))
		v1.GET("/iac/github/connections",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.iacGitHubTrampoline(func(h *handlers.IaCGitHubHandlers, c *gin.Context) { h.HandleListIaCGitHubConnections(c) }))
		v1.DELETE("/iac/github/connections/:id",
			middleware.RequireScope(services.ScopeAgentsWrite),
			s.iacGitHubTrampoline(func(h *handlers.IaCGitHubHandlers, c *gin.Context) { h.HandleDeleteIaCGitHubConnection(c) }))
		// v0.89.4 (#610) — deep-linked-wizard placement-map edit. The
		// /discovery/iac/github page accepts ?connection_id=...&step=
		// placement query params and lets the operator change just the
		// placement map on an existing connection; this route is the
		// save target. agents:write because it mutates substrate state.
		v1.PATCH("/iac/github/connections/:id/placement-map",
			middleware.RequireScope(services.ScopeAgentsWrite),
			s.iacGitHubTrampoline(func(h *handlers.IaCGitHubHandlers, c *gin.Context) { h.HandleIaCGitHubUpdatePlacementMap(c) }))
		// v0.89.28 (#643 slice 1) — PATCH for non-placement-map
		// connection fields. Slice 1 surfaces the discovery proposer's
		// LearnFromAcceptedRecommendations opt-in flag; future slices
		// append additional mutables without breaking the contract.
		v1.PATCH("/iac/github/connections/:id",
			middleware.RequireScope(services.ScopeAgentsWrite),
			s.iacGitHubTrampoline(func(h *handlers.IaCGitHubHandlers, c *gin.Context) { h.HandleIaCGitHubUpdateConnection(c) }))
		v1.POST("/iac/github/connections/:id/open-pr",
			middleware.RequireScope(services.ScopeAgentsWrite),
			s.iacGitHubTrampoline(func(h *handlers.IaCGitHubHandlers, c *gin.Context) { h.HandleIaCGitHubOpenPR(c) }))

		// v0.27 Pricing projection. Turns the v0.24 byte numbers
		// into $/month figures. Read-only; same scope as the rest
		// of the cost-insights surface.
		v1.GET("/pricing/config",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.pricingTrampoline(func(h *handlers.PricingHandlers, c *gin.Context) { h.HandleConfig(c) }))
		v1.GET("/pricing/projection",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.pricingTrampoline(func(h *handlers.PricingHandlers, c *gin.Context) { h.HandleProjection(c) }))
		// v0.39.0 month-end spend forecast. Same projection math
		// pro-rated across the calendar month into elapsed +
		// remaining buckets so the Savings page can render a
		// "projected $X by EOM" tile.
		v1.GET("/pricing/forecast",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.pricingTrampoline(func(h *handlers.PricingHandlers, c *gin.Context) { h.HandleForecast(c) }))

		// v0.42 — actual billing snapshot from the configured
		// destination's billing API (Splunk for v0.42). Reuses the
		// agents-read scope since it's tied to the Savings page,
		// not a new auth surface.
		v1.GET("/billing/snapshot",
			middleware.RequireScope(services.ScopeAgentsRead),
			func(c *gin.Context) {
				handlers.NewBillingHandlers(s.billingProvider, s.logger).HandleSnapshot(c)
			})

		// v0.28 Retrospective savings. Two endpoints: one to record
		// an Apply click (UI fires this when operator clicks the
		// recommendation's Apply button), one to fetch the
		// aggregated realized savings + per-outcome breakdown for
		// the Savings dashboard.
		v1.POST("/recommendations/:id/applied",
			middleware.RequireScope(services.ScopeAgentsWrite),
			func(c *gin.Context) {
				if s.recsEngine == nil || s.recsDismissals == nil || s.pricer == nil {
					c.JSON(http.StatusServiceUnavailable, gin.H{
						"error":   "Retrospective savings tracking is not wired (engine + pricer required)",
						"enabled": false,
					})
					return
				}
				// recsDismissals doubles as the OutcomeStore — both
				// implemented by the application store. We pass the
				// store directly via an interface match.
				store, ok := s.recsDismissals.(handlers.OutcomeStore)
				if !ok {
					c.JSON(http.StatusInternalServerError, gin.H{
						"error": "store does not implement OutcomeStore",
					})
					return
				}
				h := handlers.NewSavingsHandlers(store, s.recsEngine, s.insightsService, s.pricer, s.logger)
				h.HandleApplied(c)
			})
		v1.GET("/savings/realized",
			middleware.RequireScope(services.ScopeAgentsRead),
			func(c *gin.Context) {
				if s.recsEngine == nil || s.recsDismissals == nil || s.pricer == nil {
					c.JSON(http.StatusOK, gin.H{
						"monthly_realized_usd": 0,
						"enabled":              false,
					})
					return
				}
				store, ok := s.recsDismissals.(handlers.OutcomeStore)
				if !ok {
					c.JSON(http.StatusInternalServerError, gin.H{
						"error": "store does not implement OutcomeStore",
					})
					return
				}
				h := handlers.NewSavingsHandlers(store, s.recsEngine, s.insightsService, s.pricer, s.logger)
				h.HandleRealized(c)
			})

		// v0.29 Cost-spike alerting. Detector runs in the
		// background (started in main.go) and writes events to
		// the application store. These routes are pure reads
		// against that store plus an operator-driven Acknowledge.
		// Tick is exposed for tests + the demo path that needs
		// to provoke a detection without waiting the full minute.
		v1.GET("/alerts/cost-spikes",
			middleware.RequireScope(services.ScopeAgentsRead),
			func(c *gin.Context) {
				if s.costSpikes == nil {
					c.JSON(http.StatusOK, gin.H{
						"items": []any{}, "count": 0, "status": "open", "enabled": false,
					})
					return
				}
				h := handlers.NewCostSpikesHandlers(s.costSpikes, s.costSpikeDetector)
				h.HandleList(c)
			})
		v1.POST("/alerts/cost-spikes/:id/acknowledge",
			middleware.RequireScope(services.ScopeAgentsWrite),
			func(c *gin.Context) {
				if s.costSpikes == nil {
					c.JSON(http.StatusServiceUnavailable, gin.H{"error": "cost spikes disabled"})
					return
				}
				h := handlers.NewCostSpikesHandlers(s.costSpikes, s.costSpikeDetector)
				h.HandleAcknowledge(c)
			})
		v1.POST("/alerts/cost-spikes/tick",
			middleware.RequireScope(services.ScopeAgentsWrite),
			func(c *gin.Context) {
				if s.costSpikes == nil || s.costSpikeDetector == nil {
					c.JSON(http.StatusOK, gin.H{"ok": false, "reason": "detector disabled"})
					return
				}
				h := handlers.NewCostSpikesHandlers(s.costSpikes, s.costSpikeDetector)
				h.HandleTick(c)
			})

		// v0.31 Pipeline Health surface — collector self-metrics
		// extracted from the regular OTLP ingest path. All read-only
		// so the natural scope is ScopeAgentsRead. Handlers are
		// constructed inline behind a nil-guard: when no telemetry
		// reader is wired (test_server.go path), the routes 503
		// rather than panicking on the nil service.
		v1.GET("/pipeline-health/fleet",
			middleware.RequireScope(services.ScopeAgentsRead),
			func(c *gin.Context) {
				if s.pipelineHealth == nil {
					c.JSON(http.StatusServiceUnavailable, gin.H{
						"error": "pipeline health unavailable (no telemetry reader)",
					})
					return
				}
				handlers.NewPipelineHealthHandlers(s.pipelineHealth, s.logger).HandleFleetSummary(c)
			})
		v1.GET("/pipeline-health/agents/:agentID",
			middleware.RequireScope(services.ScopeAgentsRead),
			func(c *gin.Context) {
				if s.pipelineHealth == nil {
					c.JSON(http.StatusServiceUnavailable, gin.H{
						"error": "pipeline health unavailable (no telemetry reader)",
					})
					return
				}
				handlers.NewPipelineHealthHandlers(s.pipelineHealth, s.logger).HandleAgentSnapshot(c)
			})
		v1.GET("/pipeline-health/agents/:agentID/timeseries",
			middleware.RequireScope(services.ScopeAgentsRead),
			func(c *gin.Context) {
				if s.pipelineHealth == nil {
					c.JSON(http.StatusServiceUnavailable, gin.H{
						"error": "pipeline health unavailable (no telemetry reader)",
					})
					return
				}
				handlers.NewPipelineHealthHandlers(s.pipelineHealth, s.logger).HandleAgentTimeseries(c)
			})

		// v0.32 Inventory reconciliation — expected vs. actual diff.
		// The list/replace surfaces are designed so a CI/CD pipeline
		// can rotate its target hostlist with a single PUT.
		v1.GET("/inventory/reconciliation",
			middleware.RequireScope(services.ScopeAgentsRead),
			func(c *gin.Context) {
				if s.inventory == nil {
					c.JSON(http.StatusServiceUnavailable, gin.H{"error": "inventory unavailable"})
					return
				}
				handlers.NewInventoryHandlers(s.inventory, s.logger).HandleReconcile(c)
			})
		v1.GET("/inventory/expected",
			middleware.RequireScope(services.ScopeAgentsRead),
			func(c *gin.Context) {
				if s.inventory == nil {
					c.JSON(http.StatusServiceUnavailable, gin.H{"error": "inventory unavailable"})
					return
				}
				handlers.NewInventoryHandlers(s.inventory, s.logger).HandleListExpected(c)
			})
		v1.POST("/inventory/expected",
			middleware.RequireScope(services.ScopeAgentsWrite),
			func(c *gin.Context) {
				if s.inventory == nil {
					c.JSON(http.StatusServiceUnavailable, gin.H{"error": "inventory unavailable"})
					return
				}
				handlers.NewInventoryHandlers(s.inventory, s.logger).HandleUpsertExpected(c)
			})
		v1.PUT("/inventory/expected",
			middleware.RequireScope(services.ScopeAgentsWrite),
			func(c *gin.Context) {
				if s.inventory == nil {
					c.JSON(http.StatusServiceUnavailable, gin.H{"error": "inventory unavailable"})
					return
				}
				handlers.NewInventoryHandlers(s.inventory, s.logger).HandleReplaceExpected(c)
			})
		v1.DELETE("/inventory/expected/:hostname",
			middleware.RequireScope(services.ScopeAgentsWrite),
			func(c *gin.Context) {
				if s.inventory == nil {
					c.JSON(http.StatusServiceUnavailable, gin.H{"error": "inventory unavailable"})
					return
				}
				handlers.NewInventoryHandlers(s.inventory, s.logger).HandleDeleteExpected(c)
			})

		// v0.34 Deploy surface (GitHub Actions integration).
		// All endpoints behind ScopeDeployRead except Trigger +
		// target mutations which need ScopeDeployTrigger.
		deployRead := middleware.RequireScope(services.ScopeDeployRead)
		deployWrite := middleware.RequireScope(services.ScopeDeployTrigger)
		v1.GET("/deploy/targets", deployRead, func(c *gin.Context) {
			handlers.NewDeployHandlers(s.deploy, s.logger).HandleListTargets(c)
		})
		v1.GET("/deploy/targets/:id", deployRead, func(c *gin.Context) {
			handlers.NewDeployHandlers(s.deploy, s.logger).HandleGetTarget(c)
		})
		v1.POST("/deploy/targets", deployWrite, func(c *gin.Context) {
			handlers.NewDeployHandlers(s.deploy, s.logger).HandleCreateTarget(c)
		})
		v1.PUT("/deploy/targets/:id", deployWrite, func(c *gin.Context) {
			handlers.NewDeployHandlers(s.deploy, s.logger).HandleUpdateTarget(c)
		})
		v1.DELETE("/deploy/targets/:id", deployWrite, func(c *gin.Context) {
			handlers.NewDeployHandlers(s.deploy, s.logger).HandleDeleteTarget(c)
		})
		v1.POST("/deploy/targets/:id/lint", deployRead, func(c *gin.Context) {
			handlers.NewDeployHandlers(s.deploy, s.logger).HandleLintConfig(c)
		})
		// v0.34.1: preview the host list parsed from the target's
		// configured inventory.ini. Read-only; the actual auto-population
		// also happens server-side at trigger time.
		v1.GET("/deploy/targets/:id/inventory", deployRead, func(c *gin.Context) {
			handlers.NewDeployHandlers(s.deploy, s.logger).HandleInventoryPreview(c)
		})
		// v0.35.0: pre-flight validation that exercises every read
		// path without firing a workflow. Operator clicks "Validate"
		// to confirm the target is wired correctly before the first
		// real deploy. Idempotent + cheap.
		v1.POST("/deploy/targets/:id/validate", deployRead, func(c *gin.Context) {
			handlers.NewDeployHandlers(s.deploy, s.logger).HandleValidate(c)
		})
		// v0.35.0: redeploy with a past run's inputs. Same lint gate
		// applies — if the pinned config has degraded since the last
		// successful deploy, the redeploy still gets blocked.
		v1.POST("/deploy/runs/:id/redeploy", deployWrite, func(c *gin.Context) {
			handlers.NewDeployHandlers(s.deploy, s.logger).HandleRedeploy(c)
		})
		v1.GET("/deploy/runs", deployRead, func(c *gin.Context) {
			handlers.NewDeployHandlers(s.deploy, s.logger).HandleListRuns(c)
		})
		v1.GET("/deploy/runs/:id", deployRead, func(c *gin.Context) {
			handlers.NewDeployHandlers(s.deploy, s.logger).HandleGetRun(c)
		})
		v1.POST("/deploy/runs", deployWrite, func(c *gin.Context) {
			handlers.NewDeployHandlers(s.deploy, s.logger).HandleTriggerRun(c)
		})

		// v0.46.0 Bulk adoption deploy — fires the configured
		// adoption pipeline with a single adoption_payload input
		// containing per-host snippet blocks. Write scope: this
		// dispatches a workflow.
		v1.POST("/deploy/targets/:id/adopt", deployWrite, func(c *gin.Context) {
			handlers.NewDeployHandlers(s.deploy, s.logger).HandleAdopt(c)
		})

		// v0.39.0 DORA-style deploy metrics. Computed in-process
		// over the deploy_runs ledger — no new schema. Read-only.
		v1.GET("/deploy/metrics", deployRead, func(c *gin.Context) {
			handlers.NewDeployHandlers(s.deploy, s.logger).HandleMetrics(c)
		})

		// v0.50.2 — SIEM destination management. Late-bound via
		// s.siemService so tests + dev runs without
		// SQUADRON_SIEM_KEY get a clean 503 instead of crashing.
		// Read scope returns the destination list (no secrets, ever);
		// write scope is needed for create / update / delete / test.
		siemRead := middleware.RequireScope(services.ScopeSiemRead)
		siemWrite := middleware.RequireScope(services.ScopeSiemWrite)
		siemTrampoline := func(fn func(*handlers.SiemHandlers, *gin.Context)) gin.HandlerFunc {
			return func(c *gin.Context) {
				if s.siemService == nil {
					c.JSON(http.StatusServiceUnavailable, gin.H{"error": "SIEM export disabled (SQUADRON_SIEM_KEY unset)"})
					return
				}
				fn(handlers.NewSiemHandlers(s.siemService, s.logger), c)
			}
		}
		v1.GET("/siem/destinations", siemRead, siemTrampoline(func(h *handlers.SiemHandlers, c *gin.Context) { h.HandleListSiem(c) }))
		v1.GET("/siem/destinations/:id", siemRead, siemTrampoline(func(h *handlers.SiemHandlers, c *gin.Context) { h.HandleGetSiem(c) }))
		v1.POST("/siem/destinations", siemWrite, siemTrampoline(func(h *handlers.SiemHandlers, c *gin.Context) { h.HandleCreateSiem(c) }))
		v1.PUT("/siem/destinations/:id", siemWrite, siemTrampoline(func(h *handlers.SiemHandlers, c *gin.Context) { h.HandleUpdateSiem(c) }))
		v1.DELETE("/siem/destinations/:id", siemWrite, siemTrampoline(func(h *handlers.SiemHandlers, c *gin.Context) { h.HandleDeleteSiem(c) }))
		v1.POST("/siem/destinations/:id/test", siemWrite, siemTrampoline(func(h *handlers.SiemHandlers, c *gin.Context) { h.HandleTestSiem(c) }))

		// v0.53 — action runner endpoints (Move 2). Read scope
		// covers list/get + the runner-side pending poll; write
		// scope covers register, dispatch, revoke, and result
		// reporting. The runner daemon authenticates with a token
		// carrying actions:write issued at enrollment.
		actionsRead := middleware.RequireScope(services.ScopeActionsRead)
		actionsWrite := middleware.RequireScope(services.ScopeActionsWrite)
		// v0.89.212 — build the actions handler PER REQUEST so it reads
		// s.appStore + s.actionSigner at request time. registerRoutes()
		// runs inside NewServer BEFORE main.go calls
		// SetActionStoreAndSigner, so an eagerly-built handler captured a
		// nil store and every /actions + /runners route panicked (nil
		// deref) -> 500. Same fix as the incidents routes (v0.89.211).
		actions := func(fn func(*handlers.ActionsHandlers, *gin.Context)) gin.HandlerFunc {
			return func(c *gin.Context) {
				fn(handlers.NewActionsHandlers(s.appStore, s.actionSigner, nil, s.auditService, s.logger), c)
			}
		}
		v1.POST("/runners/register", actionsWrite, actions(func(h *handlers.ActionsHandlers, c *gin.Context) { h.HandleRegisterRunner(c) }))
		v1.GET("/runners", actionsRead, actions(func(h *handlers.ActionsHandlers, c *gin.Context) { h.HandleListRunners(c) }))
		v1.GET("/runners/:id", actionsRead, actions(func(h *handlers.ActionsHandlers, c *gin.Context) { h.HandleGetRunner(c) }))
		v1.POST("/runners/:id/revoke", actionsWrite, actions(func(h *handlers.ActionsHandlers, c *gin.Context) { h.HandleRevokeRunner(c) }))
		v1.GET("/runners/:id/pending", actionsRead, actions(func(h *handlers.ActionsHandlers, c *gin.Context) { h.HandleRunnerPending(c) }))
		v1.POST("/actions/dispatch", actionsWrite, actions(func(h *handlers.ActionsHandlers, c *gin.Context) { h.HandleDispatchAction(c) }))
		v1.GET("/actions", actionsRead, actions(func(h *handlers.ActionsHandlers, c *gin.Context) { h.HandleListActions(c) }))
		v1.GET("/actions/:id", actionsRead, actions(func(h *handlers.ActionsHandlers, c *gin.Context) { h.HandleGetAction(c) }))
		v1.POST("/actions/:id/result", actionsWrite, actions(func(h *handlers.ActionsHandlers, c *gin.Context) { h.HandlePostActionResult(c) }))

		// v0.54 — incident drafter (Move 3). Read covers the
		// operator inbox view; write covers edit, dismiss, and
		// publish. Publish is a stamping operation in the MVP
		// (clipboard provider); real provider plug ins land in a
		// follow-up chunk.
		incidentsRead := middleware.RequireScope(services.ScopeIncidentsRead)
		incidentsWrite := middleware.RequireScope(services.ScopeIncidentsWrite)
		// v0.89.211 — build the incidents handler PER REQUEST so it reads
		// s.appStore at request time. registerRoutes() runs inside
		// NewServer (above) BEFORE main.go calls SetActionStoreAndSigner,
		// so capturing s.appStore eagerly here bound a nil store and every
		// incidents route panicked (nil deref) -> 500. The discovery routes
		// already read their stores lazily via trampolines; this matches.
		incidents := func(fn func(*handlers.IncidentsHandlers, *gin.Context)) gin.HandlerFunc {
			return func(c *gin.Context) {
				fn(handlers.NewIncidentsHandlers(s.appStore, s.auditService, s.incidentsPublishers, s.logger), c)
			}
		}
		v1.GET("/incidents/drafts", incidentsRead, incidents(func(h *handlers.IncidentsHandlers, c *gin.Context) { h.HandleListDrafts(c) }))
		v1.GET("/incidents/drafts/:id", incidentsRead, incidents(func(h *handlers.IncidentsHandlers, c *gin.Context) { h.HandleGetDraft(c) }))
		v1.PATCH("/incidents/drafts/:id", incidentsWrite, incidents(func(h *handlers.IncidentsHandlers, c *gin.Context) { h.HandlePatchDraft(c) }))
		v1.POST("/incidents/drafts/:id/dismiss", incidentsWrite, incidents(func(h *handlers.IncidentsHandlers, c *gin.Context) { h.HandleDismissDraft(c) }))
		v1.POST("/incidents/drafts/:id/publish", incidentsWrite, incidents(func(h *handlers.IncidentsHandlers, c *gin.Context) { h.HandlePublishDraft(c) }))

		// v0.27.1 Quickstart. Pure config-generation; no state.
		// All read-only so ScopeAgentsRead is the natural gate.
		// Handler is constructed inline since it's cheap and the
		// late-bind dance isn't needed (port is always available
		// once SetOpAMPPort runs, which happens in NewServer
		// callers before Start).
		v1.GET("/quickstart/backends",
			middleware.RequireScope(services.ScopeAgentsRead),
			func(c *gin.Context) {
				port := s.opampPort
				if port == 0 {
					port = 4320
				}
				handlers.NewQuickstartHandlers(port, s.logger).HandleCatalog(c)
			})
		v1.GET("/quickstart/starter-config",
			middleware.RequireScope(services.ScopeAgentsRead),
			func(c *gin.Context) {
				port := s.opampPort
				if port == 0 {
					port = 4320
				}
				handlers.NewQuickstartHandlers(port, s.logger).HandleStarterConfig(c)
			})
		v1.GET("/quickstart/opamp-snippet",
			middleware.RequireScope(services.ScopeAgentsRead),
			func(c *gin.Context) {
				port := s.opampPort
				if port == 0 {
					port = 4320
				}
				handlers.NewQuickstartHandlers(port, s.logger).HandleOpAMPSnippet(c)
			})
		// v0.45 — per-host adoption snippet. Same shape as the
		// /opamp-snippet endpoint but accepts hostname + repeatable
		// label query params, used by the Inventory page to give
		// operators a paste-into-existing-config snippet for each
		// missing host.
		v1.GET("/quickstart/adoption-snippet",
			middleware.RequireScope(services.ScopeAgentsRead),
			func(c *gin.Context) {
				port := s.opampPort
				if port == 0 {
					port = 4320
				}
				handlers.NewQuickstartHandlers(port, s.logger).HandleAdoptionSnippet(c)
			})
	}

	// Serve static files for the UI
	s.router.Static("/assets", "./ui/dist/assets")

	// SPA catch-all route - must be last
	s.router.NoRoute(func(c *gin.Context) {
		// Check if file exists
		filePath := filepath.Join("./ui/dist", c.Request.URL.Path)
		if _, err := os.Stat(filePath); err == nil {
			c.File(filePath)
			return
		}

		// Serve index.html for all other routes (SPA routing)
		c.File("./ui/dist/index.html")
	})
}

// corsMiddleware adds CORS headers
func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}

		c.Next()
	}
}

// loggingMiddleware adds request logging with reduced verbosity
func loggingMiddleware(logger *zap.Logger) gin.HandlerFunc {
	return gin.LoggerWithFormatter(func(param gin.LogFormatterParams) string {
		// Skip logging for health checks and other frequent, low-value requests
		if param.Path == "/health" || param.Path == "/ready" {
			return ""
		}

		// Log errors at INFO level
		if param.StatusCode >= 400 {
			logger.Info("HTTP Request Error",
				zap.String("method", param.Method),
				zap.String("path", param.Path),
				zap.Int("status", param.StatusCode),
				zap.Duration("latency", param.Latency),
				zap.String("client_ip", param.ClientIP),
			)
			return ""
		}

		// Log all other requests at DEBUG level to reduce noise
		logger.Debug("HTTP Request",
			zap.String("method", param.Method),
			zap.String("path", param.Path),
			zap.Int("status", param.StatusCode),
			zap.Duration("latency", param.Latency),
			zap.String("client_ip", param.ClientIP),
		)
		return ""
	})
}

// metricsMiddleware tracks request metrics
func (s *Server) metricsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()

		// Process request
		c.Next()

		// Track metrics
		duration := time.Since(start)
		s.metrics.RequestCount.Inc(1)
		s.metrics.RequestDuration.Record(duration)

		// Track errors
		if c.Writer.Status() >= 400 {
			s.metrics.RequestErrors.Inc(1)
		}

		// Track specific endpoint metrics
		path := c.FullPath()
		switch {
		case path == "/health":
			s.metrics.HealthCheckCount.Inc(1)
		case path == "/api/v1/agents/:id":
			s.metrics.AgentGetCount.Inc(1)
		case path == "/api/v1/agents":
			s.metrics.AgentListCount.Inc(1)
		case path == "/api/v1/groups/:id":
			s.metrics.GroupGetCount.Inc(1)
		case path == "/api/v1/groups":
			if c.Request.Method == "GET" {
				s.metrics.GroupListCount.Inc(1)
			} else if c.Request.Method == "POST" {
				s.metrics.GroupCreateCount.Inc(1)
			}
		case path == "/api/v1/configs/:id":
			s.metrics.ConfigGetCount.Inc(1)
		case path == "/api/v1/configs":
			if c.Request.Method == "GET" {
				s.metrics.ConfigListCount.Inc(1)
			} else if c.Request.Method == "POST" {
				s.metrics.ConfigCreateCount.Inc(1)
			}
		case path == "/api/v1/telemetry/metrics/query":
			s.metrics.TelemetryQueryCount.Inc(1)
			s.metrics.TelemetryQueryDuration.Record(duration)
		case path == "/api/v1/topology":
			s.metrics.TopologyQueryCount.Inc(1)
			s.metrics.TopologyQueryDuration.Record(duration)
		}
	}
}
