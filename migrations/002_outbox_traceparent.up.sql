-- Trace context at the time the event was written: the outbox relay continues the same trace.
ALTER TABLE outbox ADD COLUMN traceparent TEXT;
