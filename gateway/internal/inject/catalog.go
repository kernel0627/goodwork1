package inject

import "fmt"

// Class groups faults by the shape of the symptom they produce. The agent's
// hardest cases are the ones where class and symptom disagree -- a cache
// failure that shows up as latency rather than errors, for instance.
type Class string

const (
	ClassError        Class = "error"
	ClassLatency      Class = "latency"
	ClassResource     Class = "resource"
	ClassConnectivity Class = "connectivity"
	ClassHealth       Class = "health"
	ClassQueue        Class = "queue"
	ClassCache        Class = "cache"
)

// Difficulty is how far the symptom sits from the root cause.
//
//	L1  symptom service == root cause service, visible directly in metrics
//	L2  symptom appears in the caller; one hop down the dependency chain
//	L3  symptom shape disagrees with the fault class, or crosses an async
//	    boundary -- needs a hypothesis to be formed and tested
type Difficulty string

const (
	L1 Difficulty = "L1"
	L2 Difficulty = "L2"
	L3 Difficulty = "L3"
)

// Spec describes one injectable fault and how to tell whether it actually fired.
//
// The verification queries exist because a fault that fails to take effect
// produces a scenario that looks injected but is not. Such a scenario silently
// scores the agent on a healthy system, which corrupts results with no visible
// failure anywhere.
type Spec struct {
	Flag    string
	Variant string // severity used for characterisation: the most extreme one

	Class      Class
	RootCause  string // service believed to be at fault
	SymptomAt  string // service where the symptom is expected to surface
	Difficulty Difficulty

	// Effect is PromQL whose value should move once the fault fires.
	Effect string
	// EffectRises says which way. All current faults make things worse, but
	// stating it explicitly keeps the verifier honest about direction.
	EffectRises bool

	// MinDeltaOverride replaces the per-class threshold when a fault's ceiling
	// is known to sit below it. cartFailure is the motivating case: it only
	// affects EmptyCart, which the load generator calls about once every 30
	// seconds, so the largest error rate it can possibly produce is ~0.03/s --
	// under the 0.05/s class floor. With the class default this fault reads as
	// inert no matter how hard it is actually failing.
	MinDeltaOverride float64

	// Site is where the flag is read in the SUT's source. Recorded because the
	// blast radius came out of reading that code, and the next person to
	// question a verdict should start there rather than from the flag's name.
	Site string

	// SyntheticLoadReaches is false when the load generator cannot drive the
	// code path at all. Such faults are excluded from the scenario library
	// regardless of verification, since no traffic would ever trigger them.
	SyntheticLoadReaches bool

	Note string
}

