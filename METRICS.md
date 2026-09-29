# Metrics Design

## Metrics

| Metric | Type | Labels (bounded) | Where |
|---|---|---|---|
| `orders_created_total` | Counter | `status` = success\|failure\|pending, `customer_tier` = free\|premium\|unknown | `MetricsInteractor` (usecase decorator). `pending` = charge outcome unknown, order left in `payment_pending` |
| `order_processing_duration_seconds` | Histogram | `step` = validation\|payment\|fulfillment | `observability.Probe` (usecase step port) |
| `orders_pending_count` | Gauge | — | `MetricsInteractor`: orders currently in processing |
| `payment_requests_total` | Counter | `outcome` = success\|declined\|rejected\|timeout\|error | `MetricsPaymentGateway` (decorator around the `PaymentGateway` port) |

Plus Go runtime and process collectors. Everything is exposed at `GET /metrics` in OpenMetrics format, which is required for exemplars.

Implementation: `observability/metrics.go`, `observability/probe.go`. The E2E test (`testing/e2e`) scrapes `/metrics` over HTTP and asserts these series after real requests.

## 1. Where do you instrument?

| Layer | What | Why here |
|---|---|---|
| HTTP boundary | Request rate, errors, latency (RED) by route and status class | The service as the caller sees it. *Not implemented here; would be an `otelhttp`/`promhttp` middleware with the route pattern as a label.* |
| **Usecase (decorator)** | `orders_created_total`, `orders_pending_count` | This is where the **business outcome** is known. The usecase itself stays free of Prometheus. |
| **Usecase steps (Probe port)** | `order_processing_duration_seconds{step}` | Only the usecase knows where steps begin and end; the port keeps Prometheus out of it. |
| Repository / clients | DB pool, query latency, HTTP client latency | Generic infrastructure metrics from instrumented drivers and clients. A repository cannot know whether an order "succeeded". |
| Domain | Nothing | Pure. It reports facts through domain events. |

Rule of thumb: **business metrics at the usecase boundary, technical metrics in the infrastructure, nothing in the domain.**

About `orders_pending_count`: the decorator tracks orders *in processing* (Inc on entry, Dec on exit). A gauge for "orders stuck in `pending` in the database", needed for the alert in ANSWERS.md Q3, would be a `GaugeFunc` over `SELECT count(*) … WHERE status='pending' AND created_at < now() - interval '5 minutes'`, or a periodic job.

## 2. Why does this cause a metric explosion, and how do you fix it?

```go
orderDuration.WithLabelValues(customerID, productID, orderID).Observe(duration)
```

**Why.** Prometheus creates a separate time series for every unique combination of label values. Each series costs memory in the head block, index entries and disk.

- `orderID` is unique per order. **Every order creates a new series**, which receives one sample and is never used again. The number of series grows with traffic, with no upper bound.
- `customerID × productID` multiplies on top of that: 100k customers × 10k products is up to 10⁹ combinations.
- It is a **histogram**: each series is actually `buckets + 2` series (`_bucket` × 12 + `_sum` + `_count`), roughly 14× more.

Result: Prometheus memory grows until OOM, queries slow down, and the remote-storage bill grows. The data is also useless: nobody asks for a p99 of a single order.

**Fix.**

1. **Only labels with a small, fixed set of values**: `step`, `status`, `customer_tier`. Rough guide: well under 100 values per label, and the product of all labels under ~1000 per metric.
2. **High-cardinality identifiers go to traces and logs**, which are built for them: `order.id` is a span attribute and a field in every log line.
3. **Never use raw user input as a label.** `tierLabel` maps anything unknown to `"unknown"`. A request with `customer_tier="gold-vip-123"` does not create a new series (`TestMetricsInteractor_BoundedLabels`).
4. **Exemplars** give the drill-down from a spike to specific requests without putting IDs into labels (next section).
5. If per-customer data is really needed (e.g. a top-N of slow customers), compute it from traces or logs, or in a separate analytics store, not in Prometheus.

## 3. How do you correlate metrics with traces?

1. **Exemplars.** When a histogram observation or counter increment happens inside a sampled span, it carries the `trace_id`:

   ```go
   eo.ObserveWithExemplar(d.Seconds(), prometheus.Labels{"trace_id": sc.TraceID().String()})
   ```

   Actual `/metrics` output:

   ```
   orders_created_total{customer_tier="premium",status="failure"} 1.0 # {trace_id="4bf92f3577b34da6a3ce929d0e0e4736"} 1.0 1.7905921568058665e+09
   ```

   Prometheus runs with `--enable-feature=exemplar-storage`. Grafana is provisioned with `exemplarTraceIdDestinations` → Jaeger (`deploy/grafana/provisioning/datasources/datasources.yml`). Clicking a point on a latency spike opens the exact slow trace.
2. **The same `trace_id` in logs** (`TraceHandler`). From a trace you get every log line of that request, and the reverse.
3. **Shared identity:** the same `service.name` on traces, and the same service/job labels on metrics. Dashboards and trace searches can then be filtered to the same service.

The usual investigation path: **metric** (something is wrong) → **exemplar** → **trace** (where and why) → **logs** (details) — without ever putting an ID into a metric label.
