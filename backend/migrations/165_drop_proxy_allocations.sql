-- Drop proxy_allocations. The provider-connect exclusive-allocation model is
-- replaced by a per-proxy capacity counter (proxies.max_bindings) enforced
-- against accounts.proxy_id (migration 164). Binding relationships live on
-- accounts.proxy_id (what the gateway actually reads); proxy_allocations was
-- only an exclusivity ledger and is no longer referenced by any code path.
--
-- Forward-only and destructive. The table carried no data the runtime still
-- needs; if a historical allocation audit is desired, export the table before
-- applying this migration (pg_dump -t proxy_allocations).
DROP TABLE IF EXISTS proxy_allocations;
