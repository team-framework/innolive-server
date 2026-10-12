BEGIN;

CREATE TABLE billing_subscriptions (
    id UUID PRIMARY KEY,
    user_id UUID NOT NULL
        REFERENCES users (id)
        ON UPDATE CASCADE
        ON DELETE CASCADE,
    channel VARCHAR(20) NOT NULL
        CONSTRAINT chk_billing_subscriptions_channel CHECK (channel IN ('paddle', 'app_store', 'onestore')),
    external_id VARCHAR(255) NOT NULL,
    plan VARCHAR(20) NOT NULL
        CONSTRAINT chk_billing_subscriptions_plan CHECK (plan IN ('glow', 'beam', 'plasma')),
    status VARCHAR(20) NOT NULL
        CONSTRAINT chk_billing_subscriptions_status CHECK (status IN ('active', 'canceled', 'expired')),
    current_period_end TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE UNIQUE INDEX uidx_billing_subscriptions_external
    ON billing_subscriptions (channel, external_id);
CREATE INDEX idx_billing_subscriptions_user_id
    ON billing_subscriptions (user_id);
CREATE INDEX idx_billing_subscriptions_current_period_end
    ON billing_subscriptions (current_period_end);

CREATE TABLE billing_events (
    id UUID PRIMARY KEY,
    channel VARCHAR(20) NOT NULL,
    external_event_id VARCHAR(255) NOT NULL,
    user_id UUID
        REFERENCES users (id)
        ON UPDATE CASCADE
        ON DELETE SET NULL,
    type VARCHAR(64) NOT NULL,
    subscription_external_id VARCHAR(255) NOT NULL,
    received_at TIMESTAMPTZ NOT NULL
);

CREATE UNIQUE INDEX uidx_billing_events_external
    ON billing_events (channel, external_event_id);
CREATE INDEX idx_billing_events_user_id
    ON billing_events (user_id);

COMMIT;
