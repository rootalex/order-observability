-- Контекст трейса на момент записи события: воркер outbox продолжит тот же трейс.
ALTER TABLE outbox ADD COLUMN traceparent TEXT;
