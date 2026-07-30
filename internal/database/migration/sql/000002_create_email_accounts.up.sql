BEGIN;

CREATE TABLE email_accounts (
    user_id       UUID PRIMARY KEY,
    email         VARCHAR(320) NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL,

    CONSTRAINT fk_email_accounts_user
        FOREIGN KEY (user_id)
        REFERENCES users (id)
        ON UPDATE CASCADE
        ON DELETE CASCADE
);

COMMIT;
