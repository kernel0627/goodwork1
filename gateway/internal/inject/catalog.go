package inject

// Class groups faults by the shape of the symptom they produce. The agent's
// hardest cases are the ones where class and symptom disagree -- a leak named
// after a cache, for instance.
type Class string

const (
	ClassError        Class = "error"
	ClassLatency      Class = "latency"
	ClassResource     Class = "resource"
	ClassConnectivity Class = "connectivity"
	ClassHealth       Class = "health"
	ClassQueue        Class = "queue"
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
// The verification signal exists because a fault that fails to take effect
// produces a scenario that looks injected but is not. Such a scenario silently
// scores the agent against a healthy system, which corrupts results with no
// visible failure anywhere.
type Spec struct {
	Flag    string
	Variant string // severity used for characterisation: the most extreme one

	Class      Class
	RootCause  string // service believed to be at fault
	SymptomAt  string // service where the symptom is expected to surface
	Difficulty Difficulty

	// Signal names an entry in queries/signals.yaml, and Subs supplies its
	// placeholders. Queries are not written inline here: the Python tool layer
	// needs the same ones, and duplicating them means duplicating the mistakes
	// (see the file's header for what those cost).
	Signal string
	Subs   map[string]string

	// SignalRises says which way the value should move. Every fault here makes
	// things worse, but stating it keeps the verifier honest about direction.
	SignalRises bool

	// MinDeltaOverride replaces the signal's own threshold. Rarely needed now
	// that faults are measured as ratios rather than absolute rates.
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
// that reads the flag. They cannot be inferred from its name. Each Spec records
// the Site it was read from, so a disputed verdict is checked against source
// rather than against intuition.
func Catalog() []Spec {
	return []Spec{
		{
			Flag: "adFailure", Variant: "on",
			Class: ClassError, RootCause: "ad", SymptomAt: "ad", Difficulty: L2,
			Site:                 "ad/src/main/java/oteldemo/AdService.java:164 (getAds)",
			Signal:               "error_ratio",
			Subs:                 map[string]string{"service": "ad"},
			SignalRises:          true,
			SyntheticLoadReaches: true,
			Note: "getAds is on the hot path for every product page. Measured as an absolute " +
				"rate this fault moved the needle only 0.0167/s and was dismissed as inert, " +
				"because ad serves just 0.23 req/s; as a ratio the same failure is ~7%",
		},
		{
			Flag: "adHighCpu", Variant: "on",
			Class: ClassResource, RootCause: "ad", SymptomAt: "ad", Difficulty: L2,
			Site:                 "ad/src/main/java/oteldemo/AdService.java:166 (getAds)",
			Signal:               "cpu",
			Subs:                 map[string]string{"container": "ad"},
			SignalRises:          true,
			SyntheticLoadReaches: true,
			Note:                 "ad idles around 1.6 cores, so the threshold is in cores rather than a fraction",
		},
		{
			Flag: "adManualGc", Variant: "on",
			Class: ClassResource, RootCause: "ad", SymptomAt: "ad", Difficulty: L3,
			Site:                 "ad/src/main/java/oteldemo/AdService.java:165 (getAds)",
			Signal:               "latency_p99",
			Subs:                 map[string]string{"service": "ad"},
			SignalRises:          true,
			SyntheticLoadReaches: true,
			Note:                 "forced full GCs surface as periodic latency spikes; a p99 over a 2m window may smooth them away",
		},
		{
			Flag: "cartFailure", Variant: "100%",
			Class: ClassError, RootCause: "cart", SymptomAt: "cart", Difficulty: L2,
			Site:                 "cart/src/services/CartService.cs:82 (EmptyCart only)",
			Signal:               "error_ratio",
			Subs:                 map[string]string{"service": "cart"},
			SignalRises:          true,
			SyntheticLoadReaches: true,
			Note: "applies to EmptyCart alone. Measured endpoint rates: GetCart 0.67/s, " +
				"AddItem 0.22/s, EmptyCart 0.03/s -- so at 100% the service-wide error " +
				"ratio should reach roughly 3%. Expected to fail anyway: cart instruments " +
				"gRPC as HTTP and reports status 200 even on failure, since the gRPC status " +
				"travels in a trailer. Error ratio is structurally blind on cart",
		},
		{
			Flag: "emailMemoryLeak", Variant: "10000x",
			Class: ClassResource, RootCause: "email", SymptomAt: "email", Difficulty: L3,
			Site:                 "email/email_server.rb:67",
			Signal:               "memory",
			Subs:                 map[string]string{"container": "email"},
			SignalRises:          true,
			SyntheticLoadReaches: true,
			Note: "email reports no server-side request metrics at all (measured 0.000 req/s), " +
				"so container memory is the only available signal. It has a memory limit, so " +
				"a large enough leak should reach OOM rather than merely trend upward",
		},
		{
			Flag: "failedReadinessProbe", Variant: "on",
			Class: ClassHealth, RootCause: "cart", SymptomAt: "cart", Difficulty: L1,
			Site:                 "cart/src/services/HealthCheckService.cs:36 (readinessCheck)",
			Signal:               "error_ratio",
			Subs:                 map[string]string{"service": "cart"},
			SignalRises:          true,
			SyntheticLoadReaches: false,
			Note: "returns HealthCheckResult.Unhealthy and never touches a request path, so " +
				"request metrics are the wrong place to look. The real signal is the gRPC " +
				"health endpoint and the container's health state, neither of which reaches " +
				"Prometheus. This Signal is a placeholder and is expected to read inert",
		},
		{
			Flag: "imageSlowLoad", Variant: "10sec",
			Class: ClassLatency, RootCause: "frontend-proxy", SymptomAt: "frontend-proxy", Difficulty: L3,
			Site:                 "frontend/components/ProductCard/ProductCard.tsx:32",
			Signal:               "latency_p99",
			Subs:                 map[string]string{"service": "frontend-proxy"},
			SignalRises:          true,
			SyntheticLoadReaches: false,
			Note: "not an image-provider fault. The browser component reads the flag and sets " +
				"an x-envoy-fault-delay-request header; Envoy applies the delay. The header " +
				"only exists if a browser renders ProductCard, and the k6 load generator " +
				"issues HTTP requests without rendering, so synthetic traffic probably never " +
				"triggers it",
		},
		{
			Flag: "intlShippingSlowdown", Variant: "10sec",
			Class: ClassLatency, RootCause: "shipping", SymptomAt: "shipping", Difficulty: L3,
			Site:                 "shipping/src/shipping_service.rs:67",
			Signal:               "latency_p99",
			Subs:                 map[string]string{"service": "shipping"},
			SignalRises:          true,
			SyntheticLoadReaches: true,
			Note:                 "fires only for international orders, so the effect depends on the load generator producing them",
		},
		{
			Flag: "kafkaQueueProblems", Variant: "on",
			Class: ClassQueue, RootCause: "checkout", SymptomAt: "fraud-detection", Difficulty: L3,
			Site:                 "checkout/main.go:707",
			Signal:               "consumer_lag",
			SignalRises:          true,
			SyntheticLoadReaches: true,
			Note: "checkout is the producer that overloads the queue, so the root cause is " +
				"checkout rather than kafka. Crosses an async boundary: the symptom is " +
				"consumer lag on fraud-detection and accounting, not a failing request",
		},
		{
			Flag: "paymentFailure", Variant: "100%",
			Class: ClassError, RootCause: "payment", SymptomAt: "checkout", Difficulty: L2,
			Site:   "payment/charge.js:39 (throws on charge)",
			Signal: "client_error_ratio",
			Subs: map[string]string{
				"caller":      "checkout",
				"rpc_pattern": "oteldemo.PaymentService/.*",
			},
			SignalRises:          true,
			SyntheticLoadReaches: true,
			Note: "payment reports no server-side metrics whatsoever (measured 0.000 req/s), so " +
				"its own failure is invisible from its own instrumentation. Measured from " +
				"the caller instead: checkout calls oteldemo.PaymentService/Charge at " +
				"0.062/s. This is also how an oncall would establish that payment is down",
		},
		{
			Flag: "paymentUnreachable", Variant: "on",
			Class: ClassConnectivity, RootCause: "payment", SymptomAt: "checkout", Difficulty: L2,
			Site:                 "checkout/flags/flags_gen.go:51 (read by checkout, not payment)",
			Signal:               "error_ratio",
			Subs:                 map[string]string{"service": "checkout"},
			SignalRises:          true,
			SyntheticLoadReaches: true,
			Note: "checkout reads the flag and refuses to reach payment, so the symptom is on " +
				"the caller while the named service merely looks absent -- a clean case of " +
				"symptom service != root cause service. Measured on checkout's own error " +
				"ratio rather than its client view, since the call may not be attempted at all",
		},
		{
			Flag: "productCatalogFailure", Variant: "on",
			Class: ClassError, RootCause: "product-catalog", SymptomAt: "product-catalog", Difficulty: L2,
			Site:                 "product-catalog/flags/flags_gen.go:29",
			Signal:               "error_ratio",
			Subs:                 map[string]string{"service": "product-catalog"},
			SignalRises:          true,
			SyntheticLoadReaches: true,
			Note: "the targeting rule scopes this to product OLJCESPC7Z, so only requests for " +
				"that one product fail. product-catalog serves 3.09 req/s, the busiest " +
				"backend, so the ratio stays low by design -- which makes it a good test of " +
				"whether the agent notices a weak signal",
		},
		{
			Flag: "recommendationCacheFailure", Variant: "on",
			Class: ClassResource, RootCause: "recommendation", SymptomAt: "recommendation", Difficulty: L3,
			Site:                 "recommendation/recommendation_server.py:78",
			Signal:               "memory",
			Subs:                 map[string]string{"container": "recommendation"},
			SignalRises:          true,
			SyntheticLoadReaches: true,
			Note: "despite the name this is a memory leak, not a cache miss problem: on each " +
				"miss the code appends a quarter of cached_ids back onto itself, so the list " +
				"grows without bound. Class is resource, not cache. recommendation also " +
				"reports no server-side request metrics, so memory is the only signal",
		},
	}
}
