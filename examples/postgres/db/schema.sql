CREATE TABLE users (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email       text NOT NULL,
    status      text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    tenant_id   bigint NOT NULL,
    org_id      bigint,
    nickname    text
);

CREATE TABLE organization_users (
    user_id         bigint NOT NULL,
    organization_id bigint NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, organization_id)
);

CREATE TABLE audit_logs (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id  bigint NOT NULL,
    actor_id   bigint,
    action     text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