// Catalog is the 13 fault-injecting flags in demo.flagd.json.
//
// The two loadGenerator* flags are excluded on purpose: they move the metric
// baseline for every service at once, which makes them experiment variables to
// be held fixed across control arms, not faults to be injected.
//
// # Every entry here was derived by reading the SUT's source
//
// The first version of this catalog was written from the flags' names and prose
// descriptions, and roughly half of it was wrong. `cartFailure` is not "cart
// fails" but "EmptyCart fails". `imageSlowLoad` does not slow the image
// service; it makes the browser ask Envoy to delay. `recommendationCacheFailure`
// leaks memory rather than adding latency. `failedReadinessProbe` flips a health
// check and never touches a request.
//
// A fault's blast radius and observable signature are properties of the code
// that reads the flag. They cannot be inferred from its name. Each Spec below
// therefore records the Site it was read from, so a disputed verdict is checked
// against source rather than against intuition.
func Catalog() []Spec {
	return []Spec{
		{
			Flag: "adFailure", Variant: "on",
			Class: ClassError, RootCause: "ad", SymptomAt: "ad", Difficulty: L2,
			Site:                 "ad/src/main/java/oteldemo/AdService.java:164 (getAds)",
			Effect:               errorRate("ad"),
			EffectRises:          true,
			SyntheticLoadReaches: true,
			Note:                 "getAds is on the hot path for every product page, so this is the strongest error signal available",
		},
		{
			Flag: "adHighCpu", Variant: "on",
			Class: ClassResource, RootCause: "ad", SymptomAt: "ad", Difficulty: L2,
			Site:                 "ad/src/main/java/oteldemo/AdService.java:166 (getAds)",
			Effect:               containerCPU("ad"),
			EffectRises:          true,
			SyntheticLoadReaches: true,
		},
		{
			Flag: "adManualGc", Variant: "on",
			Class: ClassResource, RootCause: "ad", SymptomAt: "ad", Difficulty: L3,
			Site:                 "ad/src/main/java/oteldemo/AdService.java:165 (getAds)",
			Effect:               latencyP99("ad"),
			EffectRises:          true,
			SyntheticLoadReaches: true,
			Note:                 "forced full GCs surface as periodic latency spikes; a p99 over a 2m window may smooth them away",
		},
		{
			Flag: "cartFailure", Variant: "100%",
			Class: ClassError, RootCause: "cart", SymptomAt: "cart", Difficulty: L2,
			Site:                 "cart/src/services/CartService.cs:82 (EmptyCart only)",
			Effect:               errorRate("cart"),
			EffectRises:          true,
			MinDeltaOverride:     0.01,
			SyntheticLoadReaches: true,
			Note: "applies to EmptyCart alone, which the load generator calls at ~0.03/s -- " +
				"measured GetCart 0.67/s, AddItem 0.22/s, EmptyCart 0.03/s. The fault's " +
				"ceiling is therefore below the class threshold, hence the override. " +
				"Also worth knowing: cart routes the request to a deliberately broken " +
				"store rather than throwing, so the failure may not appear as a status code at all",
		},
		{
			Flag: "emailMemoryLeak", Variant: "10000x",
			Class: ClassResource, RootCause: "email", SymptomAt: "email", Difficulty: L3,
			Site:                 "email/email_server.rb:67",
			Effect:               containerMemory("email"),
			EffectRises:          true,
			SyntheticLoadReaches: true,
			Note: "email has a container memory limit, so a large enough leak should reach OOM " +
				"rather than merely trend upward. email is only called during checkout, " +
				"so the leak accrues at checkout rate",
		},
		{
			Flag: "failedReadinessProbe", Variant: "on",
			Class: ClassHealth, RootCause: "cart", SymptomAt: "cart", Difficulty: L1,
			Site:                 "cart/src/services/HealthCheckService.cs:36 (readinessCheck)",
			Effect:               errorRate("cart"),
			EffectRises:          true,
			SyntheticLoadReaches: false,
			Note: "returns HealthCheckResult.Unhealthy and never touches a request path, so " +
				"request metrics are the wrong place to look. The real signal is the gRPC " +
				"health endpoint and the container's health state, neither of which reaches " +
				"Prometheus. Needs a container-level probe; the Effect query here is a " +
				"placeholder and is expected to read inert",
		},
		{
			Flag: "imageSlowLoad", Variant: "10sec",
			Class: ClassLatency, RootCause: "frontend-proxy", SymptomAt: "frontend-proxy", Difficulty: L3,
			Site:                 "frontend/components/ProductCard/ProductCard.tsx:32",
			Effect:               latencyP99("frontend-proxy"),
			EffectRises:          true,
			SyntheticLoadReaches: false,
			Note: "not an image-provider fault. The browser component reads the flag and sets " +
				"an x-envoy-fault-delay-request header; Envoy applies the delay. The header " +
				"only exists if a browser renders ProductCard, and the k6 load generator " +
				"issues HTTP requests without rendering, so synthetic traffic probably never " +
				"triggers it. Expected to be unusable without a browser-driving load source",
		},
		{
			Flag: "intlShippingSlowdown", Variant: "10sec",
			Class: ClassLatency, RootCause: "shipping", SymptomAt: "shipping", Difficulty: L3,
			Site:                 "shipping/src/shipping_service.rs:67",
			Effect:               latencyP99("shipping"),
			EffectRises:          true,
			SyntheticLoadReaches: true,
			Note:                 "fires only for international orders, so the effect depends on the load generator producing them",
		},
		{
			Flag: "kafkaQueueProblems", Variant: "on",
			Class: ClassQueue, RootCause: "checkout", SymptomAt: "fraud-detection", Difficulty: L3,
			Site:                 "checkout/main.go:707",
			Effect:               "sum(kafka_consumer_records_lag) or vector(0)",
			EffectRises:          true,
			SyntheticLoadReaches: true,
			Note: "checkout is the producer that overloads the queue, so the root cause is " +
				"checkout rather than kafka. Crosses an async boundary: the symptom is " +
				"consumer lag on fraud-detection and accounting, not a failing request",
		},
		{
			Flag: "paymentFailure", Variant: "100%",
			Class: ClassError, RootCause: "payment", SymptomAt: "payment", Difficulty: L2,
			Site:                 "payment/charge.js:39 (throws on charge)",
			Effect:               errorRate("payment"),
			EffectRises:          true,
			SyntheticLoadReaches: true,
			Note:                 "charge runs once per checkout, so the achievable rate is the checkout rate, not the request rate",
		},
		{
			Flag: "paymentUnreachable", Variant: "on",
			Class: ClassConnectivity, RootCause: "payment", SymptomAt: "checkout", Difficulty: L2,
			Site:                 "checkout/flags/flags_gen.go:51 (read by checkout, not payment)",
			Effect:               errorRate("checkout"),
			EffectRises:          true,
			SyntheticLoadReaches: true,
			Note: "checkout reads the flag and refuses to reach payment, so the symptom is on " +
				"the caller while the named service is the one that looks absent -- a clean " +
				"case of symptom service != root cause service",
		},
		{
			Flag: "productCatalogFailure", Variant: "on",
			Class: ClassError, RootCause: "product-catalog", SymptomAt: "product-catalog", Difficulty: L2,
			Site:                 "product-catalog/flags/flags_gen.go:29",
			Effect:               errorRate("product-catalog"),
			EffectRises:          true,
			MinDeltaOverride:     0.01,
			SyntheticLoadReaches: true,
			Note: "the targeting rule scopes this to product OLJCESPC7Z, so only requests for " +
				"that one product fail and the error rate stays low by design -- a weak " +
				"signal on purpose, which makes it a good test of whether the agent notices",
		},
		{
			Flag: "recommendationCacheFailure", Variant: "on",
			Class: ClassResource, RootCause: "recommendation", SymptomAt: "recommendation", Difficulty: L3,
			Site:                 "recommendation/recommendation_server.py:78",
			Effect:               containerMemory("recommendation"),
			EffectRises:          true,
			SyntheticLoadReaches: true,
			Note: "despite the name this is a memory leak, not a cache miss problem: on each " +
				"miss the code appends a quarter of cached_ids back onto itself, so the list " +
				"grows without bound. Class is resource, not cache. A secondary signal is " +
				"an elevated ListProducts call rate against product-catalog",
		},
	}
}

