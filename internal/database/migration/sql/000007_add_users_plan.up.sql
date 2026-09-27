BEGIN;

ALTER TABLE users
    ADD COLUMN plan VARCHAR(20) NOT NULL DEFAULT 'spark',
    ADD CONSTRAINT chk_users_plan
        CHECK (plan IN ('spark', 'glow', 'beam', 'plasma'));

COMMIT;
