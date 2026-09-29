-- Charge outcome unknown (provider timeout, network error): neither paid nor failed until reconciled.
ALTER TABLE orders DROP CONSTRAINT orders_status_check;
ALTER TABLE orders ADD CONSTRAINT orders_status_check
    CHECK (status IN ('pending', 'payment_pending', 'paid', 'completed', 'failed'));