// EvaluationQuery is the tier-1 check: does any service actually evaluate this
// flag?
//
// This is a static property, not a before/after delta. The load generator keeps
// traffic flowing, so services consult their flags continuously whether or not
// a fault is injected; the counter is already climbing before injection. What
// the counter tells us is whether anything reads the flag at all. A flag with
// no evaluations cannot possibly fire, so a scenario built on it is invalid
// regardless of what downstream metrics happen to show.
func EvaluationQuery(flagName string) string {
	return fmt.Sprintf(
		`sum(rate(feature_flag_evaluation_requests_total{feature_flag_key=%q}[5m])) or vector(0)`,
		flagName)
}

// EvaluatingServicesQuery finds which services evaluate a flag, so the
// RootCause guesses above can be checked against measurement instead of
// against the flag's prose description.
func EvaluatingServicesQuery(flagName string) string {
	return fmt.Sprintf(
		`feature_flag_evaluation_requests_total{feature_flag_key=%q}`, flagName)
}

// errorRate sums server-side failures for a service.
//
// Three terms are needed because the demo is polyglot: gRPC services report
// rpc_grpc_status_code, while HTTP services split across two semantic
// conventions -- older SDKs emit http_status_code on
// http_server_duration_milliseconds, newer ones emit http_response_status_code
// on http_server_request_duration_seconds. Covering only one would make some
// services look permanently healthy.
//
// `or vector(0)` supplies a zero for terms with no series, since sum() over an
// empty vector yields an empty vector and would otherwise void the whole
// expression.
func errorRate(service string) string {
	return fmt.Sprintf(`
(sum(rate(rpc_server_duration_milliseconds_count{service_name=%[1]q,rpc_grpc_status_code!="0"}[2m])) or vector(0))
+ (sum(rate(http_server_duration_milliseconds_count{service_name=%[1]q,http_status_code=~"5.."}[2m])) or vector(0))
+ (sum(rate(http_server_request_duration_seconds_count{service_name=%[1]q,http_response_status_code=~"5.."}[2m])) or vector(0))`,
		service)
}

// latencyP99 is the 99th percentile server latency in milliseconds.
//
// Three histogram families are covered for the same polyglot reason as
// errorRate, and the seconds-based one is scaled up so all three are in the
// same unit. `or` picks whichever family the service actually reports.
//
// The single `or vector(0)` sits at the very end, and that placement is
// load bearing. Writing `(A or vector(0)) or B` would short-circuit: the first
// term always yields something -- A when it exists, otherwise 0 -- so B would
// never be consulted, and every service that reports only the later families
// would measure a flat zero and make a working latency fault look inert.
func latencyP99(service string) string {
	return fmt.Sprintf(`
max(
  histogram_quantile(0.99, sum by (le) (rate(rpc_server_duration_milliseconds_bucket{service_name=%[1]q}[2m])))
  or
  histogram_quantile(0.99, sum by (le) (rate(http_server_duration_milliseconds_bucket{service_name=%[1]q}[2m])))
  or
  1000 * histogram_quantile(0.99, sum by (le) (rate(http_server_request_duration_seconds_bucket{service_name=%[1]q}[2m])))
) or vector(0)`, service)
}

// containerCPU and containerMemory key off container_name, which is the label
// the container metrics carry -- they have no service_name.
func containerCPU(container string) string {
	return fmt.Sprintf(`max(container_cpu_utilization_ratio{container_name=%q}) or vector(0)`, container)
}

func containerMemory(container string) string {
	return fmt.Sprintf(`max(container_memory_percent_ratio{container_name=%q}) or vector(0)`, container)
}
