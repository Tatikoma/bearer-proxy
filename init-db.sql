create table if not exists tokens (
    token text primary key,
    is_active boolean not null default true,
    request_count bigint not null default 0,
    created_at timestamptz not null default now()
);
