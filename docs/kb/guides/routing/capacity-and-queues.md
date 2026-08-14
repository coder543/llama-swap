---
title: Routing capacity and request queues
summary: Configure memory capacity, eviction policy, concurrency limits, and queued work while a model is loading or busy.
category: guides
tags: [routing, queue, capacity, concurrency, concurrency-limit, max-concurrent-requests, global-concurrency-limit, rate-limit]
config_keys: [routing, matrix.capacity, matrix.strategy, models.*.memory, models.*.evictCost, models.*.concurrencyLimit, globalConcurrencyLimit]
updated: 2026-10-16
---

# Routing capacity and request queues

## Memory capacity and eviction

Use matrix capacity mode to fit models into a shared memory budget. Choose
consistent integer units, such as GB, and set a positive `memory` on every
model. These values are planning estimates, not measured GPU usage.

```yaml
models:
  small:
    cmd: llama-server --model small.gguf --port ${PORT}
    memory: 12
  large:
    cmd: llama-server --model large.gguf --port ${PORT}
    memory: 24
    evictCost: 50
routing:
  router:
    use: matrix
    settings:
      matrix:
        capacity: 32
        strategy: cost
```

`cost` minimizes the summed eviction cost; an omitted `evictCost` defaults
to the model's memory. Set it higher for models that are expensive to reload.
`lru` instead evicts the least recently used idle models until enough space
is free. Active requests are allowed to finish before their models are evicted.

A model larger than capacity runs alone. Capacity mode cannot be mixed with
`sets`, `vars`, or `evict_costs`. Omit capacity to retain upstream's
set-expression mode, including `+auto:orphans`.

There is no `models.*.maxConcurrentRequests` setting. The real per-model key
is `models.*.concurrencyLimit`, which limits active parallel requests.

The router serializes model swaps and queues requests until the selected model
is ready. Avoid using many simultaneous client retries as a capacity control:
they create more queued work. Choose a routing policy that fits the models that
may coexist, then set client timeouts high enough for the queue and load time.

Inspect the Activity view and logs when latency grows. A queue that never drains
usually means the command, proxy, or health check is wrong; see
`guides/model-runtime/troubleshooting-model-wont-load`.

## Global concurrency limit

`globalConcurrencyLimit` is a top-level setting, separate from
`models.*.concurrencyLimit`. It caps the number of inference requests served
at once across every model combined, using a single shared semaphore:

```yaml
globalConcurrencyLimit: 8
```

The default is `0`, meaning no limit — the semaphore is not even added to the
request chain, so a default config pays no cost for the feature. Once the
limit is reached, further requests are rejected immediately with an HTTP 429
response rather than queued; there is no wait for a slot to free up. Clients
should treat a 429 as a signal to back off and retry, the same way they handle
a per-model concurrency limit rejection.

Use this to protect shared hardware (CPU, disk, network) from being
overwhelmed by traffic spread across many different models, which a per-model
`concurrencyLimit` cannot do since it only counts requests to one model at a
time.
